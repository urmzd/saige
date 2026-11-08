//go:build integration

package pgkit_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/durable/adapters/postgres/pgkit"
	"github.com/urmzd/saige/durable/adapters/postgres/pgledger"
	"github.com/urmzd/saige/durable/adapters/postgres/pgqueue"
)

const wait = 5 * time.Second

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("SAIGE_TEST_POSTGRES_DSN")
	if url == "" {
		url = "postgres://postgres:test@localhost:5433/postgres?sslmode=disable"
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse %q: %v", url, err)
	}
	cfg.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pgxpool.New(%q): %v", url, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping %q: %v (is docker compose up?)", url, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

var seq atomic.Int64

// uniqueName returns a name unique to this test-binary run, so concurrent
// and repeated runs never collide.
func uniqueName(prefix string) string {
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixMilli(), seq.Add(1))
}

func dropTables(t *testing.T, pool *pgxpool.Pool, tables ...string) {
	t.Cleanup(func() {
		for _, tab := range tables {
			if _, err := pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+tab); err != nil {
				t.Errorf("drop %s: %v", tab, err)
			}
		}
	})
}

func tableExists(t *testing.T, pool *pgxpool.Pool, table string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(), `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&ok); err != nil {
		t.Fatalf("to_regclass(%s): %v", table, err)
	}
	return ok
}

func TestApplyIsIdempotentAndTakesMultiStatementSteps(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tab := uniqueName("pk_apply")
	dropTables(t, pool, tab)
	steps := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (id bigint PRIMARY KEY)`, tab),
		"   ",
		fmt.Sprintf(`ALTER TABLE %[1]s ADD COLUMN IF NOT EXISTS note text;
CREATE INDEX IF NOT EXISTS %[1]s_note_idx ON %[1]s (note)`, tab),
	}
	for i := range 3 {
		if err := pgkit.Apply(ctx, pool, tab, steps...); err != nil {
			t.Fatalf("Apply #%d: %v", i+1, err)
		}
	}
	var cols int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = $1`, tab).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 2 || !tableExists(t, pool, tab+"_note_idx") {
		t.Fatalf("after Apply: %d columns, index %v; want 2 columns and the index", cols, tableExists(t, pool, tab+"_note_idx"))
	}

	// A *pgx.Conn is a Beginner too.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if err := pgkit.Apply(ctx, conn.Conn(), tab, steps...); err != nil {
		t.Fatalf("Apply on a *pgx.Conn: %v", err)
	}
}

func TestApplyRollsBackEveryStepOnFailure(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tab := uniqueName("pk_rollback")
	dropTables(t, pool, tab)
	err := pgkit.Apply(ctx, pool, tab,
		fmt.Sprintf(`CREATE TABLE %s (id int)`, tab),
		`SELECT * FROM pk_no_such_table`)
	if err == nil || !strings.Contains(err.Error(), "step 1 (SELECT * FROM pk_no_such_table)") {
		t.Fatalf("Apply with a failing step = %v, want an error naming step 1", err)
	}
	if tableExists(t, pool, tab) {
		t.Fatal("step 0 survived a failure in step 1; Apply must be one transaction")
	}
}

// Processes that start together migrate one at a time: every Apply's
// locked section runs alone, so racing CREATE TABLE IF NOT EXISTS never
// trips over itself.
func TestApplySerializesConcurrentCallers(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	const callers = 8
	for round := range 3 {
		tab := uniqueName("pk_race")
		dropTables(t, pool, tab)
		steps := []string{
			fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (started timestamptz NOT NULL, finished timestamptz NOT NULL)`, tab),
			fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %[1]s_started_idx ON %[1]s (started)`, tab),
			fmt.Sprintf(`DO $$
DECLARE s timestamptz := clock_timestamp();
BEGIN
	PERFORM pg_sleep(0.02);
	INSERT INTO %s VALUES (s, clock_timestamp());
