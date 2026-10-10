//go:build stress

package stress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/urmzd/duraturo/adapters/postgres/pgledger"
	"github.com/urmzd/duraturo/adapters/postgres/pgqueue"
	"github.com/urmzd/duraturo/pkg/worker"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/cache/memcache"
	"github.com/urmzd/saige/agent/durable/duraturo"
	"github.com/urmzd/saige/agent/notify"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
	"github.com/urmzd/saige/postgres"
	"github.com/urmzd/saige/rag/fusion"
	"github.com/urmzd/saige/rag/pgstore"
	ragtypes "github.com/urmzd/saige/rag/types"
)

// quietLogger discards the expected reconnect warnings.
var quietLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// TestNotifierFanOut publishes to many subscribers spread over several
// notifiers, each with its own listening connection. Every subscriber must
// receive every message exactly once, before and after its listening
// connection is killed and replaced.
func TestNotifierFanOut(t *testing.T) {
	const (
		notifiers   = 4
		subscribers = 100
		publishers  = 10
		messages    = 1000
		afterKill   = 100
		channel     = "saige.stress"
	)
	pool := pgPool(t, 32)
	ctx := testContext(t, 2*time.Minute)

	var reconnects atomic.Int32
	ns := make([]*postgres.Notifier, notifiers)
	for i := range ns {
		ns[i] = postgres.NewNotifier(pool, postgres.NotifierOptions{
			MinBackoff:      10 * time.Millisecond,
			MaxBackoff:      100 * time.Millisecond,
			ApplicationName: "saige-stress-listener",
			OnReconnect:     func() { reconnects.Add(1) },
			Logger:          quietLogger,
		})
		t.Cleanup(func() { _ = ns[i].Close() })
	}
	pub := postgres.NewNotifier(pool, postgres.NotifierOptions{ApplicationName: "saige-stress-publisher"})
	t.Cleanup(func() { _ = pub.Close() })

	// Each subscriber counts the payloads it receives.
	counts := make([]map[string]int, subscribers)
	var mu sync.Mutex
	var wg sync.WaitGroup
	cancels := make([]func(), subscribers)
	for i := range subscribers {
		ch, cancel, err := ns[i%notifiers].Subscribe(ctx, channel)
		if err != nil {
			t.Fatal(err)
		}
		cancels[i] = cancel
		counts[i] = map[string]int{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range ch {
				mu.Lock()
				counts[i][string(n.Payload)]++
				mu.Unlock()
			}
		}()
	}

	publish := func(prefix string, total int) time.Duration {
		start := time.Now()
		var pwg sync.WaitGroup
		errs := make(chan error, publishers)
		for p := range publishers {
			pwg.Add(1)
			go func() {
				defer pwg.Done()
				for k := p; k < total; k += publishers {
					if err := pub.Publish(ctx, channel, []byte(prefix+strconv.Itoa(k))); err != nil {
						errs <- err
						return
					}
				}
			}()
		}
		pwg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		return time.Since(start)
	}
	// waitDelivered waits until every subscriber holds want payloads.
	waitDelivered := func(want int) time.Duration {
		start := time.Now()
		deadline := start.Add(time.Minute)
		for {
			mu.Lock()
			short := -1
			for i, c := range counts {
				if len(c) < want {
					short = i
					break
				}
			}
			mu.Unlock()
			if short < 0 {
				return time.Since(start)
			}
			if time.Now().After(deadline) {
				mu.Lock()
				got := len(counts[short])
				mu.Unlock()
				t.Fatalf("subscriber %d received %d of %d messages", short, got, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	pubTime := publish("m", messages)
	drain := waitDelivered(messages)
	t.Logf("fan-out: %d publishes in %v (%.0f/s); %d deliveries complete %v after the last publish",
		messages, pubTime.Round(time.Millisecond), float64(messages)/pubTime.Seconds(),
		messages*subscribers, drain.Round(time.Millisecond))

	// Kill every listening connection. Publishes during the gap may be
	// lost, as documented, so publish only after each notifier reconnects.
	var killed int
	err := pool.QueryRow(ctx,
		`SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
		 WHERE application_name = 'saige-stress-listener' AND datname = current_database()`,
	).Scan(&killed)
	if err != nil || killed != notifiers {
		t.Fatalf("terminated %d listeners, want %d: %v", killed, notifiers, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for reconnects.Load() < notifiers {
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d notifiers reconnected", reconnects.Load(), notifiers)
		}
		time.Sleep(10 * time.Millisecond)
	}
	publish("r", afterKill)
	waitDelivered(messages + afterKill)

	for _, cancel := range cancels {
		cancel()
	}
	wg.Wait()
	for i, c := range counts {
		for payload, n := range c {
			if n != 1 {
				t.Fatalf("subscriber %d received %q %d times", i, payload, n)
			}
		}
	}
}

// TestCacheCoherence runs several two-level caches, each standing for one
// process, over one Postgres cache store, with concurrent gets, sets and
// invalidations on a small key space. Once writers stop and invalidations
// arrive, every process must read what the shared store holds.
func TestCacheCoherence(t *testing.T) {
	const (
		processes = 4
		workers   = 8 // per process
		ops       = 300
		keys      = 4
		channel   = "saige.stress.cache"
	)
	pool := pgPool(t, 64)
	ctx := testContext(t, 2*time.Minute)
	store := postgres.NewCacheStore(pool, postgres.CacheStoreOptions{})

	caches := make([]*notify.Cache[[]byte], processes)
	for i := range caches {
		n := postgres.NewNotifier(pool, postgres.NotifierOptions{Logger: quietLogger})
		t.Cleanup(func() { _ = n.Close() })
		c, err := notify.NewCache(ctx, notify.CacheConfig[[]byte]{
			Local:    memcache.New[[]byte](),
			Shared:   store,
			Notifier: n,
			Channel:  channel,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		caches[i] = c
	}

	var latMu sync.Mutex
	var latencies []time.Duration
	var wg sync.WaitGroup
	errs := make(chan error, processes*workers)
	start := time.Now()
	for p, c := range caches {
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				local := make([]time.Duration, 0, ops)
				for i := range ops {
					key := "k" + strconv.Itoa(rand.IntN(keys)) //nolint:gosec // workload shape only
					began := time.Now()
					var err error
					switch r := rand.IntN(10); { //nolint:gosec // workload shape only
					case r < 6:
						_, _, err = c.Get(ctx, key)
					case r < 9:
						err = c.Set(ctx, key, fmt.Appendf(nil, "p%d-w%d-%d", p, w, i), 0)
					default:
						err = c.Invalidate(ctx, key)
					}
					if err != nil {
						errs <- err
						return
					}
					local = append(local, time.Since(began))
				}
				latMu.Lock()
				latencies = append(latencies, local...)
				latMu.Unlock()
			}()
		}
	}
	wg.Wait()
	wall := time.Since(start)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	logLatency(t, "cache ops (60% get, 30% set, 10% invalidate)", latencies, wall)

	// Invalidations travel asynchronously; give them time to land, then
	// compare every process's view with the store.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var stale []string
		for k := range keys {
			key := "k" + strconv.Itoa(k)
			want, wantOK, err := store.Get(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			for p, c := range caches {
				got, ok, err := c.Get(ctx, key)
				if err != nil {
					t.Fatal(err)
				}
				if ok != wantOK || string(got) != string(want) {
					stale = append(stale, fmt.Sprintf("process %d %s: local %q, store %q", p, key, got, want))
				}
			}
		}
		if len(stale) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d stale entries after writers stopped:\n%s", len(stale), strings.Join(stale, "\n"))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stressVec is a 768-dimension embedding pointing along axis k, so document
// k is the nearest neighbour of its own vector.
func stressVec(k int) []float32 {
	v := make([]float32, 768)
	v[k%768] = 1
	v[(k+1)%768] = 0.05
	return v
}

// stressWord is a keyword unique to document k, made of letters only so
// every tokenizer keeps it whole.
func stressWord(k int) string {
	var b strings.Builder
	b.WriteString("zq")
	for range 4 {
		b.WriteByte(byte('a' + k%26))
		k /= 26
	}
	return b.String()
}

// TestPgstoreIngestAndHybridSearch ingests documents from several writers
// while readers run vector and keyword searches and fuse them. No search may
// fail, and once ingest ends every document must rank first for its own
// vector and keyword.
func TestPgstoreIngestAndHybridSearch(t *testing.T) {
	const (
		docs    = 300
		writers = 8
		readers = 8
	)
	pool := pgPool(t, 32)
	ctx := testContext(t, 3*time.Minute)
	s := pgstore.NewStore(pool, quietLogger)

	doc := func(k int) *ragtypes.Document {
		id := fmt.Sprintf("doc-%04d", k)
		now := time.Now().UTC().Truncate(time.Microsecond)
		return &ragtypes.Document{
			UUID: id, SourceURI: "mem://" + id, Fingerprint: "fp-" + id, Title: id,
			Sections: []ragtypes.Section{{
				UUID: id + "-s", DocumentUUID: id,
				Variants: []ragtypes.ContentVariant{{
					UUID: id + "-v", SectionUUID: id + "-s", ContentType: ragtypes.ContentText,
					Text:      "stress document " + stressWord(k) + " about load",
					Embedding: stressVec(k),
				}},
			}},
			CreatedAt: now, UpdatedAt: now,
		}
	}
	hybrid := func(k int) ([]ragtypes.SearchHit, error) {
		opts := &ragtypes.SearchOptions{Limit: 5}
		vec, err := s.SearchByEmbedding(ctx, stressVec(k), opts)
		if err != nil {
			return nil, fmt.Errorf("vector search: %w", err)
		}
		kw, err := s.SearchByKeyword(ctx, stressWord(k), opts)
		if err != nil {
			return nil, fmt.Errorf("keyword search: %w", err)
		}
		return fusion.RRF{}.Fuse([]ragtypes.RankedList{
			{Retriever: "vector", Hits: vec},
			{Retriever: "bm25", Hits: kw},
		}, ragtypes.FuseOptions{}), nil
	}

	var next atomic.Int32
	var ingested atomic.Int32
	var ingestMu sync.Mutex
	var ingestLat, searchLat []time.Duration
	errs := make(chan error, writers+readers)
	var wg sync.WaitGroup
	start := time.Now()
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				k := int(next.Add(1)) - 1
				if k >= docs {
					return
				}
				began := time.Now()
				if err := s.CreateDocument(ctx, doc(k)); err != nil {
					errs <- fmt.Errorf("ingest %d: %w", k, err)
					return
				}
				d := time.Since(began)
				ingested.Add(1)
				ingestMu.Lock()
				ingestLat = append(ingestLat, d)
				ingestMu.Unlock()
			}
		}()
	}
	var readerWG sync.WaitGroup
	for range readers {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			for ingested.Load() < docs {
				began := time.Now()
				if _, err := hybrid(rand.IntN(docs)); err != nil { //nolint:gosec // workload shape only
					errs <- err
					return
				}
				d := time.Since(began)
				ingestMu.Lock()
				searchLat = append(searchLat, d)
				ingestMu.Unlock()
			}
		}()
	}
	wg.Wait()
	ingestWall := time.Since(start)
	readerWG.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	logLatency(t, "ingest under search load", ingestLat, ingestWall)
	if len(searchLat) > 0 {
		logLatency(t, "hybrid search under ingest load", searchLat, ingestWall)
	}

	for k := range docs {
		hits, err := hybrid(k)
		if err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("doc-%04d-v", k)
		if len(hits) == 0 {
			t.Fatalf("doc %d: no hits", k)
		}
		best := hits[0]
		for _, h := range hits[1:] {
			if h.Score > best.Score {
				best = h
			}
		}
		if best.Variant.UUID != want {
			t.Fatalf("doc %d: top fused hit %s, want %s", k, best.Variant.UUID, want)
		}
	}
}

// durableProvider calls one tool on a conversation's first turn and answers
// once the result is in. Every call is counted per run.
type durableProvider struct {
	run   string
	tool  string
	calls *sync.Map // run ID -> *atomic.Int32
}

func (p durableProvider) Stream(_ context.Context, req types.Request) (<-chan types.Delta, error) {
	messages := req.Messages
	counter(p.calls, p.run).Add(1)
	deltas := agenttest.TextResponse("done " + p.run)
	if !hasResult(messages) {
		deltas = agenttest.ToolCallResponse("call-1", p.tool, map[string]any{})
	}
	ch := make(chan types.Delta, len(deltas))
	for _, d := range deltas {
		ch <- d
	}
	close(ch)
	return ch, nil
}

func hasResult(messages []types.Message) bool {
	for _, m := range messages {
		if sm, ok := m.(types.SystemMessage); ok {
			for _, c := range sm.Parts {
				if _, ok := c.(types.ToolResultPart); ok {
					return true
				}
			}
		}
	}
	return false
}

func counter(m *sync.Map, key string) *atomic.Int32 {
	v, _ := m.LoadOrStore(key, new(atomic.Int32))
	return v.(*atomic.Int32)
}

// TestDurableWorkersCompete runs many durable agent runs on one Postgres
// queue served by several workers. Half the runs call a tool that needs
// approval and park until the host decides. Every tool must run exactly once
// per run, and every model step must run once.
func TestDurableWorkersCompete(t *testing.T) {
	const (
		runs    = 40
		workers = 4
	)
	dbURL := pgDatabase(t)
	ctx := testContext(t, 3*time.Minute)
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ddl := pgledger.RecommendedDDL(pgledger.DefaultMapping()) + "\n" + pgqueue.RecommendedDDL(pgqueue.DefaultMapping())
	if _, err := pool.Exec(ctx, ddl); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	lgr, err := pgledger.New(pool, pgledger.DefaultMapping())
	if err != nil {
		t.Fatal(err)
	}
	q, err := pgqueue.New(pool, pgqueue.DefaultMapping(), pgqueue.WithPollInterval(10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	e := duraturo.New(lgr, q)

	var modelCalls, toolRuns sync.Map
	wf := e.Register("stress", func(runID string) *agent.Agent {
		tool := "read"
		if strings.HasSuffix(runID, "-approve") {
			tool = "write"
		}
		run := func(context.Context, map[string]any) (string, error) {
			counter(&toolRuns, runID).Add(1)
			return "ok", nil
		}
		write := types.WithMarkers(&types.ToolFunc{Def: types.ToolDef{Name: "write"}, Fn: run}, types.Marker{Kind: "approval"})
		read := &types.ToolFunc{Def: types.ToolDef{Name: "read"}, Fn: run}
		return must.Get(agent.New(agent.Config{
			Provider:     durableProvider{run: runID, tool: tool, calls: &modelCalls},
			SystemPrompt: "stress",
			Tools:        types.NewToolRegistry(write, read),
		}))
	})

	workerCtx, stopWorkers := context.WithCancel(context.Background())
	var workerWG sync.WaitGroup
	for range workers {
		w := e.Worker(
			worker.WithLeaseTTL(time.Second),
			worker.WithJanitorEvery(50*time.Millisecond),
			worker.WithConcurrency(2),
			worker.WithLogger(quietLogger),
		)
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			_ = w.Run(workerCtx)
		}()
	}
	t.Cleanup(func() {
		stopWorkers()
		workerWG.Wait()
	})

	ids := make([]string, runs)
	for i := range ids {
		ids[i] = fmt.Sprintf("run-%02d", i)
		if i%2 == 0 {
			ids[i] += "-approve"
		}
	}
	latencies := make([]time.Duration, runs)
	errs := make(chan error, runs)
	var wg sync.WaitGroup
	start := time.Now()
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			began := time.Now()
			input := []types.Message{types.UserMsg(types.Text("go " + id))}
			_, err := e.Run(ctx, wf, id, input)
			if strings.HasSuffix(id, "-approve") {
				if !errors.Is(err, types.ErrSuspended) {
					errs <- fmt.Errorf("%s: Run = %v, want ErrSuspended", id, err)
					return
				}
				if err := e.Decide(ctx, id, "marker/call-1", "decision-"+id, types.ApprovalDecision{Approved: true}); err != nil {
					errs <- fmt.Errorf("%s: Decide: %w", id, err)
					return
				}
				_, err = e.Wait(ctx, id)
			}
			if err != nil {
				errs <- fmt.Errorf("%s: %w", id, err)
				return
			}
			latencies[i] = time.Since(began)
		}()
	}
	wg.Wait()
	wall := time.Since(start)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	logLatency(t, fmt.Sprintf("durable runs on %d workers (half with an approval)", workers), latencies, wall)

	for _, id := range ids {
		if n := counter(&toolRuns, id).Load(); n != 1 {
			t.Errorf("%s: tool ran %d times, want 1", id, n)
		}
		if n := counter(&modelCalls, id).Load(); n != 2 {
			t.Errorf("%s: model called %d times, want 2", id, n)
		}
	}
}
