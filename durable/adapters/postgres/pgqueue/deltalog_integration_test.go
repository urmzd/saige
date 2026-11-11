//go:build integration

package pgqueue_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/durable"
	"github.com/urmzd/saige/durable/adapters/postgres/pgkit"
	"github.com/urmzd/saige/durable/adapters/postgres/pgledger"
	"github.com/urmzd/saige/durable/adapters/postgres/pgqueue"
	"github.com/urmzd/saige/durable/queue"
	"github.com/urmzd/saige/durable/queue/queuetest"
	"github.com/urmzd/saige/durable/run"
	"github.com/urmzd/saige/durable/worker"
)

func newDeltaQueue(t *testing.T, pool *pgxpool.Pool, m pgqueue.Mapping) *pgqueue.DeltaQueue {
	t.Helper()
	dm := pgqueue.DefaultDeltaMapping()
	dm.Table = m.Table + "_delta"
	dm.Channel = m.Table + "_delta"
	applyDDL(t, pool, pgqueue.RecommendedDDL(m))
	applyDDL(t, pool, pgqueue.RecommendedDeltaDDL(dm))
	dropTables(t, pool, m.Table, dm.Table)

	q, err := pgqueue.NewDeltaQueue(pool, m, dm, pgqueue.WithPollInterval(5*time.Millisecond))
	if err != nil {
		t.Fatalf("NewDeltaQueue: %v", err)
	}
	if err := q.Validate(context.Background()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return q
}

func TestDeltaQueueConformance(t *testing.T) {
	pool := testPool(t)
	queuetest.Run(t, func(t *testing.T) queue.Queue {
		m := pgqueue.DefaultMapping()
		m.Table = uniqueName("dt_dq")
		return newDeltaQueue(t, pool, m)
	})
}

func TestDeltaQueueConformanceScoped(t *testing.T) {
	pool := testPool(t)
	queuetest.Run(t, func(t *testing.T) queue.Queue {
		m := pgqueue.DefaultMapping()
		m.Table = uniqueName("dt_dq_scoped")
		m.Scope = pgqueue.Scope{Column: "source", Value: "anchor"}
		return newDeltaQueue(t, pool, m)
	})
}

func TestDeltaLogConformance(t *testing.T) {
	pool := testPool(t)
	queuetest.RunDeltaLog(t, func(t *testing.T) queue.DeltaLog {
		m := pgqueue.DefaultMapping()
		m.Table = uniqueName("dt_dl")
		return newDeltaQueue(t, pool, m)
	})
}

// TestDeltaLogFenceIgnoresOtherScope: a scoped queue fences only against its
// own rows, so another application's high attempt counter on the same run ID
// does not supersede our appends.
func TestDeltaLogFenceIgnoresOtherScope(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	m := pgqueue.DefaultMapping()
	m.Table = uniqueName("dt_dl_scope")
	m.Scope = pgqueue.Scope{Column: "source", Value: "anchor"}
	q := newDeltaQueue(t, pool, m)

	if _, err := pool.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %s (run_id, ready_at, lease_until, attempt, source)
		 VALUES ('shared', NULL, NULL, 99, 'other-app')`, m.Table)); err != nil {
		t.Fatalf("seed alien row: %v", err)
	}
	// The unique index is on run_id alone, so the alien row owns the ID in
	// this table; our scope sees no row for it and must allow the append.
	if err := q.Append(ctx, run.Delta{RunID: "shared", Attempt: 1, Kind: run.DeltaUser, Payload: []byte("x")}); err != nil {
		t.Fatalf("Append under scope: %v", err)
	}
}

// TestDeltaLogRoundTripsFields checks every column survives the trip,
// including an empty divider payload.
func TestDeltaLogRoundTripsFields(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	m := pgqueue.DefaultMapping()
	m.Table = uniqueName("dt_dl_fields")
	q := newDeltaQueue(t, pool, m)

	in := []run.Delta{
		{RunID: "r1", Attempt: 1, Kind: run.DeltaAttempt},
		{RunID: "r1", RecordKey: "llm#0", Attempt: 1, Kind: run.DeltaUser, Payload: []byte{0, 1, 2, 255}},
		{RunID: "r1", RecordKey: "llm#0", Attempt: 1, Kind: run.DeltaSeal},
	}
	for _, d := range in {
		if err := q.Append(ctx, d); err != nil {
			t.Fatalf("Append %+v: %v", d, err)
		}
	}
	got, _, err := q.Read(ctx, "r1", "", 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != len(in) {
		t.Fatalf("Read returned %d deltas, want %d", len(got), len(in))
	}
	for i := range in {
		w, g := in[i], got[i]
		if g.RunID != w.RunID || g.RecordKey != w.RecordKey || g.Attempt != w.Attempt || g.Kind != w.Kind || string(g.Payload) != string(w.Payload) {
			t.Errorf("delta %d = %+v, want %+v", i, g, w)
		}
	}
}

// TestDeltaLogWatchWakesByNotify uses a long poll interval so only NOTIFY
// can deliver the live append in time.
func TestDeltaLogWatchWakesByNotify(t *testing.T) {
	pool := testPool(t)
	m := pgqueue.DefaultMapping()
	m.Table = uniqueName("dt_dl_notify")
	dm := pgqueue.DefaultDeltaMapping()
	dm.Table = m.Table + "_delta"
	dm.Channel = m.Table + "_delta"
	applyDDL(t, pool, pgqueue.RecommendedDDL(m))
	applyDDL(t, pool, pgqueue.RecommendedDeltaDDL(dm))
	dropTables(t, pool, m.Table, dm.Table)
	q, err := pgqueue.NewDeltaQueue(pool, m, dm, pgqueue.WithPollInterval(time.Minute))
	if err != nil {
		t.Fatalf("NewDeltaQueue: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := q.Watch(ctx, "r1", "")
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		d, _, err := s.Recv(ctx)
		if err == nil && string(d.Payload) != "live" {
			err = fmt.Errorf("payload %q, want live", d.Payload)
		}
		done <- err
	}()

	// Give the listener time to LISTEN and the first query to come up empty.
	time.Sleep(300 * time.Millisecond)
	if err := q.Append(ctx, run.Delta{RunID: "r1", Attempt: 1, Kind: run.DeltaUser, Payload: []byte("live")}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("live append not delivered within 3s with a 1m poll: NOTIFY wake-up is broken")
	}
}

// TestDeltaQueuesShareOneListener: two queues on different channels share
// one pgkit.Listener, so one connection wakes both, with a poll long enough
// that only NOTIFY can deliver in time.
func TestDeltaQueuesShareOneListener(t *testing.T) {
	pool := testPool(t)
	l, err := pgkit.NewListener(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var queues []*pgqueue.DeltaQueue
	var streams []queue.Stream
	for range 2 {
		m := pgqueue.DefaultMapping()
		m.Table = uniqueName("dt_dl_shared")
		dm := pgqueue.DefaultDeltaMapping()
		dm.Table = m.Table + "_delta"
		dm.Channel = m.Table + "_delta"
		applyDDL(t, pool, pgqueue.RecommendedDDL(m))
		applyDDL(t, pool, pgqueue.RecommendedDeltaDDL(dm))
		dropTables(t, pool, m.Table, dm.Table)
		q, err := pgqueue.NewDeltaQueue(pool, m, dm, pgqueue.WithPollInterval(time.Minute), pgqueue.WithListener(l))
		if err != nil {
			t.Fatalf("NewDeltaQueue: %v", err)
		}
		s, err := q.Watch(ctx, "r1", "")
		if err != nil {
			t.Fatalf("Watch: %v", err)
		}
		queues, streams = append(queues, q), append(streams, s)
	}

	// The first Recv of each stream finds nothing and waits on the shared
	// subscription; give the listener time to LISTEN both channels.
	got := make(chan string, 2)
	for i, s := range streams {
		go func() {
			d, _, err := s.Recv(ctx)
			if err != nil {
				got <- fmt.Sprintf("stream %d: %v", i, err)
				return
			}
			got <- string(d.Payload)
		}()
	}
	time.Sleep(300 * time.Millisecond)
	for i, q := range queues {
		if err := q.Append(ctx, run.Delta{RunID: "r1", Attempt: 1, Kind: run.DeltaUser, Payload: []byte(fmt.Sprintf("q%d", i))}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	seen := map[string]bool{}
	for range 2 {
		select {
		case p := <-got:
			seen[p] = true
		case <-time.After(3 * time.Second):
			t.Fatalf("live appends not delivered within 3s with a 1m poll (got %v): the shared listener is not waking", seen)
		}
	}
	if !seen["q0"] || !seen["q1"] {
		t.Fatalf("deliveries = %v, want q0 and q1", seen)
	}
}

func TestDeltaQueueValidateActionableErrors(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	m := pgqueue.DefaultMapping()
	m.Table = uniqueName("dt_dl_validate")
	applyDDL(t, pool, pgqueue.RecommendedDDL(m))
	dropTables(t, pool, m.Table)

	dm := pgqueue.DefaultDeltaMapping()
	dm.Table = m.Table + "_delta"
	q, err := pgqueue.NewDeltaQueue(pool, m, dm)
	if err != nil {
		t.Fatalf("NewDeltaQueue: %v", err)
	}
	err = q.Validate(ctx)
	if err == nil || !strings.Contains(err.Error(), "delta table") || !strings.Contains(err.Error(), "CREATE TABLE") {
		t.Fatalf("Validate on missing delta table = %v, want actionable CREATE TABLE advice", err)
	}

	applyDDL(t, pool, fmt.Sprintf(`CREATE TABLE %s (seq bigint GENERATED ALWAYS AS IDENTITY, run_id text, record_key text, attempt bigint, kind text, payload jsonb)`, dm.Table))
	dropTables(t, pool, dm.Table)
	err = q.Validate(ctx)
	if err == nil || !strings.Contains(err.Error(), `"payload"`) || !strings.Contains(err.Error(), "bytea") {
		t.Fatalf("Validate on jsonb payload = %v, want a bytea type error", err)
	}
}

// TestPostgresOnlyStreaming is the end-to-end proof: pgledger + DeltaQueue,
// no Redis, and a workflow's Emit reaches Client.Watch through a worker.
func TestPostgresOnlyStreaming(t *testing.T) {
	pool := testPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	lm := pgledger.DefaultMapping()
	lm.Runs.Table = uniqueName("dt_e2e_runs")
	lm.Records.Table = uniqueName("dt_e2e_records")
	applyDDL(t, pool, pgledger.RecommendedDDL(lm))
	dropTables(t, pool, lm.Runs.Table, lm.Records.Table)
	lgr, err := pgledger.New(pool, lm)
	if err != nil {
		t.Fatalf("pgledger.New: %v", err)
	}

	m := pgqueue.DefaultMapping()
	m.Table = uniqueName("dt_e2e_queue")
	q := newDeltaQueue(t, pool, m)

	reg := run.NewRegistry()
	wf := durable.ActivityIn(reg, "stream-tokens", func(ctx context.Context, n int) (int, error) {
		for i := range n {
			if err := durable.Emit(ctx, fmt.Sprintf("tok-%d", i)); err != nil {
				return 0, err
			}
		}
		return n, nil
	})

	c := durable.New(lgr, q)
	w := worker.New(lgr, q, worker.WithRegistry(reg), worker.WithJanitorEvery(time.Hour))
	go func() { _ = w.Run(ctx) }()

	out, err := durable.Exec(ctx, c, wf, 3, durable.WithRunID("e2e-1"))
	if err != nil || out != 3 {
		t.Fatalf("Exec = %d, %v; want 3, nil", out, err)
	}

	s, err := c.Watch(ctx, "e2e-1", "")
	if err != nil {
		t.Fatalf("Client.Watch on a Postgres-only deployment: %v", err)
	}
	var users []string
	sawAttempt := false
	for len(users) < 3 {
		d, _, err := s.Recv(ctx)
		if err != nil {
			t.Fatalf("Recv after %v: %v", users, err)
		}
		switch d.Kind {
		case run.DeltaAttempt:
			sawAttempt = true
		case run.DeltaUser:
			users = append(users, string(d.Payload))
		}
	}
	if !sawAttempt {
		t.Error("no DeltaAttempt divider before the user deltas")
	}
	want := []string{`"tok-0"`, `"tok-1"`, `"tok-2"`}
	if strings.Join(users, ",") != strings.Join(want, ",") {
		t.Fatalf("user deltas = %v, want %v", users, want)
	}
}