END $$`, tab),
		}
		var wg sync.WaitGroup
		errs := make(chan error, callers)
		for range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- pgkit.Apply(ctx, pool, tab, steps...)
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d: concurrent Apply: %v", round, err)
			}
		}

		rows, err := pool.Query(ctx, fmt.Sprintf(`SELECT started, finished FROM %s ORDER BY started`, tab))
		if err != nil {
			t.Fatal(err)
		}
		var spans [][2]time.Time
		for rows.Next() {
			var s, f time.Time
			if err := rows.Scan(&s, &f); err != nil {
				t.Fatal(err)
			}
			spans = append(spans, [2]time.Time{s, f})
		}
		rows.Close()
		if len(spans) != callers {
			t.Fatalf("round %d: %d executions recorded, want %d", round, len(spans), callers)
		}
		for i := 1; i < len(spans); i++ {
			if spans[i][0].Before(spans[i-1][1]) {
				t.Fatalf("round %d: executions overlapped: %v began before %v ended", round, spans[i][0], spans[i-1][1])
			}
		}
	}
}

// The lock is named and transaction-scoped: an Apply joined to an open
// transaction holds it until that transaction ends, the same name waits,
// and a different name does not.
func TestApplyLockIsNamedAndTransactionScoped(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	name := uniqueName("pk_lock")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := pgkit.Apply(ctx, tx, name, "SELECT 1"); err != nil {
		t.Fatalf("Apply inside a transaction: %v", err)
	}

	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if err := pgkit.Apply(short, pool, name, "SELECT 1"); err == nil {
		t.Fatal("Apply under a lock held by an open transaction returned at once; want it to wait")
	}
	if err := pgkit.Apply(ctx, pool, name+"_other", "SELECT 1"); err != nil {
		t.Fatalf("Apply under another name: %v", err)
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	quick, cancel2 := context.WithTimeout(ctx, wait)
	defer cancel2()
	if err := pgkit.Apply(quick, pool, name, "SELECT 1"); err != nil {
		t.Fatalf("Apply after the holder committed: %v", err)
	}
}

// The recommended DDL is what an application hands Apply: applied twice, it
// leaves tables that both adapters validate, with byte-ordered keys.
func TestApplyRecommendedDDL(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	lm := pgledger.DefaultMapping()
	lm.Runs.Table = uniqueName("pk_runs")
	lm.Records.Table = uniqueName("pk_records")
	qm := pgqueue.DefaultMapping()
	qm.Table = uniqueName("pk_queue")
	dm := pgqueue.DefaultDeltaMapping()
	dm.Table = qm.Table + "_delta"
	dm.Channel = dm.Table
	dropTables(t, pool, lm.Runs.Table, lm.Records.Table, qm.Table, dm.Table)

	for range 2 {
		if err := pgkit.Apply(ctx, pool, "anchor.durable."+qm.Table,
			pgledger.RecommendedDDL(lm), pgqueue.RecommendedDDL(qm), pgqueue.RecommendedDeltaDDL(dm)); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	lgr, err := pgledger.New(pool, lm)
	if err != nil {
		t.Fatal(err)
	}
	if err := lgr.Validate(ctx); err != nil {
		t.Fatalf("ledger Validate after Apply: %v", err)
	}
	q, err := pgqueue.NewDeltaQueue(pool, qm, dm)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Validate(ctx); err != nil {
		t.Fatalf("queue Validate after Apply: %v", err)
	}

	for _, c := range [][2]string{
		{lm.Runs.Table, "run_id"}, {lm.Records.Table, "run_id"}, {lm.Records.Table, "record_key"},
		{qm.Table, "run_id"}, {dm.Table, "run_id"},
	} {
		var coll *string
		if err := pool.QueryRow(ctx,
			`SELECT collation_name FROM information_schema.columns WHERE table_name = $1 AND column_name = $2`,
			c[0], c[1]).Scan(&coll); err != nil {
			t.Fatalf("collation of %s.%s: %v", c[0], c[1], err)
		}
		if coll == nil || *coll != "C" {
			t.Errorf("%s.%s collation = %v, want C", c[0], c[1], coll)
		}
	}
}

func newListener(t *testing.T, pool *pgxpool.Pool, opts ...pgkit.ListenerOption) *pgkit.Listener {
	t.Helper()
	l, err := pgkit.NewListener(pool, opts...)
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	return l
}

// next reads one notification or fails the test.
func next(t *testing.T, ch <-chan pgkit.Notification) pgkit.Notification {
	t.Helper()
	select {
	case n, ok := <-ch:
		if !ok {
			t.Fatal("subscription closed")
		}
		return n
	case <-time.After(wait):
		t.Fatal("no notification within the deadline")
	}
	return pgkit.Notification{}
}

func quiet(t *testing.T, ch <-chan pgkit.Notification, d time.Duration) {
	t.Helper()
	select {
	case n := <-ch:
		t.Fatalf("got %+v, want nothing", n)
	case <-time.After(d):
	}
}

func notify(t *testing.T, pool *pgxpool.Pool, channel, payload string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `SELECT pg_notify($1, $2)`, channel, payload); err != nil {
		t.Fatalf("pg_notify(%s): %v", channel, err)
	}
}

func TestListenerDeliversPerChannelAfterResync(t *testing.T) {
	pool := testPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := newListener(t, pool)
	a, b := uniqueName("pk_a"), uniqueName("Pk_Mixed_B")

	chA, err := l.Listen(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if n := next(t, chA); !n.Resync || n.Channel != a {
		t.Fatalf("first message = %+v, want a Resync on %s", n, a)
	}
	if !l.Listening() {
		t.Fatal("Listening() = false after the Resync")
	}
	notify(t, pool, a, "p1")
	if n := next(t, chA); n != (pgkit.Notification{Channel: a, Payload: "p1"}) {
		t.Fatalf("got %+v, want p1 on %s", n, a)
	}

	// A second channel joins the live connection; names are exact.
	chB, err := l.Listen(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if n := next(t, chB); !n.Resync {
		t.Fatalf("first message on %s = %+v, want a Resync", b, n)
	}
	notify(t, pool, strings.ToLower(b), "wrong case")
	notify(t, pool, b, "p2")
	if n := next(t, chB); n.Payload != "p2" {
		t.Fatalf("got %+v on %s, want p2 (a channel is case-sensitive)", n, b)
	}
	quiet(t, chA, 100*time.Millisecond)
}

func TestListenerFansOutAndClosesWhenIdle(t *testing.T) {
	pool := testPool(t)
	l := newListener(t, pool)
	c := uniqueName("pk_fan")
	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	ch1, err := l.Listen(ctx1, c)
	if err != nil {
		t.Fatal(err)
	}
	ch2, err := l.Listen(ctx2, c)
	if err != nil {
		t.Fatal(err)
	}
	next(t, ch1)
	next(t, ch2)
	notify(t, pool, c, "both")
	if a, b := next(t, ch1), next(t, ch2); a.Payload != "both" || b.Payload != "both" {
		t.Fatalf("fan-out got %+v and %+v", a, b)
	}

	cancel1()
	if _, ok := <-ch1; ok {
		t.Fatal("a cancelled subscription stayed open")
	}
	notify(t, pool, c, "one")
	if n := next(t, ch2); n.Payload != "one" {
		t.Fatalf("remaining subscriber got %+v", n)
	}
	if got := listenBackends(t, pool, c); got != 1 {
		t.Fatalf("%d connections listening on %s, want 1", got, c)
	}

	cancel2()
	deadline := time.Now().Add(wait)
	for listenBackends(t, pool, c) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the connection outlived the last subscription")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if l.Listening() {
		t.Fatal("Listening() = true with nothing subscribed")
	}
}

// listenBackends counts connections whose last statement listened on c.
func listenBackends(t *testing.T, pool *pgxpool.Pool, c string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_stat_activity WHERE query = $1`, "LISTEN "+pgx.Identifier{c}.Sanitize()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestListenerReconnectsAndResyncs(t *testing.T) {
	pool := testPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := newListener(t, pool, pgkit.WithBackoff(10*time.Millisecond, 50*time.Millisecond))
	c := uniqueName("pk_reconnect")
	ch, err := l.Listen(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	next(t, ch)

	var killed int
	if err := pool.QueryRow(ctx,
		`SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE query = $1`,
		"LISTEN "+pgx.Identifier{c}.Sanitize()).Scan(&killed); err != nil || killed != 1 {
		t.Fatalf("terminate the listening backend: killed %d, %v", killed, err)
	}
	if n := next(t, ch); !n.Resync {
		t.Fatalf("after the connection died got %+v, want a Resync", n)
	}
	notify(t, pool, c, "after")
	if n := next(t, ch); n.Payload != "after" {
		t.Fatalf("after reconnect got %+v, want the payload", n)
	}
}

// A subscriber that stops reading never holds up the others; what it misses
// is folded into a Resync.
func TestListenerSlowSubscriberDoesNotBlockOthers(t *testing.T) {
	pool := testPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := newListener(t, pool, pgkit.WithBuffer(2))
	c := uniqueName("pk_slow")
	slow, err := l.Listen(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	fast, err := l.Listen(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	next(t, fast)
	const total = 20
	for i := range total {
		notify(t, pool, c, fmt.Sprint(i))
		if n := next(t, fast); n.Payload != fmt.Sprint(i) {
			t.Fatalf("fast subscriber got %+v, want %d", n, i)
		}
	}
	var got []pgkit.Notification
	for len(got) < 2 {
		got = append(got, next(t, slow))
	}
	if !got[0].Resync || !got[1].Resync {
		t.Fatalf("slow subscriber holds %+v, want its first Resync and the one that replaced the overflow", got)
	}
}

func TestListenerSubscriptionEndsWithContext(t *testing.T) {
	pool := testPool(t)
	l := newListener(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	ch, err := l.Listen(ctx, uniqueName("pk_ctx"))
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("channel closed before its context ended: %v", ctx.Err())
	}
}
