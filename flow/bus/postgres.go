package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/flow/event"
)

// pgSchema is everything the Postgres bus owns. The tables of the agent-keyed
// bus it replaced (fiber_bus_runs, fiber_bus_events) are not touched or read.
//
// fiber_event_runs holds each run's high-water mark and the time of its last
// event, which the sweep ages runs out by.
//
// fiber_event_tenants holds each tenant's last position (top). It is never
// swept, so a position is never handed out twice, even after every event of
// the tenant has aged out.
//
// fiber_events is the retained window, and the outbox consumer groups read.
// tseq is an event's position among its tenant's events, the cursor every
// reader scans by. Publishes to one tenant serialize on one advisory lock
// per tenant (see fiber_event_publish), so a tenant's position is assigned
// and committed before its next one is, and a reader scanning "tseq >
// cursor" can never step over a row that commits late. Publishes to
// different tenants do not wait on each other. id is the append order across
// every tenant, kept for retention and the sweep. A run event (seq > 0) is
// unique by (tenant, run, seq); every other event (seq = 0) by (tenant,
// event_id).
//
// fiber_consumers is each consumer group's cursor and lease, per tenant.
//
// A table created before positions existed gains tseq here, numbered per
// tenant in id order, which is the order its rows committed in.
const pgSchema = `
CREATE TABLE IF NOT EXISTS fiber_event_runs (
	tenant     text        NOT NULL,
	run        text        NOT NULL,
	top        bigint      NOT NULL,
	updated_at timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (tenant, run)
);

CREATE TABLE IF NOT EXISTS fiber_event_tenants (
	tenant text   PRIMARY KEY,
	top    bigint NOT NULL
);

CREATE TABLE IF NOT EXISTS fiber_events (
	id       bigserial   PRIMARY KEY,
	tenant   text        NOT NULL,
	tseq     bigint,
	channel  text        NOT NULL,
	type     text        NOT NULL,
	run      text        NOT NULL DEFAULT '',
	seq      bigint      NOT NULL DEFAULT 0,
	event_id text        NOT NULL,
	body     text        NOT NULL,
	at       timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE fiber_events ADD COLUMN IF NOT EXISTS tseq bigint;
UPDATE fiber_events e SET tseq = n.base + n.rn
	FROM (SELECT f.id,
			row_number() OVER (PARTITION BY f.tenant ORDER BY f.id) AS rn,
			COALESCE((SELECT max(x.tseq) FROM fiber_events x WHERE x.tenant = f.tenant), 0) AS base
		FROM fiber_events f WHERE f.tseq IS NULL) n
	WHERE e.id = n.id;
INSERT INTO fiber_event_tenants (tenant, top)
	SELECT tenant, max(tseq) FROM fiber_events GROUP BY tenant
	ON CONFLICT (tenant) DO UPDATE SET top = GREATEST(fiber_event_tenants.top, EXCLUDED.top);
ALTER TABLE fiber_events ALTER COLUMN tseq SET NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS fiber_events_run_seq ON fiber_events (tenant, run, seq) WHERE seq > 0;
CREATE UNIQUE INDEX IF NOT EXISTS fiber_events_event_id ON fiber_events (tenant, event_id) WHERE seq = 0;
CREATE UNIQUE INDEX IF NOT EXISTS fiber_events_tenant_tseq ON fiber_events (tenant, tseq);
CREATE INDEX IF NOT EXISTS fiber_events_channel ON fiber_events (tenant, channel, id);
CREATE INDEX IF NOT EXISTS fiber_events_run ON fiber_events (tenant, run, id);
CREATE INDEX IF NOT EXISTS fiber_events_at ON fiber_events (at);

CREATE TABLE IF NOT EXISTS fiber_consumers (
	"group"     text        NOT NULL,
	tenant      text        NOT NULL,
	cursor      bigint      NOT NULL DEFAULT 0,
	owner       text        NOT NULL DEFAULT '',
	lease_until timestamptz NOT NULL DEFAULT '-infinity',
	PRIMARY KEY ("group", tenant)
);

CREATE OR REPLACE FUNCTION fiber_event_publish(p_tenant text, p_channel text, p_type text, p_run text, p_seq bigint, p_event_id text, p_body text, p_retention int)
RETURNS boolean LANGUAGE plpgsql AS $$
DECLARE
	v_top  bigint;
	v_tseq bigint;
BEGIN
	PERFORM pg_advisory_xact_lock(hashtextextended('fiber_events:' || current_schema() || ':' || p_tenant, 0));
	IF p_seq > 0 THEN
		SELECT r.top INTO v_top FROM fiber_event_runs r WHERE r.tenant = p_tenant AND r.run = p_run;
		IF v_top IS NOT NULL AND p_seq <= v_top THEN
			RETURN false;
		END IF;
	ELSIF EXISTS (SELECT 1 FROM fiber_events x WHERE x.tenant = p_tenant AND x.event_id = p_event_id AND x.seq = 0) THEN
		RETURN false;
	END IF;
	INSERT INTO fiber_event_tenants (tenant, top) VALUES (p_tenant, 1)
		ON CONFLICT (tenant) DO UPDATE SET top = fiber_event_tenants.top + 1
		RETURNING top INTO v_tseq;
	INSERT INTO fiber_events (tenant, tseq, channel, type, run, seq, event_id, body)
		VALUES (p_tenant, v_tseq, p_channel, p_type, p_run, GREATEST(p_seq, 0), p_event_id, p_body);
	IF p_seq > 0 THEN
		INSERT INTO fiber_event_runs (tenant, run, top, updated_at) VALUES (p_tenant, p_run, p_seq, now())
			ON CONFLICT (tenant, run) DO UPDATE SET top = EXCLUDED.top, updated_at = EXCLUDED.updated_at;
		DELETE FROM fiber_events e
			WHERE e.tenant = p_tenant AND e.run = p_run AND e.seq > 0
			AND e.seq < (SELECT x.seq FROM fiber_events x
				WHERE x.tenant = p_tenant AND x.run = p_run AND x.seq > 0
				ORDER BY x.seq DESC OFFSET p_retention - 1 LIMIT 1);
	ELSE
		DELETE FROM fiber_events e
			WHERE e.tenant = p_tenant AND e.channel = p_channel AND e.seq = 0
			AND e.id < (SELECT x.id FROM fiber_events x
				WHERE x.tenant = p_tenant AND x.channel = p_channel AND x.seq = 0
				ORDER BY x.id DESC OFFSET p_retention - 1 LIMIT 1);
	END IF;
	PERFORM pg_notify('fiber_events', p_tenant);
	RETURN true;
END
$$;
`

