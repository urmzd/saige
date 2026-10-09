package harness

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/store"
	"github.com/urmzd/saige/eval/store/memstore"
)

// failingServer answers every chat request, failing those whose last
// message contains FAIL, and counts the requests.
func failingServer(t *testing.T, calls *atomic.Int64, delay time.Duration, inflight, peak *atomic.Int64) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if inflight != nil {
			n := inflight.Add(1)
			defer inflight.Add(-1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
		}
		req := decodeChatRequest(t, r)
		// Failing requests answer at once and successful ones take delay, so a
		// failure always lands before a concurrent success finishes.
		if strings.Contains(req.Messages[len(req.Messages)-1].Content, "FAIL") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		time.Sleep(delay)
		writeChatResponse(t, w, "doc", 3, 2)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestRunnerConcurrency(t *testing.T) {
	tests := []struct {
		name        string
		concurrency int
		wantPeak    int64
	}{
		{"serial", 1, 1},
		{"bounded", 2, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for _, id := range []string{"a", "b", "c", "d"} {
				writeTurns(t, filepath.Join(root, id), id+" synth")
			}
			var calls, inflight, peak atomic.Int64
			server := failingServer(t, &calls, 80*time.Millisecond, &inflight, &peak)
			scripts, err := LoadCorpus(root)
			if err != nil {
				t.Fatal(err)
			}
			results := memstore.New()
			runner := &Runner{
				Client:      NewClient(server.URL, "k", "mock"),
				Flows:       []Flow{BaseFlow{}},
				Concurrency: tt.concurrency,
				Results:     results,
				RunID:       "run",
			}
			if err := runner.Run(context.Background(), scripts); err != nil {
				t.Fatal(err)
			}
			if peak.Load() != tt.wantPeak {
				t.Errorf("peak in-flight = %d, want %d", peak.Load(), tt.wantPeak)
			}
			units, err := results.Units(context.Background(), "run", store.UnitFilter{})
			if err != nil {
				t.Fatal(err)
			}
			var keys []string
			for _, u := range units {
				keys = append(keys, u.Key)
			}
			if got := strings.Join(keys, ","); got != "a/0@base,b/0@base,c/0@base,d/0@base" {
				t.Errorf("units recorded out of script order: %s", got)
			}
		})
	}
}

func TestRunnerConcurrentStopsLaunchingOnError(t *testing.T) {
	root := t.TempDir()
	writeTurns(t, filepath.Join(root, "a"), "a FAIL synth")
	for _, id := range []string{"b", "c", "d", "e"} {
		writeTurns(t, filepath.Join(root, id), id+" synth")
	}
	var calls atomic.Int64
	server := failingServer(t, &calls, 50*time.Millisecond, nil, nil)
	scripts, err := LoadCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Client: NewClient(server.URL, "k", "mock"), Flows: []Flow{BaseFlow{}}, Concurrency: 2}
	if err := runner.Run(context.Background(), scripts); err == nil || !strings.Contains(err.Error(), "a:") {
		t.Fatalf("err = %v, want a to fail", err)
	}
	// a and b start together; a fails, so no script after b starts.
	if calls.Load() > 2 {
		t.Errorf("calls = %d, want at most 2", calls.Load())
	}
}

