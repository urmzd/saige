package harness

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/store/memstore"
	"github.com/urmzd/saige/internal/must"
)

// outageServer answers chat requests, refusing those whose last message
// contains AUTH with 401 (an infrastructure failure the client does not
// retry) and those containing FAIL with 400 (a real failure).
func outageServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeChatRequest(t, r)
		last := req.Messages[len(req.Messages)-1].Content
		switch {
		case strings.Contains(last, "AUTH"):
			http.Error(w, "invalid key", http.StatusUnauthorized)
		case strings.Contains(last, "FAIL"):
			http.Error(w, "bad request", http.StatusBadRequest)
		default:
			writeChatResponse(t, w, "doc", 3, 2)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestRunnerInconclusive(t *testing.T) {
	gate := []eval.Assertion{{Metric: MetricTurnSucceeded, Op: eval.GTE, Threshold: 1}}
	tests := []struct {
		name             string
		scripts          map[string][]string
		assert           []eval.Assertion
		maxInconclusive  float64
		wantInconclusive bool
		wantAssertFail   bool
		wantErr          bool
		wantStatus       eval.RunStatus
		wantOutcome      eval.Outcome
		wantSubjectErrs  int
	}{
		{
			name:             "script lost to an outage",
			scripts:          map[string][]string{"a": {"a synth"}, "b": {"AUTH synth"}},
			wantInconclusive: true, wantErr: true,
			wantStatus: eval.RunErrored, wantSubjectErrs: 1,
		},
		{
			name:            "outage within the tolerance",
			scripts:         map[string][]string{"a": {"a synth"}, "b": {"AUTH synth"}},
			maxInconclusive: 0.5,
			wantStatus:      eval.RunSucceeded, wantSubjectErrs: 1,
		},
		{
			name:    "a real failure wins",
			scripts: map[string][]string{"a": {"a synth"}, "b": {"AUTH synth"}, "c": {"FAIL synth"}},
			wantErr: true, wantStatus: eval.RunErrored, wantSubjectErrs: 2,
		},
		{
			name:             "edit turn lost to an outage leaves the gate inconclusive",
			scripts:          map[string][]string{"a": {"a synth", "AUTH edit"}},
			assert:           gate,
			wantInconclusive: true, wantErr: true,
			wantStatus: eval.RunErrored, wantOutcome: eval.OutcomeInconclusive, wantSubjectErrs: 0,
		},
		{
			name:           "failed edit turn fails the gate",
			scripts:        map[string][]string{"a": {"a synth", "FAIL edit"}, "b": {"AUTH synth"}},
			assert:         gate,
			wantAssertFail: true, wantErr: true,
			wantStatus: eval.RunErrored, wantOutcome: eval.OutcomeFailed, wantSubjectErrs: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for id, prompts := range tt.scripts {
				writeTurns(t, filepath.Join(root, id), prompts...)
			}
			scripts, err := LoadCorpus(root)
			if err != nil {
				t.Fatal(err)
			}
			results := memstore.New()
			runner := must.Get(New(Config{
				Client:          NewClient(outageServer(t).URL, "k", "mock"),
				Flows:           []Flow{BaseFlow{}},
				ContinueOnError: true,
				Results:         results,
				RunID:           "run",
				Assert:          tt.assert,
				MaxInconclusive: tt.maxInconclusive,
			}))
			err = runner.Run(context.Background(), scripts)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error %v", err, tt.wantErr)
			}
			if got := errors.Is(err, ErrInconclusive); got != tt.wantInconclusive {
				t.Fatalf("err = %v, want inconclusive %v", err, tt.wantInconclusive)
			}
			if got := errors.Is(err, ErrAssertionsFailed); got != tt.wantAssertFail {
				t.Fatalf("err = %v, want assertion failure %v", err, tt.wantAssertFail)
			}
			run, err := results.GetRun(context.Background(), "run")
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != tt.wantStatus || run.Outcome != tt.wantOutcome || run.Inconclusive != 1 {
				t.Fatalf("run status %s, outcome %q, inconclusive %d; want %s, %q, 1", run.Status, run.Outcome, run.Inconclusive, tt.wantStatus, tt.wantOutcome)
			}
			if run.SubjectErrors != tt.wantSubjectErrs {
				t.Fatalf("subject errors %d, want %d", run.SubjectErrors, tt.wantSubjectErrs)
			}
		})
	}
}

func TestInconclusiveTurnObservation(t *testing.T) {
	reason := "request failed: 429"
	obs := turnObservation(Script{ID: "a"}, "base", TurnResult{Turn: 1, Failed: true, FailureReason: &reason, Inconclusive: true})
	sc := obs.Scores[0]
	if sc.Name != MetricTurnSucceeded || sc.Error != reason || !sc.Inconclusive || sc.Value != 0 {
		t.Fatalf("turn_succeeded = %+v", sc)
	}
	if !obs.Inconclusive() {
		t.Fatal("observation not inconclusive")
	}

	// The flag survives metrics.json, so reused metrics keep it.
	data, err := json.Marshal(TurnResult{Turn: 1, Failed: true, FailureReason: &reason, Inconclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	var back TurnResult
	if err := json.Unmarshal(data, &back); err != nil || !back.Inconclusive || back.Extra != nil {
		t.Fatalf("round trip = %+v, %v (%s)", back, err, data)
	}
	if _, err := json.Marshal(TurnResult{Extra: map[string]any{"inconclusive": true}}); err == nil {
		t.Fatal("extra key inconclusive did not collide")
	}
}
