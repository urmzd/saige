package pgledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/durable/adapters/postgres/internal/pgident"
	"github.com/urmzd/saige/durable/ledger"
	"github.com/urmzd/saige/durable/run"
)

// Compile-time contract checks: Ledger is a ledger.Ledger and offers the
// optional ledger.RunGetter point read. It also offers ledger.PrefixReader,
// checked in the tests only, so this package still builds against a pinned
// root release that predates that interface.
var (
	_ ledger.Ledger    = (*Ledger)(nil)
	_ ledger.RunGetter = (*Ledger)(nil)
)

// Envelope write mode for Complete's in-place JSON merge. jsonb columns take
// the merge directly; text columns need the result cast back to text. The
// mode is learned from Validate, or lazily from the first Complete.
const (
	envModeUnknown int32 = iota
	envModeJSONB
	envModeText
)

// Ledger implements ledger.Ledger on the caller's own PostgreSQL tables. Use
// New to construct one and Validate to check the live schema.
type Ledger struct {
	pool *pgxpool.Pool
	m    Mapping

	// Quoted SQL fragments, derived once from the validated mapping.
	qRunsTable, qRunsID, qRunsStatus, qRunsEnv, qRunsCreated, qRunsScope string
	qRecsTable, qRecsID, qRecsKey, qRecsEnv, qRecsScope                  string

	// Raw mapped column names, to refuse Defaults collisions.
	runsMapped map[string]bool
	recsMapped map[string]bool

	// Statically-shaped statements, built once.
	sqlCompleteJSONB string
	sqlCompleteText  string
	sqlStatus        string
	sqlLoadRun       string
	sqlLoadRecs      string
	sqlRunExists     string
	sqlRecordGet     string
	sqlRecsPrefix    string
	sqlListPending   string

	envMode atomic.Int32
}

// New builds a Ledger over pool and m. It validates that every required
// mapping field is present and every identifier is safe to interpolate; it
// performs no I/O. Call Validate to check the mapping against the live
// database.
func New(pool *pgxpool.Pool, m Mapping) (*Ledger, error) {
	if pool == nil {
		return nil, errors.New("pgledger: pool is required")
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("pgledger: invalid mapping: %w", err)
	}

	l := &Ledger{pool: pool, m: m}

	l.qRunsTable = pgident.QuoteTable(m.Runs.Table)
	l.qRunsID = pgident.QuoteColumn(m.Runs.RunID)
	l.qRunsStatus = pgident.QuoteColumn(m.Runs.Status)
	l.qRunsEnv = pgident.QuoteColumn(m.Runs.Envelope)
	l.runsMapped = map[string]bool{m.Runs.RunID: true, m.Runs.Status: true, m.Runs.Envelope: true}
	if m.Runs.CreatedAt != "" {
		l.qRunsCreated = pgident.QuoteColumn(m.Runs.CreatedAt)
		l.runsMapped[m.Runs.CreatedAt] = true
	}
	if m.Runs.Scope.Column != "" {
		l.qRunsScope = pgident.QuoteColumn(m.Runs.Scope.Column)
		l.runsMapped[m.Runs.Scope.Column] = true
	}

	l.qRecsTable = pgident.QuoteTable(m.Records.Table)
	l.qRecsID = pgident.QuoteColumn(m.Records.RunID)
	l.qRecsKey = pgident.QuoteColumn(m.Records.Key)
	l.qRecsEnv = pgident.QuoteColumn(m.Records.Envelope)
	l.recsMapped = map[string]bool{m.Records.RunID: true, m.Records.Key: true, m.Records.Envelope: true}
	if m.Records.Scope.Column != "" {
		l.qRecsScope = pgident.QuoteColumn(m.Records.Scope.Column)
		l.recsMapped[m.Records.Scope.Column] = true
	}

	l.buildSQL()
	return l, nil
}