func TestRunnerResume(t *testing.T) {
	root := t.TempDir()
	writeTurns(t, filepath.Join(root, "a"), "a synth", "a edit")
	writeTurns(t, filepath.Join(root, "b"), "b FAIL synth", "b edit")
	var calls atomic.Int64
	server := failingServer(t, &calls, 0, nil, nil)
	scripts, err := LoadCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	results := memstore.New()
	ctx := context.Background()
	first := &Runner{Client: NewClient(server.URL, "k", "mock"), Flows: []Flow{BaseFlow{}}, ContinueOnError: true, Results: results, RunID: "first"}
	if err := first.Run(ctx, scripts); err == nil {
		t.Fatal("first run should fail on b")
	}

	// Fix b and resume: only b runs again, and the new run holds both.
	writeFixture(t, filepath.Join(root, "b", "turn-0.md"), "b synth")
	scripts, err = LoadCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	calls.Store(0)
	second := &Runner{Client: NewClient(server.URL, "k", "mock"), Flows: []Flow{BaseFlow{}}, Results: results, RunID: "second", Resume: "first"}
	plan, err := second.Plan(ctx, scripts)
	if err != nil {
		t.Fatal(err)
	}
	if plan[0].Action != PlanResume || plan[1].Action != PlanRun || plan[1].Calls != 2 {
		t.Errorf("plan = %+v", plan)
	}
	if err := second.Run(ctx, scripts); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2 (only b)", calls.Load())
	}
	run, err := results.GetRun(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != eval.RunSucceeded || run.Units != 4 || run.SubjectErrors != 0 {
		t.Errorf("resumed run = %+v", run)
	}

	otherModel := &Runner{Client: NewClient(server.URL, "k", "other"), Flows: []Flow{BaseFlow{}}, Results: results, RunID: "third", Resume: "first"}
	calls.Store(0)
	if err := otherModel.Run(ctx, scripts); err == nil || !strings.Contains(err.Error(), "other") {
		t.Errorf("resume with a different model = %v, want a model mismatch error", err)
	}
	if calls.Load() != 0 {
		t.Errorf("resume with a different model made %d calls", calls.Load())
	}

	missing := &Runner{Client: NewClient(server.URL, "k", "mock"), Flows: []Flow{BaseFlow{}}, Resume: "first"}
	if err := missing.Run(ctx, scripts); err == nil {
		t.Error("resume without a store should fail")
	}
	unknown := &Runner{Client: NewClient(server.URL, "k", "mock"), Flows: []Flow{BaseFlow{}}, Results: results, Resume: "nope"}
	if err := unknown.Run(ctx, scripts); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("resume of an unknown run = %v, want ErrNotFound", err)
	}
}

func TestRunnerAssert(t *testing.T) {
	tests := []struct {
		name      string
		prompt    string
		assert    []eval.Assertion
		withStore bool
		wantFail  bool
	}{
		{"passing gate", "edit", []eval.Assertion{{Metric: MetricTurnSucceeded, Op: eval.GTE, Threshold: 1}}, true, false},
		{"failing gate", "FAIL edit", []eval.Assertion{{Metric: MetricTurnSucceeded, Op: eval.GTE, Threshold: 1}}, true, true},
		{"aggregate gate without a store", "FAIL edit", []eval.Assertion{{Metric: MetricTurnSucceeded, Op: eval.GTE, Threshold: 0.5, Scope: eval.OnAggregate}}, false, false},
		{"misspelled metric fails", "edit", []eval.Assertion{{Metric: "turn_succeedd", Op: eval.GTE, Threshold: 1}}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeTurns(t, filepath.Join(root, "a"), "a synth", tt.prompt)
			var calls atomic.Int64
			server := failingServer(t, &calls, 0, nil, nil)
			scripts, err := LoadCorpus(root)
			if err != nil {
				t.Fatal(err)
			}
			var gated *eval.SuiteResult
			runner := &Runner{
				Client:  NewClient(server.URL, "k", "mock"),
				Flows:   []Flow{BaseFlow{}},
				Assert:  tt.assert,
				RunID:   "gated",
				OnGated: func(s *eval.SuiteResult) { gated = s },
			}
			results := memstore.New()
			if tt.withStore {
				runner.Results = results
			}
			err = runner.Run(context.Background(), scripts)
			if got := errors.Is(err, ErrAssertionsFailed); got != tt.wantFail {
				t.Fatalf("err = %v, want assertion failure %v", err, tt.wantFail)
			}
			var ae *AssertionError
			if tt.wantFail && (!errors.As(err, &ae) || len(ae.Violations) == 0) {
				t.Errorf("err = %v, want violations", err)
			}
			if gated == nil {
				t.Fatal("OnGated not called")
			}
			if !tt.withStore {
				return
			}
			run, err := results.GetRun(context.Background(), "gated")
			if err != nil {
				t.Fatal(err)
			}
			wantOutcome := eval.OutcomePassed
			if tt.wantFail {
				wantOutcome = eval.OutcomeFailed
			}
			// A failed gate does not make the run itself errored.
			if run.Status != eval.RunSucceeded || run.Outcome != wantOutcome {
				t.Errorf("run = %+v", run)
			}
		})
	}
}

func TestRunnerReuseMetrics(t *testing.T) {
	root := t.TempDir()
	writeTurns(t, filepath.Join(root, "a"), "a synth", "a edit")
	var calls atomic.Int64
	server := failingServer(t, &calls, 0, nil, nil)
	scripts, err := LoadCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	gate := []eval.Assertion{{Metric: MetricTurnSucceeded, Op: eval.GTE, Threshold: 1}}
	newRunner := func(reuse bool) *Runner {
		return &Runner{Client: NewClient(server.URL, "k", "mock"), Flows: []Flow{BaseFlow{}}, Assert: gate, ReuseMetrics: reuse}
	}
	if err := newRunner(true).Run(context.Background(), scripts); err != nil {
		t.Fatal(err)
	}
	calls.Store(0)
	tests := []struct {
		name     string
		reuse    bool
		wantFail bool
	}{
		{"skipped script is gated from its metrics", true, false},
		{"skipped script without reuse has nothing to gate", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := newRunner(tt.reuse).Run(context.Background(), scripts)
			if got := errors.Is(err, ErrAssertionsFailed); got != tt.wantFail {
				t.Errorf("err = %v, want assertion failure %v", err, tt.wantFail)
			}
			if calls.Load() != 0 {
				t.Errorf("a skipped script made %d calls", calls.Load())
			}
		})
	}
}