// pgMigrateLock is the advisory lock Migrate holds, the one every flow store
// takes, so two processes starting together do not race on the catalog.
const pgMigrateLock = 0x6d616e64

// pgChannel is the LISTEN/NOTIFY channel publishes wake readers on.
const pgChannel = "fiber_events"

// listeningPollFactor stretches a reader's safety poll while the LISTEN
// connection is healthy.
const listeningPollFactor = 20

// Postgres is a bus on two tables in the database a deployment already has.
//
// Sequencing, positions, idempotent replay, dedupe and retention happen
// inside one publish function, under one advisory lock per tenant, so a
// tenant's publishes are serialized and different tenants' are not.
//
// Readers are one goroutine per subscription. Each scans fiber_events by
// position (tseq), narrowed in SQL to its tenant, channels and run, then
// waits for a NOTIFY whose payload is its tenant (one LISTEN connection per
// bus, held while anything is subscribed) or the poll interval.
//
// Consumer groups keep their cursors and leases in fiber_consumers. A lease
// is timed by the database's clock, so replicas whose clocks drift agree on
// when it expires.
//
// Events are durable rows until retention trims them or a sweep finds them
// older than WithIndexTTL (24 hours by default). Those rows reach the
// database's WAL, replicas and backups like any other.
//
// It does not own the pool. Call Migrate once before first use.
type Postgres struct {
	pool      *pgxpool.Pool
	retention int
	backlog   int
	poll      time.Duration
	ttl       time.Duration
	opts      options
	hub       *pgHub

	mu    sync.Mutex
	swept time.Time
}