// buildSQL precomputes every statically-shaped statement. Only Accept and
// Record build SQL per call, because Defaults can add columns.
func (l *Ledger) buildSQL() {
	// Predicate suffixes: run-scoped statements take the run ID as $1 and
	// the scope value, when configured, as the last parameter.
	runsScope := func(n int) string {
		if l.qRunsScope == "" {
			return ""
		}
		return fmt.Sprintf(" AND %s = $%d", l.qRunsScope, n)
	}
	recsScope := func(n int) string {
		if l.qRecsScope == "" {
			return ""
		}
		return fmt.Sprintf(" AND %s = $%d", l.qRecsScope, n)
	}

	// Complete: pending → terminal CAS plus an envelope patch merging the
	// result fields into the stored JSON.
	merge := fmt.Sprintf("(coalesce(%s::jsonb, '{}'::jsonb) || $3::jsonb)", l.qRunsEnv)
	where := fmt.Sprintf("%s = $1 AND %s = $4%s", l.qRunsID, l.qRunsStatus, runsScope(5))
	l.sqlCompleteJSONB = fmt.Sprintf("UPDATE %s SET %s = $2, %s = %s WHERE %s",
		l.qRunsTable, l.qRunsStatus, l.qRunsEnv, merge, where)
	l.sqlCompleteText = fmt.Sprintf("UPDATE %s SET %s = $2, %s = %s::text WHERE %s",
		l.qRunsTable, l.qRunsStatus, l.qRunsEnv, merge, where)

	l.sqlStatus = fmt.Sprintf("SELECT %s FROM %s WHERE %s = $1%s",
		l.qRunsStatus, l.qRunsTable, l.qRunsID, runsScope(2))

	loadCols := fmt.Sprintf("%s::jsonb, %s", l.qRunsEnv, l.qRunsStatus)
	if l.qRunsCreated != "" {
		loadCols += ", " + l.qRunsCreated
	}
	l.sqlLoadRun = fmt.Sprintf("SELECT %s FROM %s WHERE %s = $1%s",
		loadCols, l.qRunsTable, l.qRunsID, runsScope(2))

	l.sqlLoadRecs = fmt.Sprintf("SELECT %s, %s::jsonb FROM %s WHERE %s = $1%s",
		l.qRecsKey, l.qRecsEnv, l.qRecsTable, l.qRecsID, recsScope(2))

	l.sqlRunExists = fmt.Sprintf("SELECT 1 FROM %s WHERE %s = $1%s",
		l.qRunsTable, l.qRunsID, runsScope(2))

	l.sqlRecordGet = fmt.Sprintf("SELECT %s::jsonb FROM %s WHERE %s = $1 AND %s = $2%s",
		l.qRecsEnv, l.qRecsTable, l.qRecsID, l.qRecsKey, recsScope(3))

	// Prefix read: the run's existence and its matching records in one
	// statement. The LEFT JOIN yields no row for an unknown run, one
	// all-NULL row for a known run with no match, else the matches. ^@ is
	// starts_with: prefixes are literal, with no LIKE wildcards to escape.
	// Takes $1 run ID, $2 prefixes, then the runs and records scope values
	// for whichever scopes are configured.
	next := 3
	prefixRunsScope := ""
	if l.qRunsScope != "" {
		prefixRunsScope = fmt.Sprintf(" AND %s = $%d", l.qRunsScope, next)
		next++
	}
	prefixRecsScope := ""
	if l.qRecsScope != "" {
		prefixRecsScope = fmt.Sprintf(" AND rec.%s = $%d", l.qRecsScope, next)
	}
	l.sqlRecsPrefix = fmt.Sprintf(`SELECT rec.%s, rec.%s::jsonb
FROM (SELECT 1 FROM %s WHERE %s = $1%s) AS r
LEFT JOIN %s AS rec ON rec.%s = $1 AND rec.%s ^@ ANY($2::text[])%s`,
		l.qRecsKey, l.qRecsEnv,
		l.qRunsTable, l.qRunsID, prefixRunsScope,
		l.qRecsTable, l.qRecsID, l.qRecsKey, prefixRecsScope)

	// ListPending pages in ascending (created, id) order, strictly after
	// the cursor. The row comparison uses the exact same created
	// expression as the select list, the before-filter, and the ORDER BY —
	// with the envelope fallback, a different expression on either side
	// would paginate one ordering while returning another.
	created := l.createdExpr()
	l.sqlListPending = fmt.Sprintf(
		"SELECT %s, %s FROM %s WHERE %s = $1 AND %s < $2 AND (%s, %s) > ($3, $4)%s ORDER BY %s ASC, %s ASC LIMIT $5",
		l.qRunsID, created, l.qRunsTable, l.qRunsStatus, created, created, l.qRunsID, runsScope(6), created, l.qRunsID)
}

