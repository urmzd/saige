package pgqueue

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/durable/adapters/postgres/internal/pgident"
	"github.com/urmzd/saige/durable/adapters/postgres/pgkit"
	"github.com/urmzd/saige/durable/queue"
	"github.com/urmzd/saige/durable/run"
)

// deltaReadBatch is how many rows a Watch pulls per catch-up query.
const deltaReadBatch = 64

// listeningPollFactor stretches a stream's safety poll while the LISTEN
// connection is healthy: notifications do the waking, the poll only covers
// a notification lost between reconnects.
const listeningPollFactor = 25

// maxChannelLen is the longest channel Postgres keeps (NAMEDATALEN-1 bytes):
// pg_notify refuses a longer one, which would fail every append.
const maxChannelLen = 63

// DeltaMapping declares which existing table and columns carry the delta
// log. All column fields are required.
type DeltaMapping struct {
	// Table is the append-only delta table, optionally schema-qualified.
	Table string
	// Seq is an integer column the database fills on insert (an identity
	// or bigserial column): the per-table append order and the cursor.
	Seq string
	// RunID is the text column carrying the run ID.
	RunID string
	// RecordKey is the text column carrying the emitting record key; ""
	// for framework dividers.
	RecordKey string
	// Attempt is an integer column: the attempt that emitted the delta.
	Attempt string
	// Kind is the text column carrying run.DeltaKind.
	Kind string
	// Payload is the bytea column carrying the raw delta payload.
	Payload string
	// Channel is the LISTEN/NOTIFY channel that wakes watchers, at most
	// 63 bytes. Appends NOTIFY it with the run ID as payload; watchers
	// fall back to polling when the listener connection is unavailable
	// (for example behind a transaction-pooling proxy).
	Channel string
}

// DefaultDeltaMapping returns the mapping matching RecommendedDeltaDDL's
// suggested durable_delta table.
func DefaultDeltaMapping() DeltaMapping {
	return DeltaMapping{
		Table:     "durable_delta",
		Seq:       "seq",
		RunID:     "run_id",
		RecordKey: "record_key",
		Attempt:   "attempt",
		Kind:      "kind",
		Payload:   "payload",
		Channel:   "durable_delta",
	}
}

func (dm DeltaMapping) validate() error {
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	add(pgident.Table("DeltaMapping.Table", dm.Table))
	add(pgident.Column("DeltaMapping.Seq", dm.Seq))
	add(pgident.Column("DeltaMapping.RunID", dm.RunID))
	add(pgident.Column("DeltaMapping.RecordKey", dm.RecordKey))
	add(pgident.Column("DeltaMapping.Attempt", dm.Attempt))
	add(pgident.Column("DeltaMapping.Kind", dm.Kind))
	add(pgident.Column("DeltaMapping.Payload", dm.Payload))
	add(pgident.Column("DeltaMapping.Channel", dm.Channel))
	if len(dm.Channel) > maxChannelLen {
		errs = append(errs, fmt.Errorf("DeltaMapping.Channel: %q is longer than Postgres's %d-byte channel limit", dm.Channel, maxChannelLen))
	}
	return errors.Join(errs...)
}

// DeltaQueue is a Queue plus the queue.DeltaLog capability on a Postgres
// delta table, so a Postgres-only deployment can stream: Emit, Client.Watch,
// and PriorDeltas all work without Redis. Construct it with NewDeltaQueue;
// a plain Queue from New stays a queue without a delta log, exactly as
// before.
//
// Appends are fenced against the queue table's attempt counter in the same
// implicit transaction that writes them: a delta carrying an attempt below
// the run's current counter gets run.ErrSuperseded and writes nothing.
// Appends for unknown runs, and for the current attempt even when the run
// is not leased, are allowed. Each run's appends are serialized by a
// transaction-scoped advisory lock, so a run's Seq order is also its commit
// order and a cursor can never skip a late-committing row.
//
// Watchers share one pgkit.Listener subscription on the delta channel, held
// only while at least one Watch is live; the Watch ctx bounds it. The
// Listener is the DeltaQueue's own unless WithListener hands it the
// process's, so one connection carries every component's notifications.
// When the connection is unavailable, streams poll at the queue's poll
// interval.
type DeltaQueue struct {
	*Queue
	dm  DeltaMapping
	hub *deltaHub

	sqlAppend string
	sqlRead   string
	sqlTrim   string
	lockSpace string
}

