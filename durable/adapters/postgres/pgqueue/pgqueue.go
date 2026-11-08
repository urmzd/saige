// Package pgqueue implements durable's queue.Queue on a PostgreSQL table
// the application already owns.
//
// Like pgledger, this adapter requires no additional database and no table
// of its own: it maps onto an existing table via a declarative Mapping,
// Validate introspects and reports exactly what is missing, and
// RecommendedDDL returns suggested CREATE statements as a string. Nothing
// here ever executes DDL.
//
// Semantics: a row is claimable when it is ready (ready_at set and due) or
// its lease has lapsed. Claim polls with FOR UPDATE SKIP LOCKED, increments
// the attempt counter — and, when the claim reclaims an expired lease, the
// failures counter, because the previous execution died holding it — and
// takes a lease; Heartbeat, Release, and Settle are fenced by
// (RunID, Attempt) and answer a stale attempt with run.ErrSuperseded.
// A claim statement already sent is not abandoned when Claim's context is
// cancelled: Postgres may commit it anyway, so Claim waits up to 5s past
// the cancellation for the answer and returns any lease it took.
// Release carries a failed flag: a failed release consumes retry budget
// (failures increments), an interrupted one does not. Rows are never
// deleted — Settle clears ready_at and lease_until but keeps the row (and
// both counters) so the attempt lineage survives: a re-enqueued parked run
// claims at attempt N+1, never 1, and waiting never consumes retry budget.
// Retention of settled rows is the caller's policy, exercised with their own
// tooling.
//
// Streaming: a Queue from New has no delta log, so durable's Emit is a no-op
// and Client.Watch errors. NewDeltaQueue adds the queue.DeltaLog capability
// on a second, append-only table (DeltaMapping; RecommendedDeltaDDL
// suggests one): appends are fenced against the queue's attempt counter,
// and watchers are woken by LISTEN/NOTIFY through a pgkit.Listener, with a
// poll fallback, so a Postgres-only deployment streams without Redis.
// Deltas are advisory flow: losing them loses observability history, never
// correctness.
package pgqueue

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/durable/adapters/postgres/internal/pgident"
	"github.com/urmzd/saige/durable/adapters/postgres/pgkit"
	"github.com/urmzd/saige/durable/queue"
	"github.com/urmzd/saige/durable/run"
)

const (
	// defaultPollInterval is how often a blocked Claim re-checks for
	// claimable work when not overridden by WithPollInterval.
	defaultPollInterval = 200 * time.Millisecond

	// claimAnswerWait bounds how long Claim keeps waiting, once its ctx is
	// done, for the answer to a claim statement it already sent. A healthy
	// database answers in milliseconds, so this only delays the stop of a
	// worker whose database has stopped answering.
	claimAnswerWait = 5 * time.Second
)

// Scope is an optional discriminator for queue tables shared with
// non-anchor rows: every row written carries Value in Column and every
// statement filters on it.
type Scope struct {
	Column string
	Value  string
}

// Mapping declares which existing table and columns carry the queue. All
// column fields are required.
type Mapping struct {
	// Table is the queue table, optionally schema-qualified.
	Table string
	// RunID is the column carrying the run ID; it must be covered by a
	// plain (non-partial, non-expression, non-deferrable) UNIQUE
	// constraint or unique index (Validate checks).
	RunID string
	// ReadyAt is a timestamptz column: when the run becomes claimable.
	// NULL while leased or settled.
	ReadyAt string
	// LeaseUntil is a timestamptz column: the current lease deadline.
	// NULL when not leased.
	LeaseUntil string
	// Attempt is an integer column: the monotonically increasing attempt
	// counter, the fencing token. Never reset.
	Attempt string
	// Failures is an integer column: the run's retry-budget consumption,
	// counting only executions that actually failed — a failed Release or
	// a lease that expired under its worker. Never reset; park/resume
	// (Settle then Enqueue) and interrupted releases leave it untouched.
	Failures string
	// Scope optionally discriminates rows in a shared table.
	Scope Scope
	// Defaults fills the caller's own NOT NULL columns when Enqueue
	// inserts a brand-new row.
	Defaults func(runID string) map[string]any
}