// createdExpr is the SQL expression for a run's creation time: the dedicated
// column when mapped, else the envelope's CreatedAt field cast through
// timestamptz so comparison and ordering are temporal, not textual.
func (l *Ledger) createdExpr() string {
	if l.qRunsCreated != "" {
		return l.qRunsCreated
	}
	return fmt.Sprintf("(%s::jsonb ->> 'CreatedAt')::timestamptz", l.qRunsEnv)
}

// Accept durably registers a run. Idempotent per the ledger contract: an
// existing run ID is left untouched and Accept returns nil.
func (l *Ledger) Accept(ctx context.Context, r run.Run) error {
	return l.accept(ctx, l.pool, r)
}

// AcceptTx registers a run as Accept does, in tx, the caller's transaction,
// so the run commits together with whatever else tx writes: the event that
// announces it, for an outbox. tx must be on this ledger's database.
func (l *Ledger) AcceptTx(ctx context.Context, tx pgx.Tx, r run.Run) error {
	if tx == nil {
		return fmt.Errorf("pgledger: accept %s: no transaction", r.ID)
	}
	return l.accept(ctx, tx, r)
}

// execer is what a write runs on: the pool, or a caller's transaction.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func (l *Ledger) accept(ctx context.Context, db execer, r run.Run) error {
	env, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("pgledger: accept %s: marshal envelope: %w", r.ID, err)
	}

	cols := []string{l.qRunsID, l.qRunsStatus, l.qRunsEnv}
	args := []any{r.ID, string(r.Status), env}
	if l.qRunsCreated != "" {
		cols = append(cols, l.qRunsCreated)
		args = append(args, r.CreatedAt)
	}
	if l.qRunsScope != "" {
		cols = append(cols, l.qRunsScope)
		args = append(args, l.m.Runs.Scope.Value)
	}
	if l.m.Runs.Defaults != nil {
		cols, args, err = appendDefaults(cols, args, l.runsMapped, l.m.Runs.Defaults(r))
		if err != nil {
			return fmt.Errorf("pgledger: accept %s: %w", r.ID, err)
		}
	}

	q := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (%s) DO NOTHING",
		l.qRunsTable, strings.Join(cols, ", "), placeholders(len(args)), l.qRunsID)
	if _, err := db.Exec(ctx, q, args...); err != nil {
		return fmt.Errorf("pgledger: accept %s: %w", r.ID, err)
	}
	return nil
}