var (
	_ Bus         = (*Postgres)(nil)
	_ TxPublisher = (*Postgres)(nil)
)

// NewPostgres returns a bus on pool. It honours WithRetention, WithBacklog,
// WithPollInterval, WithIndexTTL, WithMaxHops and WithLease, and does not
// touch the database.
func NewPostgres(pool *pgxpool.Pool, opts ...Option) *Postgres {
	o := newOptions(opts)
	return &Postgres{
		pool:      pool,
		retention: o.retention,
		backlog:   o.backlog,
		poll:      o.poll,
		ttl:       o.indexTTL,
		opts:      o,
		hub:       &pgHub{pool: pool, retry: o.poll},
		swept:     time.Now(),
	}
}

// Migrate creates the tables and the publish function, and numbers the rows
// of a table created before positions existed. It is idempotent and safe to
// run from every replica at startup.
func (p *Postgres) Migrate(ctx context.Context) error {
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(pgMigrateLock)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, pgSchema)
		return err
	})
	if err != nil {
		return fmt.Errorf("bus: migrate: %w", err)
	}
	return nil
}

// Publish implements Bus.
func (p *Postgres) Publish(ctx context.Context, e event.Envelope) error {
	if err := p.publish(ctx, p.pool, e); err != nil {
		return err
	}
	p.sweep(ctx)
	return nil
}

// PublishTx implements TxPublisher: it writes e in tx, the caller's
// transaction, so the state change tx makes and the event announcing it
// commit together or not at all. Readers see the event once tx commits;
// a rollback leaves no event and hands out no position.
//
// The tenant's publish lock is taken in tx and held until tx ends, which is
// what keeps positions committing in order: every other publish to the same
// tenant, from any process, waits for tx. Keep tx short.
//
// tx must be on the database and schema this bus was migrated in. The
// retention sweep is left to the next Publish.
func (p *Postgres) PublishTx(ctx context.Context, tx pgx.Tx, e event.Envelope) error {
	if tx == nil {
		return fmt.Errorf("%w: no transaction to publish %q in", ErrInvalid, e.ID)
	}
	return p.publish(ctx, tx, e)
}

// querier is what a publish runs on: the pool, or a caller's transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// publish validates e and runs the publish function on q.
func (p *Postgres) publish(ctx context.Context, q querier, e event.Envelope) error {
	if err := normalize(&e, p.opts); err != nil {
		return err
	}
	if e.Seq > maxPos {
		return fmt.Errorf("%w: sequence %d does not fit in a bigint", ErrInvalid, e.Seq)
	}
	body, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("bus: encode %s %q: %w", e.Type, e.ID, err)
	}
	var stored bool
	err = q.QueryRow(ctx, `SELECT fiber_event_publish($1, $2, $3, $4, $5, $6, $7, $8)`,
		e.Tenant, e.Channel, e.Type, e.Run, int64(e.Seq), e.ID, string(body), p.retention).Scan(&stored)
	if err != nil {
		return fmt.Errorf("bus: publish %s %q on %s: %w", e.Type, e.ID, e.Channel, err)
	}
	return nil
}

// sweep forgets runs and events older than the TTL. A failure is ignored:
// the next sweep tries again.
func (p *Postgres) sweep(ctx context.Context) {
	p.mu.Lock()
	due := time.Since(p.swept) >= p.ttl/4
	if due {
		p.swept = time.Now()
	}
	p.mu.Unlock()
	if !due {
		return
	}
	cutoff := time.Now().Add(-p.ttl)
	_, _ = p.pool.Exec(ctx, `DELETE FROM fiber_events WHERE at < $1`, cutoff)
	_, _ = p.pool.Exec(ctx, `DELETE FROM fiber_event_runs WHERE updated_at < $1`, cutoff)
}