// DefaultMapping returns the mapping matching RecommendedDDL's suggested
// durable_queue table.
func DefaultMapping() Mapping {
	return Mapping{
		Table:      "durable_queue",
		RunID:      "run_id",
		ReadyAt:    "ready_at",
		LeaseUntil: "lease_until",
		Attempt:    "attempt",
		Failures:   "failures",
	}
}

// Option configures a Queue.
type Option func(*Queue) error

// WithPollInterval sets how often a blocked Claim polls for claimable work.
// The default is 200ms; tests use short intervals like 5ms.
func WithPollInterval(d time.Duration) Option {
	return func(q *Queue) error {
		if d <= 0 {
			return fmt.Errorf("pgqueue: poll interval must be positive, got %v", d)
		}
		q.poll = d
		return nil
	}
}

// WithListener hands a DeltaQueue the pgkit.Listener its watchers subscribe
// through, so a process that already has one (shared with other components)
// holds one LISTEN connection instead of one per user. Without it a
// DeltaQueue builds its own over the pool. A plain Queue ignores it.
func WithListener(l *pgkit.Listener) Option {
	return func(q *Queue) error {
		if l == nil {
			return errors.New("pgqueue: listener is nil")
		}
		q.listener = l
		return nil
	}
}

// Queue implements queue.Queue on the caller's own PostgreSQL table. Use New
// to construct one and Validate to check the live schema.
type Queue struct {
	pool     *pgxpool.Pool
	m        Mapping
	poll     time.Duration
	listener *pgkit.Listener

	// Quoted SQL fragments, derived once from the validated mapping.
	qTable, qID, qReady, qLease, qAttempt, qFailures, qScope string

	// Raw mapped column names, to refuse Defaults collisions.
	mapped map[string]bool

	sqlClaim     string
	sqlHeartbeat string
	sqlRelease   string
	sqlSettle    string
}

// New builds a Queue over pool and m. It validates that every mapping field
// is present and every identifier is safe to interpolate; it performs no
// I/O. Call Validate to check the mapping against the live database.
func New(pool *pgxpool.Pool, m Mapping, opts ...Option) (*Queue, error) {
	if pool == nil {
		return nil, errors.New("pgqueue: pool is required")
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("pgqueue: invalid mapping: %w", err)
	}

	q := &Queue{pool: pool, m: m, poll: defaultPollInterval}
	q.qTable = pgident.QuoteTable(m.Table)
	q.qID = pgident.QuoteColumn(m.RunID)
	q.qReady = pgident.QuoteColumn(m.ReadyAt)
	q.qLease = pgident.QuoteColumn(m.LeaseUntil)
	q.qAttempt = pgident.QuoteColumn(m.Attempt)
	q.qFailures = pgident.QuoteColumn(m.Failures)
	q.mapped = map[string]bool{m.RunID: true, m.ReadyAt: true, m.LeaseUntil: true, m.Attempt: true, m.Failures: true}
	if m.Scope.Column != "" {
		q.qScope = pgident.QuoteColumn(m.Scope.Column)
		q.mapped[m.Scope.Column] = true
	}

	q.buildSQL()
	for _, opt := range opts {
		if err := opt(q); err != nil {
			return nil, err
		}
	}
	return q, nil
}

func (m Mapping) validate() error {
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	add(pgident.Table("Mapping.Table", m.Table))
	add(pgident.Column("Mapping.RunID", m.RunID))
	add(pgident.Column("Mapping.ReadyAt", m.ReadyAt))
	add(pgident.Column("Mapping.LeaseUntil", m.LeaseUntil))
	add(pgident.Column("Mapping.Attempt", m.Attempt))
	add(pgident.Column("Mapping.Failures", m.Failures))
	switch {
	case m.Scope.Column == "" && m.Scope.Value == "":
	case m.Scope.Column == "":
		errs = append(errs, errors.New("Mapping.Scope.Column is required when Scope.Value is set"))
	case m.Scope.Value == "":
		errs = append(errs, errors.New("Mapping.Scope.Value is required when Scope.Column is set"))
	default:
		add(pgident.Column("Mapping.Scope.Column", m.Scope.Column))
	}
	return errors.Join(errs...)
}