// Complete writes the terminal result: a CAS from pending to the terminal
// status, first write wins. The result fields are merged into the envelope
// so Load reconstructs the full struct.
func (l *Ledger) Complete(ctx context.Context, runID string, attempt int, res run.Result) error {
	// The patch mirrors run.Run's JSON field names so the merged envelope
	// still decodes as a run.Run.
	patch, err := json.Marshal(struct {
		Status      run.RunStatus
		Output      []byte
		Error       string
		Attempt     int
		CompletedAt time.Time
	}{res.Status, res.Output, res.Error, attempt, time.Now().UTC()})
	if err != nil {
		return fmt.Errorf("pgledger: complete %s: marshal patch: %w", runID, err)
	}

	args := []any{runID, string(res.Status), patch, string(run.StatusPending)}
	statusArgs := []any{runID}
	if l.qRunsScope != "" {
		args = append(args, l.m.Runs.Scope.Value)
		statusArgs = append(statusArgs, l.m.Runs.Scope.Value)
	}

	// The CAS races other completers: a 0-row update is classified by a
	// follow-up status read. If the run is somehow pending again by then
	// (another writer landed between the two statements), retry the CAS.
	for range 3 {
		tag, err := l.execComplete(ctx, args)
		if err != nil {
			return fmt.Errorf("pgledger: complete %s: %w", runID, err)
		}
		if tag.RowsAffected() > 0 {
			return nil
		}

		var status string
		err = l.pool.QueryRow(ctx, l.sqlStatus, statusArgs...).Scan(&status)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("pgledger: complete %s: %w", runID, run.ErrNotFound)
		case err != nil:
			return fmt.Errorf("pgledger: complete %s: read status: %w", runID, err)
		}
		if run.RunStatus(status).Terminal() {
			return fmt.Errorf("pgledger: complete %s: status %q: %w", runID, status, run.ErrAlreadyTerminal)
		}
	}
	return fmt.Errorf("pgledger: complete %s: CAS did not settle after retries", runID)
}

// execComplete runs the CAS with the envelope mode the column requires,
// learning the mode from the first datatype-mismatch error when Validate has
// not been called.
func (l *Ledger) execComplete(ctx context.Context, args []any) (pgconn.CommandTag, error) {
	mode := l.envMode.Load()
	sqlText := l.sqlCompleteJSONB
	if mode == envModeText {
		sqlText = l.sqlCompleteText
	}
	tag, err := l.pool.Exec(ctx, sqlText, args...)
	if err != nil && mode == envModeUnknown && isDatatypeMismatch(err) {
		l.envMode.Store(envModeText)
		return l.pool.Exec(ctx, l.sqlCompleteText, args...)
	}
	if err == nil && mode == envModeUnknown {
		l.envMode.Store(envModeJSONB)
	}
	return tag, err
}

// isDatatypeMismatch reports SQLSTATE 42804 (datatype mismatch): the jsonb
// merge result being assigned to a text envelope column.
func isDatatypeMismatch(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42804"
}

// getRun fetches one run row and reconstructs the run.Run from its envelope
// with the mapped columns overlaid. Errors carry no caller prefix; a missing
// run is run.ErrNotFound.
func (l *Ledger) getRun(ctx context.Context, runID string) (run.Run, error) {
	args := []any{runID}
	if l.qRunsScope != "" {
		args = append(args, l.m.Runs.Scope.Value)
	}

	var env []byte
	var status string
	var created *time.Time
	dest := []any{&env, &status}
	if l.qRunsCreated != "" {
		dest = append(dest, &created)
	}
	err := l.pool.QueryRow(ctx, l.sqlLoadRun, args...).Scan(dest...)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return run.Run{}, run.ErrNotFound
	case err != nil:
		return run.Run{}, err
	}

	return decodeRun(env, runID, status, created)
}

// decodeRun rebuilds a run.Run from its envelope, overlaying the mapped
// columns, which take precedence over the envelope's copies.
func decodeRun(env []byte, runID, status string, created *time.Time) (run.Run, error) {
	var r run.Run
	if err := json.Unmarshal(env, &r); err != nil {
		return run.Run{}, fmt.Errorf("decode envelope: %w", err)
	}
	r.ID = runID
	r.Status = run.RunStatus(status)
	if created != nil {
		r.CreatedAt = *created
	}
	return r, nil
}