var _ queue.DeltaLog = (*DeltaQueue)(nil)

// NewDeltaQueue builds a DeltaQueue over pool, the queue mapping m, and the
// delta mapping dm. Like New it validates the mappings and performs no I/O;
// call Validate to check both tables against the live database.
func NewDeltaQueue(pool *pgxpool.Pool, m Mapping, dm DeltaMapping, opts ...Option) (*DeltaQueue, error) {
	q, err := New(pool, m, opts...)
	if err != nil {
		return nil, err
	}
	if err := dm.validate(); err != nil {
		return nil, fmt.Errorf("pgqueue: invalid delta mapping: %w", err)
	}
	listener := q.listener
	if listener == nil {
		if listener, err = pgkit.NewListener(pool); err != nil {
			return nil, fmt.Errorf("pgqueue: %w", err)
		}
	}
	d := &DeltaQueue{Queue: q, dm: dm, lockSpace: dm.Table + ":"}
	d.hub = &deltaHub{listener: listener, channel: dm.Channel}
	d.buildDeltaSQL()
	return d, nil
}

func (d *DeltaQueue) buildDeltaSQL() {
	dt := pgident.QuoteTable(d.dm.Table)
	seq := pgident.QuoteColumn(d.dm.Seq)
	id := pgident.QuoteColumn(d.dm.RunID)
	key := pgident.QuoteColumn(d.dm.RecordKey)
	att := pgident.QuoteColumn(d.dm.Attempt)
	kind := pgident.QuoteColumn(d.dm.Kind)
	payload := pgident.QuoteColumn(d.dm.Payload)

	scope := ""
	if d.qScope != "" {
		scope = fmt.Sprintf(" AND f.%s = $7", d.qScope)
	}
	// The fence reads the queue row's attempt counter; NOT EXISTS keeps
	// unknown runs appendable. pg_notify runs only when the row was
	// written, and is delivered at commit.
	d.sqlAppend = fmt.Sprintf(`WITH ins AS (
	INSERT INTO %s (%s, %s, %s, %s, %s)
	SELECT $1, $2, $3, $4, $5
	WHERE NOT EXISTS (SELECT 1 FROM %s AS f WHERE f.%s = $1 AND f.%s > $3%s)
	RETURNING %s
)
SELECT ins.%s FROM ins, LATERAL (SELECT pg_notify($6, $1)) AS n`,
		dt, id, key, att, kind, payload,
		d.qTable, d.qID, d.qAttempt, scope,
		seq,
		seq)

	d.sqlRead = fmt.Sprintf(
		"SELECT %s, %s, %s, %s, %s FROM %s WHERE %s = $1 AND %s > $2 ORDER BY %s LIMIT $3",
		seq, key, att, kind, payload, dt, id, seq, seq)

	d.sqlTrim = fmt.Sprintf("DELETE FROM %s WHERE %s = $1", dt, id)
}

// Append implements queue.DeltaLog. One round trip: the advisory lock, the
// fenced insert, and the NOTIFY share pgx's implicit batch transaction.
func (d *DeltaQueue) Append(ctx context.Context, delta run.Delta) error {
	payload := delta.Payload
	if payload == nil {
		payload = []byte{} // the payload column is NOT NULL; a divider may carry none
	}
	args := []any{delta.RunID, delta.RecordKey, delta.Attempt, string(delta.Kind), payload, d.dm.Channel}
	if d.qScope != "" {
		args = append(args, d.m.Scope.Value)
	}
	b := &pgx.Batch{}
	b.Queue("SELECT pg_advisory_xact_lock(hashtextextended($1 || $2, 0))", d.lockSpace, delta.RunID)
	b.Queue(d.sqlAppend, args...)
	br := d.pool.SendBatch(ctx, b)
	if _, err := br.Exec(); err != nil {
		_ = br.Close()
		return fmt.Errorf("pgqueue: append %s attempt %d: lock: %w", delta.RunID, delta.Attempt, err)
	}
	var seq int64
	err := br.QueryRow().Scan(&seq)
	closeErr := br.Close()
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("pgqueue: append %s attempt %d: %w", delta.RunID, delta.Attempt, run.ErrSuperseded)
	case err != nil:
		return fmt.Errorf("pgqueue: append %s attempt %d: %w", delta.RunID, delta.Attempt, err)
	case closeErr != nil:
		return fmt.Errorf("pgqueue: append %s attempt %d: commit: %w", delta.RunID, delta.Attempt, closeErr)
	}
	return nil
}