func (q *Queue) buildSQL() {
	scope := func(n int) string {
		if q.qScope == "" {
			return ""
		}
		return fmt.Sprintf(" AND %s = $%d", q.qScope, n)
	}
	innerScope := ""
	if q.qScope != "" {
		innerScope = fmt.Sprintf(" AND c.%s = $2", q.qScope)
	}

	// Claimable: due-and-ready, or holding a lapsed lease (leased rows
	// have ready_at NULL, so expiry is its own arm of the predicate).
	claimable := fmt.Sprintf(
		"((c.%s IS NOT NULL AND c.%s <= now()) OR (c.%s IS NOT NULL AND c.%s < now()))",
		q.qReady, q.qReady, q.qLease, q.qLease)

	// expiredOne is 1 exactly when the row holds a lapsed lease: the
	// previous execution died, so resolving the lapse consumes retry
	// budget. In an UPDATE, every SET expression reads the pre-update
	// row, so this is computed from lease_until BEFORE the same statement
	// overwrites (or clears) it; and now() is fixed per statement, so the
	// claimable subquery and this CASE judge the lease against the same
	// clock and, via FOR UPDATE's lock, the same row version.
	expiredOne := fmt.Sprintf(
		"CASE WHEN t.%s IS NOT NULL AND t.%s < now() THEN 1 ELSE 0 END",
		q.qLease, q.qLease)

	q.sqlClaim = fmt.Sprintf(`UPDATE %s AS t
SET %s = now() + make_interval(secs => $1), %s = t.%s + 1, %s = t.%s + %s, %s = NULL
FROM (
	SELECT c.%s FROM %s AS c
	WHERE %s%s
	ORDER BY c.%s
	LIMIT 1
	FOR UPDATE SKIP LOCKED
) AS picked
WHERE t.%s = picked.%s
RETURNING t.%s, t.%s, t.%s`,
		q.qTable,
		q.qLease, q.qAttempt, q.qAttempt, q.qFailures, q.qFailures, expiredOne, q.qReady,
		q.qID, q.qTable,
		claimable, innerScope,
		q.qReady,
		q.qID, q.qID,
		q.qID, q.qAttempt, q.qFailures)

	q.sqlHeartbeat = fmt.Sprintf(
		"UPDATE %s SET %s = now() + make_interval(secs => $3) WHERE %s = $1 AND %s = $2 AND %s IS NOT NULL AND %s >= now()%s",
		q.qTable, q.qLease, q.qID, q.qAttempt, q.qLease, q.qLease, scope(4))

	q.sqlRelease = fmt.Sprintf(
		"UPDATE %s SET %s = NULL, %s = now() + make_interval(secs => $3), %s = %s + CASE WHEN $4::boolean THEN 1 ELSE 0 END WHERE %s = $1 AND %s = $2 AND %s IS NOT NULL%s",
		q.qTable, q.qLease, q.qReady, q.qFailures, q.qFailures, q.qID, q.qAttempt, q.qLease, scope(5))

	q.sqlSettle = fmt.Sprintf(
		"UPDATE %s SET %s = NULL, %s = NULL WHERE %s = $1 AND %s = $2%s",
		q.qTable, q.qLease, q.qReady, q.qID, q.qAttempt, scope(3))
}