// GetRun is the optional ledger.RunGetter capability: a point read of one
// run without its records — the right cost for result polling and status
// checks, where Load's full record prefetch is waste.
func (l *Ledger) GetRun(ctx context.Context, runID string) (run.Run, error) {
	r, err := l.getRun(ctx, runID)
	if err != nil {
		return run.Run{}, fmt.Errorf("pgledger: get run %s: %w", runID, err)
	}
	return r, nil
}

// ListRuns returns up to limit runs, newest first: descending creation time,
// then descending run ID. An empty status lists runs of every status;
// otherwise only runs whose mapped status column equals it. It is an
// inspection read for operators and tools, not part of ledger.Ledger: the
// listed runs carry no Input or Output (the payloads stay in the database,
// so a listing never pays for them); GetRun or Load reads one run in full.
func (l *Ledger) ListRuns(ctx context.Context, status run.RunStatus, limit int) ([]run.Run, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("pgledger: list runs: limit %d must be positive", limit)
	}
	created := l.createdExpr()
	cols := fmt.Sprintf("%s, %s::jsonb - 'Input' - 'Output', %s", l.qRunsID, l.qRunsEnv, l.qRunsStatus)
	if l.qRunsCreated != "" {
		cols += ", " + l.qRunsCreated
	}
	var conds []string
	args := []any{limit}
	if status != "" {
		args = append(args, string(status))
		conds = append(conds, fmt.Sprintf("%s = $%d", l.qRunsStatus, len(args)))
	}
	if l.qRunsScope != "" {
		args = append(args, l.m.Runs.Scope.Value)
		conds = append(conds, fmt.Sprintf("%s = $%d", l.qRunsScope, len(args)))
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	q := fmt.Sprintf("SELECT %s FROM %s%s ORDER BY %s DESC NULLS LAST, %s DESC LIMIT $1",
		cols, l.qRunsTable, where, created, l.qRunsID)

	rows, err := l.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("pgledger: list runs: %w", err)
	}
	defer rows.Close()

	var runs []run.Run
	for rows.Next() {
		var id, st string
		var env []byte
		var createdAt *time.Time
		dest := []any{&id, &env, &st}
		if l.qRunsCreated != "" {
			dest = append(dest, &createdAt)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("pgledger: list runs: %w", err)
		}
		r, err := decodeRun(env, id, st, createdAt)
		if err != nil {
			return nil, fmt.Errorf("pgledger: list runs: %s: %w", id, err)
		}
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgledger: list runs: %w", err)
	}
	return runs, nil
}

