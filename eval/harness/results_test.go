package harness

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/store"
	"github.com/urmzd/saige/eval/store/memstore"
)

func TestScriptObservations(t *testing.T) {
	reason := "request failed: 400"
	script := Script{ID: "s1", Format: "text/markdown", Turns: []Turn{{Index: 0, Prompt: "synth"}, {Index: 1, Prompt: "edit"}}}
	results := map[string]FlowResult{
		"base": {
			Turn0: TurnMetrics{InputTokens: 10, OutputTokens: 5, LatencyMS: 7, ArtifactBytes: 30},
			Turns: []TurnResult{{Turn: 1, Edit: "edit", InputTokens: 20, OutputTokens: 6, LatencyMS: 9, OutputBytes: 31}},
		},
		"stateless": {
			Turn0: TurnMetrics{InputTokens: 10, OutputTokens: 5, LatencyMS: 7, ArtifactBytes: 30},
			Turns: []TurnResult{{Turn: 1, Edit: "edit", LatencyMS: 3, Failed: true, FailureReason: &reason}},
		},
	}
	got := ScriptObservations(script, []Flow{BaseFlow{}, StatelessFlow{}, countingFlow{}}, results)

	tests := []struct {
		key       string
		succeeded float64
		reason    string
		latency   int64
		inTokens  int
	}{
		{"s1/0@base", 1, "", 7, 10},
		{"s1/1@base", 1, "", 9, 20},
		{"s1/0@stateless", 1, "", 7, 10},
		{"s1/1@stateless", 0, reason, 3, 0},
	}
	if len(got) != len(tests) {
		t.Fatalf("got %d observations, want %d", len(got), len(tests))
	}
	for i, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			r := got[i]
			if k := eval.UnitKey(r.Observation); k != tt.key {
				t.Fatalf("key = %q, want %q", k, tt.key)
			}
			if r.Observation.Labels["format"] != "text/markdown" {
				t.Errorf("labels = %v", r.Observation.Labels)
			}
			if r.Observation.Timing.TotalMs != tt.latency || r.Observation.Timing.InputTokens != tt.inTokens {
				t.Errorf("timing = %+v", r.Observation.Timing)
			}
			s := r.Scores[0]
			if s.Name != MetricTurnSucceeded || s.Value != tt.succeeded || s.Reason != tt.reason {
				t.Errorf("turn_succeeded = %+v", s)
			}
			if len(r.Scores) != 5 {
				t.Errorf("scores = %v", r.Scores)
			}
		})
	}
}

func TestRunnerRecordsRun(t *testing.T) {
	root := t.TempDir()
	writeTurns(t, filepath.Join(root, "a"), "a synth", "a FAIL edit")
	writeTurns(t, filepath.Join(root, "b"), "b FAIL synth", "b edit")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeChatRequest(t, r)
		last := req.Messages[len(req.Messages)-1].Content
		if strings.Contains(last, "FAIL") {
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return
		}
		writeChatResponse(t, w, "doc", 10, 5)
	}))
	defer server.Close()

	scripts, err := LoadCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	results := memstore.New()
	var saved eval.RunRecord
	runner := &Runner{
		Client:          NewClient(server.URL, "test-key", "mock"),
		Flows:           []Flow{BaseFlow{}, StatelessFlow{}},
		ContinueOnError: true,
		Results:         results,
		RunID:           "run-1",
		Provenance:      eval.Provenance{GitCommit: "abc", Models: []string{"judge"}},
		OnSaved:         func(r eval.RunRecord) { saved = r },
	}
	runErr := runner.Run(context.Background(), scripts)
	if runErr == nil || !strings.Contains(runErr.Error(), "b:") {
		t.Fatalf("Run err = %v, want b to fail", runErr)
	}

	ctx := context.Background()
	run, err := results.GetRun(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID != "run-1" {
		t.Errorf("OnSaved got %+v", saved)
	}
	// a: two turns in each of two flows; b: one failed-script unit.
	if run.Suite != DefaultSuite || run.Status != eval.RunErrored || run.Units != 5 || run.SubjectErrors != 1 {
		t.Fatalf("run = %+v", run)
	}
	if run.Provenance.GitCommit != "abc" || strings.Join(run.Provenance.Models, ",") != "judge,mock" {
		t.Errorf("provenance = %+v", run.Provenance)
	}
	if len(runner.Provenance.Models) != 1 {
		t.Errorf("Run changed the runner's provenance: %v", runner.Provenance.Models)
	}
	if got := run.Aggregate[MetricTurnSucceeded]; got != 0.5 {
		t.Errorf("turn_succeeded mean = %v, want 0.5", got)
	}
	units, err := results.Units(ctx, "run-1", store.UnitFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, u := range units {
		keys = append(keys, u.Key)
	}
	want := "a/0@base,a/1@base,a/0@stateless,a/1@stateless,b/0@base"
	if strings.Join(keys, ",") != want {
		t.Errorf("unit keys = %v, want %s", keys, want)
	}
	if msg := eval.SubjectError(units[4].Observation); !strings.Contains(msg, "400") {
		t.Errorf("failed script error = %q", msg)
	}
}

func TestRunnerRecordsCanceledRun(t *testing.T) {
	root := t.TempDir()
	writeTurns(t, filepath.Join(root, "a"), "a synth")
	scripts, err := LoadCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results := memstore.New()
	runner := &Runner{
		Client:  NewClient("http://127.0.0.1:0", "k", "mock"),
		Flows:   []Flow{BaseFlow{}},
		Results: results,
		RunID:   "canceled",
	}
	if err := runner.Run(ctx, scripts); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", err)
	}
	run, err := results.GetRun(context.Background(), "canceled")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != eval.RunCanceled || run.Units != 0 {
		t.Errorf("run = %+v", run)
	}
}

func TestRunnerReportsSaveFailure(t *testing.T) {
	results := memstore.New()
	if err := results.CreateRun(context.Background(), eval.RunRecord{ID: "taken", Suite: DefaultSuite}); err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Client: NewClient("http://127.0.0.1:0", "k", "mock"), Results: results, RunID: "taken"}
	err := runner.Run(context.Background(), nil)
	if !errors.Is(err, store.ErrRunExists) {
		t.Fatalf("Run err = %v, want ErrRunExists", err)
	}
}