// Enqueue makes runID claimable after delay. Idempotent while the run is
// queued or leased; a settled row (ready_at NULL, no live lease) is re-armed
// in place, keeping its attempt counter so the lineage survives.
func (q *Queue) Enqueue(ctx context.Context, runID string, delay time.Duration) error {
	cols := []string{q.qID, q.qReady, q.qLease, q.qAttempt, q.qFailures}
	vals := []string{"$1", "now() + make_interval(secs => $2)", "NULL", "0", "0"}
	args := []any{runID, delay.Seconds()}
	if q.qScope != "" {
		cols = append(cols, q.qScope)
		vals = append(vals, fmt.Sprintf("$%d", len(args)+1))
		args = append(args, q.m.Scope.Value)
	}
	if q.m.Defaults != nil {
		defaults := q.m.Defaults(runID)
		for _, k := range slices.Sorted(maps.Keys(defaults)) {
			if err := pgident.Column("Defaults key", k); err != nil {
				return fmt.Errorf("pgqueue: enqueue %s: %w", runID, err)
			}
			if q.mapped[k] {
				return fmt.Errorf("pgqueue: enqueue %s: Defaults key %q collides with a mapped column", runID, k)
			}
			cols = append(cols, pgident.QuoteColumn(k))
			vals = append(vals, fmt.Sprintf("$%d", len(args)+1))
			args = append(args, defaults[k])
		}
	}

	// A settled or lease-lapsed row is re-armed (clearing any stale
	// lease so the fresh delay is honored); a queued or live-leased row
	// is left untouched. Clearing a LAPSED lease also consumes retry
	// budget: the execution died holding it, and had Claim resolved the
	// lapse instead of this re-arm it would have charged the same +1 —
	// park/resume re-arms (lease already NULL) stay free.
	sqlText := fmt.Sprintf(`INSERT INTO %s AS t (%s) VALUES (%s)
ON CONFLICT (%s) DO UPDATE SET %s = EXCLUDED.%s, %s = NULL,
%s = t.%s + CASE WHEN t.%s IS NOT NULL AND t.%s < now() THEN 1 ELSE 0 END
WHERE t.%s IS NULL AND (t.%s IS NULL OR t.%s < now())`,
		q.qTable, strings.Join(cols, ", "), strings.Join(vals, ", "),
		q.qID, q.qReady, q.qReady, q.qLease,
		q.qFailures, q.qFailures, q.qLease, q.qLease,
		q.qReady, q.qLease, q.qLease)
	if _, err := q.pool.Exec(ctx, sqlText, args...); err != nil {
		return fmt.Errorf("pgqueue: enqueue %s: %w", runID, err)
	}
	return nil
}

// Claim blocks until a run is claimable or ctx is done, polling at the
// configured interval. The returned Item carries the incremented attempt and
// the run's accumulated failure count — reclaiming an expired lease
// increments both. The lease deadline returned is computed on the caller's
// clock; the authoritative lease lives in the database on the server's
// clock.
//
// Waiting between polls, and for a pool connection, ends as soon as ctx is
// done, and Claim on a done ctx asks nothing. A claim statement already
// sent is not abandoned, though: it runs on a context ctx's cancellation
// does not reach, and once ctx is done Claim gives it up to
// claimAnswerWait (5s) more to answer. pgx abandons a statement the moment
// its context is cancelled, yet Postgres can still carry it out and
// commit, which would lease a run to nobody. So a lease the statement took
// comes back as an Item, even after ctx is done, for the caller to hold or
// hand back. An error once the statement is sent leaves the outcome
// unknown, whether Postgres stayed silent past that bound or the
// connection broke after it committed: a lease the statement took lapses
// after ttl.
func (q *Queue) Claim(ctx context.Context, ttl time.Duration) (queue.Item, time.Time, error) {
	args := []any{ttl.Seconds()}
	if q.qScope != "" {
		args = append(args, q.m.Scope.Value)
	}
	for {
		if err := ctx.Err(); err != nil {
			return queue.Item{}, time.Time{}, err
		}
		deadline := time.Now().Add(ttl)
		it, ok, err := q.claimOnce(ctx, args)
		switch {
		case err != nil:
			return queue.Item{}, time.Time{}, err
		case ok:
			return it, deadline, nil
		}

		select {
		case <-ctx.Done():
			return queue.Item{}, time.Time{}, ctx.Err()
		case <-time.After(q.poll):
		}
	}
}