// RecordsWithPrefix is the optional ledger.PrefixReader capability: the
// run's records whose key starts with one of prefixes, and a run.ErrNotFound
// for an unknown run, in one round trip. The (run ID, key) unique index
// narrows the scan to the run, and only matching envelopes are decoded and
// sent, so a poll for one small record never pays for a large one.
func (l *Ledger) RecordsWithPrefix(ctx context.Context, runID string, prefixes ...string) ([]run.Record, error) {
	if prefixes == nil {
		prefixes = []string{}
	}
	args := []any{runID, prefixes}
	if l.qRunsScope != "" {
		args = append(args, l.m.Runs.Scope.Value)
	}
	if l.qRecsScope != "" {
		args = append(args, l.m.Records.Scope.Value)
	}
	rows, err := l.pool.Query(ctx, l.sqlRecsPrefix, args...)
	if err != nil {
		return nil, fmt.Errorf("pgledger: records of %s: %w", runID, err)
	}
	defer rows.Close()

	found := false
	var recs []run.Record
	for rows.Next() {
		found = true
		var key *string
		var env []byte
		if err := rows.Scan(&key, &env); err != nil {
			return nil, fmt.Errorf("pgledger: records of %s: %w", runID, err)
		}
		if key == nil {
			continue // the run exists and nothing matched
		}
		rec, err := decodeRecord(env, runID, *key)
		if err != nil {
			return nil, fmt.Errorf("pgledger: records of %s: %w", runID, err)
		}
		recs = append(recs, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgledger: records of %s: %w", runID, err)
	}
	if !found {
		return nil, fmt.Errorf("pgledger: records of %s: %w", runID, run.ErrNotFound)
	}
	return recs, nil
}

// Load returns the run and all of its records, reconstructed from the
// envelope with mapped columns overlaid.
func (l *Ledger) Load(ctx context.Context, runID string) (run.Run, []run.Record, error) {
	r, err := l.getRun(ctx, runID)
	if err != nil {
		return run.Run{}, nil, fmt.Errorf("pgledger: load %s: %w", runID, err)
	}

	recArgs := []any{runID}
	if l.qRecsScope != "" {
		recArgs = append(recArgs, l.m.Records.Scope.Value)
	}
	rows, err := l.pool.Query(ctx, l.sqlLoadRecs, recArgs...)
	if err != nil {
		return run.Run{}, nil, fmt.Errorf("pgledger: load %s: records: %w", runID, err)
	}
	defer rows.Close()

	var recs []run.Record
	for rows.Next() {
		var key string
		var recEnv []byte
		if err := rows.Scan(&key, &recEnv); err != nil {
			return run.Run{}, nil, fmt.Errorf("pgledger: load %s: records: %w", runID, err)
		}
		rec, err := decodeRecord(recEnv, runID, key)
		if err != nil {
			return run.Run{}, nil, fmt.Errorf("pgledger: load %s: %w", runID, err)
		}
		recs = append(recs, rec)
	}
	if err := rows.Err(); err != nil {
		return run.Run{}, nil, fmt.Errorf("pgledger: load %s: records: %w", runID, err)
	}
	return r, recs, nil
}

// Record persists one memoized result, write-once. On a (RunID, Key)
// conflict it returns the already-stored record with run.ErrAlreadyRecorded
// so concurrent attempts converge on one history.
func (l *Ledger) Record(ctx context.Context, rec run.Record) (run.Record, error) {
	existArgs := []any{rec.RunID}
	if l.qRunsScope != "" {
		existArgs = append(existArgs, l.m.Runs.Scope.Value)
	}
	var one int
	err := l.pool.QueryRow(ctx, l.sqlRunExists, existArgs...).Scan(&one)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return run.Record{}, fmt.Errorf("pgledger: record %s/%s: %w", rec.RunID, rec.Key, run.ErrNotFound)
	case err != nil:
		return run.Record{}, fmt.Errorf("pgledger: record %s/%s: %w", rec.RunID, rec.Key, err)
	}

	if rec.RecordedAt.IsZero() {
		rec.RecordedAt = time.Now().UTC()
	}
	env, err := json.Marshal(rec)
	if err != nil {
		return run.Record{}, fmt.Errorf("pgledger: record %s/%s: marshal envelope: %w", rec.RunID, rec.Key, err)
	}

	cols := []string{l.qRecsID, l.qRecsKey, l.qRecsEnv}
	args := []any{rec.RunID, rec.Key, env}
	if l.qRecsScope != "" {
		cols = append(cols, l.qRecsScope)
		args = append(args, l.m.Records.Scope.Value)
	}
	if l.m.Records.Defaults != nil {
		cols, args, err = appendDefaults(cols, args, l.recsMapped, l.m.Records.Defaults(rec))
		if err != nil {
			return run.Record{}, fmt.Errorf("pgledger: record %s/%s: %w", rec.RunID, rec.Key, err)
		}
	}

	q := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (%s, %s) DO NOTHING",
		l.qRecsTable, strings.Join(cols, ", "), placeholders(len(args)), l.qRecsID, l.qRecsKey)
	tag, err := l.pool.Exec(ctx, q, args...)
	if err != nil {
		return run.Record{}, fmt.Errorf("pgledger: record %s/%s: %w", rec.RunID, rec.Key, err)
	}
	if tag.RowsAffected() > 0 {
		// Return a decode of what was stored: guaranteed detached from
		// every caller buffer.
		stored, err := decodeRecord(env, rec.RunID, rec.Key)
		if err != nil {
			return run.Record{}, fmt.Errorf("pgledger: record %s/%s: %w", rec.RunID, rec.Key, err)
		}
		return stored, nil
	}

	// Conflict: first write won earlier; adopt it.
	getArgs := []any{rec.RunID, rec.Key}
	if l.qRecsScope != "" {
		getArgs = append(getArgs, l.m.Records.Scope.Value)
	}
	var storedEnv []byte
	err = l.pool.QueryRow(ctx, l.sqlRecordGet, getArgs...).Scan(&storedEnv)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return run.Record{}, fmt.Errorf("pgledger: record %s/%s: conflicted but stored row not readable", rec.RunID, rec.Key)
	case err != nil:
		return run.Record{}, fmt.Errorf("pgledger: record %s/%s: read stored: %w", rec.RunID, rec.Key, err)
	}
	stored, err := decodeRecord(storedEnv, rec.RunID, rec.Key)
	if err != nil {
		return run.Record{}, fmt.Errorf("pgledger: record %s/%s: %w", rec.RunID, rec.Key, err)
	}
	return stored, fmt.Errorf("pgledger: record %s/%s: %w", rec.RunID, rec.Key, run.ErrAlreadyRecorded)
}