// Subscribe implements Bus.
func (p *Postgres) Subscribe(ctx context.Context, sub Subscription) (Stream, error) {
	if err := validate(sub); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if sub.From > maxPos {
		return nil, fmt.Errorf("%w: cursor %d does not fit in a bigint", ErrInvalid, sub.From)
	}
	return p.open(ctx, sub, 0), nil
}

// open starts a reader of sub after the position after.
func (p *Postgres) open(ctx context.Context, sub Subscription, after uint64) *pgStream {
	readCtx, cancel := context.WithCancel(ctx)
	s := &pgStream{
		p:      p,
		sub:    sub,
		ch:     make(chan Delivery, p.backlog),
		cancel: cancel,
		closed: make(chan struct{}),
		done:   make(chan struct{}),
	}
	if sub.matchesNothing() {
		go func() {
			defer close(s.done)
			<-readCtx.Done()
			s.err = ErrClosed
		}()
	} else {
		wake, unsubscribe := p.hub.subscribe(sub.Tenant)
		go s.read(readCtx, int64(after), wake, unsubscribe) //nolint:gosec // bus positions are BIGSERIAL ids, never negative
	}
	if done := ctx.Done(); done != nil {
		go func() {
			select {
			case <-done:
				_ = s.Close()
			case <-s.closed:
			}
		}()
	}
	return s
}

// Consume implements Bus.
func (p *Postgres) Consume(ctx context.Context, group string, sub Subscription) (GroupStream, error) {
	if err := validateGroup(group, sub); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if sub.From > maxPos {
		return nil, fmt.Errorf("%w: cursor %d does not fit in a bigint", ErrInvalid, sub.From)
	}
	return newGroupStream(ctx, p, group, sub, p.opts.lease), nil
}

// claim implements leases. A group seen for the first time starts at the
// oldest event retained.
func (p *Postgres) claim(ctx context.Context, group, tenant, owner string, ttl time.Duration) (uint64, bool, error) {
	var cursor int64
	err := p.pool.QueryRow(ctx, `INSERT INTO fiber_consumers ("group", tenant, cursor, owner, lease_until)
VALUES ($1, $2, 0, $3, now() + $4 * interval '1 millisecond')
ON CONFLICT ("group", tenant) DO UPDATE SET owner = EXCLUDED.owner, lease_until = EXCLUDED.lease_until
WHERE fiber_consumers.owner = EXCLUDED.owner OR fiber_consumers.owner = '' OR fiber_consumers.lease_until < now()
RETURNING cursor`, group, tenant, owner, ttl.Milliseconds()).Scan(&cursor)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("bus: claim %s for %s: %w", group, tenant, err)
	}
	return uint64(cursor), true, nil //nolint:gosec // bus positions are BIGSERIAL ids, never negative
}

// commit implements leases.
func (p *Postgres) commit(ctx context.Context, group, tenant, owner string, pos uint64) (bool, error) {
	tag, err := p.pool.Exec(ctx, `UPDATE fiber_consumers SET cursor = GREATEST(cursor, $4)
WHERE "group" = $1 AND tenant = $2 AND owner = $3`, group, tenant, owner, int64(pos)) //nolint:gosec // bus positions are BIGSERIAL ids, never negative
	if err != nil {
		return false, fmt.Errorf("bus: commit %s for %s: %w", group, tenant, err)
	}
	return tag.RowsAffected() == 1, nil
}

// release implements leases.
func (p *Postgres) release(ctx context.Context, group, tenant, owner string) error {
	_, err := p.pool.Exec(ctx, `UPDATE fiber_consumers SET owner = '', lease_until = '-infinity'
WHERE "group" = $1 AND tenant = $2 AND owner = $3`, group, tenant, owner)
	if err != nil {
		return fmt.Errorf("bus: release %s for %s: %w", group, tenant, err)
	}
	return nil
}