// claimOnce sends the claim statement once and reports whether it took a
// lease. The connection is acquired on ctx, since nothing has been asked of
// the database yet; the statement runs on answerContext, so ctx's
// cancellation cannot abandon it.
func (q *Queue) claimOnce(ctx context.Context, args []any) (queue.Item, bool, error) {
	conn, err := q.pool.Acquire(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return queue.Item{}, false, ctx.Err()
		}
		return queue.Item{}, false, fmt.Errorf("pgqueue: claim: %w", err)
	}
	defer conn.Release()
	if err := ctx.Err(); err != nil {
		return queue.Item{}, false, err // stopped while acquiring: ask nothing
	}

	stmtCtx, stop := answerContext(ctx, claimAnswerWait)
	defer stop()
	var id string
	var attempt, failures int64
	err = conn.QueryRow(stmtCtx, q.sqlClaim, args...).Scan(&id, &attempt, &failures)
	switch {
	case err == nil:
		return queue.Item{RunID: id, Attempt: int(attempt), Failures: int(failures)}, true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return queue.Item{}, false, nil
	case stmtCtx.Err() != nil:
		// Silent past the bound: whether the statement took a lease is
		// unknown, and one it took lapses after its ttl.
		return queue.Item{}, false, fmt.Errorf("pgqueue: claim: no answer within %v of cancellation: %w", claimAnswerWait, ctx.Err())
	default:
		// Not ctx's doing, even if ctx is done by now: the statement runs on
		// stmtCtx. Postgres may have committed it before the answer was lost
		// (a broken connection), so the outcome is unknown here too.
		return queue.Item{}, false, fmt.Errorf("pgqueue: claim: %w", err)
	}
}

// answerContext returns the context a statement that must not be abandoned
// runs on. It carries ctx's values but not its cancellation or deadline
// while ctx is live; once ctx is done, it stays live for wait more, so a
// statement already sent can still deliver its answer, and then it is
// cancelled. stop releases it.
func answerContext(ctx context.Context, wait time.Duration) (context.Context, context.CancelFunc) {
	stmtCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stopGrace := context.AfterFunc(ctx, func() {
		t := time.AfterFunc(wait, cancel)
		context.AfterFunc(stmtCtx, func() { t.Stop() })
	})
	return stmtCtx, func() {
		stopGrace()
		cancel()
	}
}

// Heartbeat extends the lease by ttl iff the (RunID, Attempt) fence still
// holds and the lease has not lapsed.
func (q *Queue) Heartbeat(ctx context.Context, it queue.Item, ttl time.Duration) (time.Time, error) {
	deadline := time.Now().Add(ttl)
	args := []any{it.RunID, it.Attempt, ttl.Seconds()}
	if q.qScope != "" {
		args = append(args, q.m.Scope.Value)
	}
	tag, err := q.pool.Exec(ctx, q.sqlHeartbeat, args...)
	if err != nil {
		return time.Time{}, fmt.Errorf("pgqueue: heartbeat %s attempt %d: %w", it.RunID, it.Attempt, err)
	}
	if tag.RowsAffected() == 0 {
		return time.Time{}, fmt.Errorf("pgqueue: heartbeat %s attempt %d: %w", it.RunID, it.Attempt, run.ErrSuperseded)
	}
	return deadline, nil
}

// Release gives the run back voluntarily, claimable again after delay.
// Fenced by it.Attempt. failed says whether this execution consumed retry
// budget: true increments the failures counter, false (an interruption,
// e.g. graceful shutdown) leaves it untouched.
func (q *Queue) Release(ctx context.Context, it queue.Item, delay time.Duration, failed bool) error {
	args := []any{it.RunID, it.Attempt, delay.Seconds(), failed}
	if q.qScope != "" {
		args = append(args, q.m.Scope.Value)
	}
	tag, err := q.pool.Exec(ctx, q.sqlRelease, args...)
	if err != nil {
		return fmt.Errorf("pgqueue: release %s attempt %d: %w", it.RunID, it.Attempt, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("pgqueue: release %s attempt %d: %w", it.RunID, it.Attempt, run.ErrSuperseded)
	}
	return nil
}

// Settle removes the run from the queue: the row stays — keeping both the
// attempt lineage and the failures budget — but is neither ready nor leased.
// Fenced by it.Attempt.
func (q *Queue) Settle(ctx context.Context, it queue.Item) error {
	args := []any{it.RunID, it.Attempt}
	if q.qScope != "" {
		args = append(args, q.m.Scope.Value)
	}
	tag, err := q.pool.Exec(ctx, q.sqlSettle, args...)
	if err != nil {
		return fmt.Errorf("pgqueue: settle %s attempt %d: %w", it.RunID, it.Attempt, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("pgqueue: settle %s attempt %d: %w", it.RunID, it.Attempt, run.ErrSuperseded)
	}
	return nil
}