func TestRunnerPlan(t *testing.T) {
	root := t.TempDir()
	writeTurns(t, filepath.Join(root, "a"), "synth", "edit one", "edit two")
	writeTurns(t, filepath.Join(root, "b"), "synth")
	writeFixture(t, filepath.Join(root, "b", "metrics.json"), "{}")
	scripts, err := LoadCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		flows     []Flow
		force     bool
		wantCalls int
		wantExact bool
		wantSkipB bool
	}{
		{"base then stateless reuses synthesis", []Flow{BaseFlow{}, StatelessFlow{}}, false, 5, true, true},
		{"stateless alone synthesizes", []Flow{StatelessFlow{}}, false, 3, true, true},
		{"custom flow is not exact", []Flow{BaseFlow{}, countingFlow{}}, true, 3, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := (&Runner{Flows: tt.flows, Force: tt.force}).Plan(context.Background(), scripts)
			if err != nil {
				t.Fatal(err)
			}
			if plan[0].Calls != tt.wantCalls || plan[0].Exact != tt.wantExact || plan[0].Turns != 3 {
				t.Errorf("plan a = %+v", plan[0])
			}
			if got := plan[1].Action == PlanSkip; got != tt.wantSkipB {
				t.Errorf("plan b = %+v", plan[1])
			}
		})
	}
	if _, err := os.Stat(filepath.Join(root, "a", "outputs")); err == nil {
		t.Error("Plan must not write outputs")
	}
}

func TestRunnerReuseMetricsRequiresSameModel(t *testing.T) {
	root := t.TempDir()
	writeTurns(t, filepath.Join(root, "a"), "a synth")
	var calls atomic.Int64
	server := failingServer(t, &calls, 0, nil, nil)
	scripts, err := LoadCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Runner{Client: NewClient(server.URL, "k", "model-a"), Flows: []Flow{BaseFlow{}}}).Run(context.Background(), scripts); err != nil {
		t.Fatal(err)
	}
	gate := []eval.Assertion{{Metric: MetricTurnSucceeded, Op: eval.GTE, Threshold: 1}}
	tests := []struct {
		model    string
		wantFail bool
	}{
		{"model-a", false},
		{"model-b", true},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			runner := &Runner{Client: NewClient(server.URL, "k", tt.model), Flows: []Flow{BaseFlow{}}, Assert: gate, ReuseMetrics: true}
			err := runner.Run(context.Background(), scripts)
			if got := errors.Is(err, ErrAssertionsFailed); got != tt.wantFail {
				t.Errorf("err = %v, want assertion failure %v", err, tt.wantFail)
			}
		})
	}
}

func TestRunnerReuseMetricsRequiresEveryFlow(t *testing.T) {
	root := t.TempDir()
	writeTurns(t, filepath.Join(root, "a"), "a synth", "a edit")
	var calls atomic.Int64
	server := failingServer(t, &calls, 0, nil, nil)
	scripts, err := LoadCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Runner{Client: NewClient(server.URL, "k", "mock"), Flows: []Flow{BaseFlow{}}}).Run(context.Background(), scripts); err != nil {
		t.Fatal(err)
	}
	gate := []eval.Assertion{{Metric: MetricTurnSucceeded, Op: eval.GTE, Threshold: 1}}
	tests := []struct {
		name     string
		flows    []Flow
		wantFail bool
	}{
		{"same flows reuse", []Flow{BaseFlow{}}, false},
		{"a flow missing from the metrics is not reused", []Flow{BaseFlow{}, StatelessFlow{}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &Runner{Client: NewClient(server.URL, "k", "mock"), Flows: tt.flows, Assert: gate, ReuseMetrics: true}
			err := runner.Run(context.Background(), scripts)
			if got := errors.Is(err, ErrAssertionsFailed); got != tt.wantFail {
				t.Errorf("err = %v, want assertion failure %v", err, tt.wantFail)
			}
		})
	}
}