// read implements leases.
func (p *Postgres) read(ctx context.Context, sub Subscription, after uint64) (positioned, error) {
	return p.open(ctx, sub, after), nil
}

// pgStream is one subscription: a reader goroutine filling a bounded buffer.
type pgStream struct {
	p   *Postgres
	sub Subscription
	ch  chan Delivery

	once   sync.Once
	cancel context.CancelFunc
	closed chan struct{}
	done   chan struct{}
	err    error
}

var _ Stream = (*pgStream)(nil)

// Recv implements Stream.
func (s *pgStream) Recv(ctx context.Context) (event.Envelope, error) {
	d, err := s.next(ctx)
	return d.Event, err
}

// next returns the next event with its position.
func (s *pgStream) next(ctx context.Context) (Delivery, error) {
	return recv(ctx, s.ch, s.closed, s.done, func() error { return s.err })
}

// Close implements Stream. It waits for the reader, so nothing outlives it.
func (s *pgStream) Close() error {
	s.once.Do(func() {
		close(s.closed)
		s.cancel()
	})
	<-s.done
	return nil
}

// read scans the tenant's events after the position after, from the start
// of what is retained when it is zero, and then keeps scanning as rows
// arrive.
func (s *pgStream) read(ctx context.Context, after int64, wake <-chan struct{}, unsubscribe func()) {
	defer close(s.done)
	defer unsubscribe()

	for {
		if err := ctx.Err(); err != nil {
			s.err = ErrClosed
			return
		}
		n, err := s.batch(ctx, &after)
		if err != nil {
			if ctx.Err() != nil {
				s.err = ErrClosed
			} else {
				s.err = err
			}
			return
		}
		if n == readBatch {
			continue
		}
		wait := s.p.poll
		if s.p.hub.listening.Load() {
			wait *= listeningPollFactor
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
		case <-wake:
			t.Stop()
		case <-t.C:
		}
	}
}

// pgQuery narrows the scan as far as SQL can: the tenant, a channel prefix,
// the run, and the cursor over run sequences. Accepts decides the rest.
var pgQuery = `SELECT tseq, body FROM fiber_events
WHERE tseq > $1 AND tenant = $2
AND (seq = 0 OR seq > $3)
AND ($4 = '' OR run = $4)
AND EXISTS (SELECT 1 FROM unnest($5::text[]) AS p(prefix) WHERE starts_with(channel, p.prefix))
ORDER BY tseq LIMIT ` + strconv.Itoa(readBatch)