// ListPending returns pending runs created strictly before t, in ascending
// (CreatedAt, ID) order strictly after cursor, up to limit. The zero cursor
// needs no special-casing: (0001-01-01 UTC, "") sorts strictly before every
// real row — run IDs are non-empty — so one statement serves the first page
// and every subsequent one.
func (l *Ledger) ListPending(ctx context.Context, before time.Time, cursor run.PendingCursor, limit int) ([]run.PendingRun, error) {
	args := []any{string(run.StatusPending), before, cursor.CreatedAt, cursor.ID, limit}
	if l.qRunsScope != "" {
		args = append(args, l.m.Runs.Scope.Value)
	}
	rows, err := l.pool.Query(ctx, l.sqlListPending, args...)
	if err != nil {
		return nil, fmt.Errorf("pgledger: list pending: %w", err)
	}
	defer rows.Close()

	var batch []run.PendingRun
	for rows.Next() {
		var p run.PendingRun
		if err := rows.Scan(&p.ID, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("pgledger: list pending: %w", err)
		}
		batch = append(batch, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgledger: list pending: %w", err)
	}
	return batch, nil
}

// decodeRecord rebuilds a run.Record from its envelope, overlaying the
// mapped identity columns.
func decodeRecord(env []byte, runID, key string) (run.Record, error) {
	var rec run.Record
	if err := json.Unmarshal(env, &rec); err != nil {
		return run.Record{}, fmt.Errorf("decode record envelope: %w", err)
	}
	rec.RunID = runID
	rec.Key = key
	return rec, nil
}

// appendDefaults folds a Defaults map into an insert's column and argument
// lists: keys sorted for stable SQL, validated as identifiers, and refused
// when they collide with a mapped column.
func appendDefaults(cols []string, args []any, mapped map[string]bool, defaults map[string]any) ([]string, []any, error) {
	for _, k := range slices.Sorted(maps.Keys(defaults)) {
		if err := pgident.Column("Defaults key", k); err != nil {
			return nil, nil, err
		}
		if mapped[k] {
			return nil, nil, fmt.Errorf("defaults key %q collides with a mapped column", k)
		}
		cols = append(cols, pgident.QuoteColumn(k))
		args = append(args, defaults[k])
	}
	return cols, args, nil
}

// placeholders returns "$1, $2, ..., $n".
func placeholders(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		if i > 1 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "$%d", i)
	}
	return b.String()
}