// Read implements queue.DeltaLog: a bounded snapshot from the cursor, in
// append order, plus the cursor to continue from. The cursor is the Seq of
// the last delta returned; when nothing is returned the input cursor comes
// back unchanged. limit <= 0 means no bound.
func (d *DeltaQueue) Read(ctx context.Context, runID string, from queue.Cursor, limit int) ([]run.Delta, queue.Cursor, error) {
	after, err := parseCursor(from)
	if err != nil {
		return nil, "", fmt.Errorf("pgqueue: read %s: %w", runID, err)
	}
	var lim any
	if limit > 0 {
		lim = limit
	}
	deltas, curs, err := d.readAfter(ctx, runID, after, lim)
	if err != nil {
		return nil, "", fmt.Errorf("pgqueue: read %s from %q: %w", runID, from, err)
	}
	next := from
	if len(curs) > 0 {
		next = curs[len(curs)-1]
	}
	return deltas, next, nil
}

// readAfter returns the run's deltas with Seq > after, each paired with its
// cursor. A nil limit means no bound.
func (d *DeltaQueue) readAfter(ctx context.Context, runID string, after int64, limit any) ([]run.Delta, []queue.Cursor, error) {
	rows, err := d.pool.Query(ctx, d.sqlRead, runID, after, limit)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var deltas []run.Delta
	var curs []queue.Cursor
	for rows.Next() {
		var seq, attempt int64
		var key, kind string
		var payload []byte
		if err := rows.Scan(&seq, &key, &attempt, &kind, &payload); err != nil {
			return nil, nil, err
		}
		deltas = append(deltas, run.Delta{
			RunID:     runID,
			RecordKey: key,
			Attempt:   int(attempt),
			Kind:      run.DeltaKind(kind),
			Payload:   payload,
		})
		curs = append(curs, formatCursor(seq))
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return deltas, curs, nil
}

// Watch implements queue.DeltaLog. The returned Stream serves recorded
// history from the cursor first, then tails live appends, woken by
// LISTEN/NOTIFY with a poll fallback. ctx bounds the stream's lifetime: the
// wake-up subscription is released when it is done. Watchers are
// independent: each holds only its own cursor.
func (d *DeltaQueue) Watch(ctx context.Context, runID string, from queue.Cursor) (queue.Stream, error) {
	after, err := parseCursor(from)
	if err != nil {
		return nil, fmt.Errorf("pgqueue: watch %s: %w", runID, err)
	}
	wake, unsubscribe := d.hub.subscribe(runID)
	context.AfterFunc(ctx, unsubscribe)
	return &deltaStream{d: d, runID: runID, after: after, wake: wake}, nil
}

// Trim implements queue.DeltaLog: it deletes the run's delta rows. Queue
// state and the attempt counter are untouched. Seq keeps increasing, so a
// cursor taken before the Trim stays valid and simply sees later appends.
func (d *DeltaQueue) Trim(ctx context.Context, runID string) error {
	if _, err := d.pool.Exec(ctx, d.sqlTrim, runID); err != nil {
		return fmt.Errorf("pgqueue: trim %s: %w", runID, err)
	}
	return nil
}

// deltaStream is a Watch handle: a cursor into one run's delta log plus a
// buffer of fetched-but-undelivered rows. Not safe for concurrent Recv.
type deltaStream struct {
	d     *DeltaQueue
	runID string
	after int64
	wake  <-chan struct{}
	buf   []run.Delta
	curs  []queue.Cursor
}

// Recv implements queue.Stream. It drains the local buffer, refills it from
// the table, and otherwise waits for a NOTIFY wake-up or the poll interval,
// always honoring ctx between rounds.
func (s *deltaStream) Recv(ctx context.Context) (run.Delta, queue.Cursor, error) {
	for {
		if err := ctx.Err(); err != nil {
			return run.Delta{}, "", err
		}
		if len(s.buf) > 0 {
			d, cur := s.buf[0], s.curs[0]
			s.buf, s.curs = s.buf[1:], s.curs[1:]
			seq, _ := parseCursor(cur)
			s.after = seq
			return d, cur, nil
		}

		deltas, curs, err := s.d.readAfter(ctx, s.runID, s.after, deltaReadBatch)
		if err != nil {
			if ctx.Err() != nil {
				return run.Delta{}, "", ctx.Err()
			}
			return run.Delta{}, "", fmt.Errorf("pgqueue: watch %s: %w", s.runID, err)
		}
		if len(deltas) > 0 {
			s.buf, s.curs = deltas, curs
			continue
		}

		wait := s.d.poll
		if s.d.hub.listening() {
			wait *= listeningPollFactor
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return run.Delta{}, "", ctx.Err()
		case <-s.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// parseCursor decodes a cursor into the Seq it follows; "" is the start.
func parseCursor(c queue.Cursor) (int64, error) {
	if c == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(string(c), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid cursor %q", c)
	}
	return n, nil
}

func formatCursor(seq int64) queue.Cursor {
	return queue.Cursor(strconv.FormatInt(seq, 10))
}

// deltaHub routes one Listener subscription on the delta channel to the
// watchers of each run, by the run ID each append notifies with. It holds
// the subscription only while at least one watcher is subscribed. A Resync
// (the subscription starting, or the Listener reconnecting) wakes every
// watcher, so a notification lost in a gap costs at most one extra query.
type deltaHub struct {
	listener *pgkit.Listener
	channel  string

	mu    sync.Mutex
	subs  map[string]map[chan struct{}]struct{}
	count int
	// gen names the live subscription; synced reports that it has had
	// its first Resync, so the channel is being listened to.
	gen    uint64
	synced bool
	cancel context.CancelFunc
}

// subscribe registers a wake-up channel for runID, subscribing to the
// Listener on the first one. The returned func is idempotent.
func (h *deltaHub) subscribe(runID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subs == nil {
		h.subs = make(map[string]map[chan struct{}]struct{})
	}
	set := h.subs[runID]
	if set == nil {
		set = make(map[chan struct{}]struct{})
		h.subs[runID] = set
	}
	set[ch] = struct{}{}
	h.count++
	if h.count == 1 {
		// The cancel func is kept on the hub and called when the last
		// subscriber leaves, in the func returned below.
		ctx, cancel := context.WithCancel(context.Background()) //nolint:gosec // G118: cancelled by the last unsubscribe
		h.cancel = cancel
		h.gen++
		h.synced = false
		// The channel was validated with the mapping, so Listen cannot
		// refuse it; if it ever did, watchers would simply poll.
		if notes, err := h.listener.Listen(ctx, h.channel); err == nil {
			go h.route(h.gen, notes)
		}
	}
	h.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			delete(h.subs[runID], ch)
			if len(h.subs[runID]) == 0 {
				delete(h.subs, runID)
			}
			h.count--
			if h.count == 0 && h.cancel != nil {
				h.cancel()
				h.cancel = nil
				h.gen++
				h.synced = false
			}
		})
	}
}

// route turns one subscription's notifications into wake-ups until the
// subscription ends.
func (h *deltaHub) route(gen uint64, notes <-chan pgkit.Notification) {
	for n := range notes {
		if n.Resync {
			h.mu.Lock()
			if h.gen == gen {
				h.synced = true
			}
			h.mu.Unlock()
			h.wake("", true)
			continue
		}
		h.wake(n.Payload, false)
	}
}

// listening reports whether notifications are doing the waking: the
// subscription has been listened to and the connection is up.
func (h *deltaHub) listening() bool {
	h.mu.Lock()
	synced := h.synced
	h.mu.Unlock()
	return synced && h.listener.Listening()
}

// wake signals runID's watchers, or every watcher when all is true. Sends
// never block: a pending signal already guarantees a re-query.
func (h *deltaHub) wake(runID string, all bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	signal := func(set map[chan struct{}]struct{}) {
		for ch := range set {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
	if all {
		for _, set := range h.subs {
			signal(set)
		}
		return
	}
	signal(h.subs[runID])
}