// batch reads one page after the cursor and hands what the subscription
// accepts to the consumer, advancing the cursor past every row it read.
func (s *pgStream) batch(ctx context.Context, after *int64) (int, error) {
	rows, err := s.p.pool.Query(ctx, pgQuery, *after, s.sub.Tenant, int64(s.sub.From), s.sub.Run, s.sub.Channels) //nolint:gosec // bus positions are BIGSERIAL ids, never negative
	if err != nil {
		return 0, fmt.Errorf("bus: read %s: %w", s.sub.Tenant, err)
	}
	type row struct {
		id   int64
		e    event.Envelope
		keep bool
	}
	var page []row
	for rows.Next() {
		var (
			id   int64
			body string
		)
		if err := rows.Scan(&id, &body); err != nil {
			rows.Close()
			return 0, fmt.Errorf("bus: read %s: %w", s.sub.Tenant, err)
		}
		var e event.Envelope
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			rows.Close()
			return 0, fmt.Errorf("bus: decode row %d: %w", id, err)
		}
		page = append(page, row{id: id, e: e, keep: s.sub.Accepts(e)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("bus: read %s: %w", s.sub.Tenant, err)
	}
	for _, r := range page {
		*after = r.id
		if !r.keep {
			continue
		}
		select {
		case s.ch <- Delivery{Event: r.e, Pos: uint64(r.id)}: //nolint:gosec // bus positions are BIGSERIAL ids, never negative
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return len(page), nil
}

// pgHub multiplexes one LISTEN connection across every live reader of a bus.
// It listens only while something is subscribed. A notification's payload is
// the tenant that was published to, so it wakes only that tenant's readers.
// Every (re)connect wakes every reader.
type pgHub struct {
	pool  *pgxpool.Pool
	retry time.Duration

	listening atomic.Bool

	mu     sync.Mutex
	subs   map[string]map[chan struct{}]struct{}
	count  int
	cancel context.CancelFunc
}

// subscribe registers a reader of tenant and returns its wake channel and the
// function that removes it.
func (h *pgHub) subscribe(tenant string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[string]map[chan struct{}]struct{}{}
	}
	if h.subs[tenant] == nil {
		h.subs[tenant] = map[chan struct{}]struct{}{}
	}
	h.subs[tenant][ch] = struct{}{}
	h.count++
	if h.count == 1 {
		ctx, cancel := context.WithCancel(context.Background())
		h.cancel = cancel
		go h.listen(ctx)
	}
	h.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			delete(h.subs[tenant], ch)
			if len(h.subs[tenant]) == 0 {
				delete(h.subs, tenant)
			}
			h.count--
			if h.count == 0 && h.cancel != nil {
				h.cancel()
				h.cancel = nil
			}
		})
	}
}

// wake signals the readers of the tenant a publish concerns. An empty
// payload wakes everyone.
func (h *pgHub) wake(tenant string) {
	if strings.TrimSpace(tenant) == "" {
		h.wakeAll()
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[tenant] {
		signal(ch)
	}
}

func (h *pgHub) wakeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, set := range h.subs {
		for ch := range set {
			signal(ch)
		}
	}
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (h *pgHub) listen(ctx context.Context) {
	pgListen(ctx, h.pool, h.retry, pgChannel, &h.listening, func(context.Context, *pgx.Conn) error {
		h.wakeAll()
		return nil
	}, h.wake)
}

// pgListen holds one LISTEN connection on channel until ctx ends, taking a
// new one retry after it fails. connected runs on every new connection once
// it listens, notified on every notification's payload. listening is true
// while a connection listens.
func pgListen(ctx context.Context, pool *pgxpool.Pool, retry time.Duration, channel string, listening *atomic.Bool, connected func(context.Context, *pgx.Conn) error, notified func(string)) {
	for ctx.Err() == nil {
		pgSession(ctx, pool, channel, listening, connected, notified)
		listening.Store(false)
		t := time.NewTimer(retry)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// pgSession holds one LISTEN connection until it fails or ctx ends.
func pgSession(ctx context.Context, pool *pgxpool.Pool, channel string, listening *atomic.Bool, connected func(context.Context, *pgx.Conn) error, notified func(string)) {
	pc, err := pool.Acquire(ctx)
	if err != nil {
		return
	}
	conn := pc.Hijack()
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
		return
	}
	listening.Store(true)
	if err := connected(ctx, conn); err != nil {
		return
	}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return
		}
		notified(n.Payload)
	}
}

// readBatch bounds how many rows one read asks for.
const readBatch = 128

// recv is the Recv of a stream fed by a reader goroutine.
func recv(ctx context.Context, ch <-chan Delivery, closed, done <-chan struct{}, err func() error) (Delivery, error) {
	for {
		if e := ctx.Err(); e != nil {
			return Delivery{}, e
		}
		select {
		case <-closed:
			return Delivery{}, ErrClosed
		default:
		}
		select {
		case d := <-ch:
			return d, nil
		default:
		}
		select {
		case <-done:
			if err() == nil {
				return Delivery{}, ErrClosed
			}
			return Delivery{}, err()
		default:
		}
		select {
		case d := <-ch:
			return d, nil
		case <-done:
		case <-closed:
		case <-ctx.Done():
			return Delivery{}, ctx.Err()
		}
	}
}
