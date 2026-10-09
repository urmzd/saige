package harness

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func writeTurns(t *testing.T, dir string, prompts ...string) {
	t.Helper()
	writeFixture(t, filepath.Join(dir, "system.md"), "You produce Markdown.")
	for i, p := range prompts {
		writeFixture(t, filepath.Join(dir, "turn-"+string(rune('0'+i))+".md"), p)
	}
}

func TestRunnerContinueOnError(t *testing.T) {
	root := t.TempDir()
	// a: turn 2 fails, the flow continues. b: turn 0 fails, the experiment
	// fails. c: everything succeeds.
	writeTurns(t, filepath.Join(root, "a"), "a synth", "a edit one", "a FAIL edit two", "a edit three")
	writeTurns(t, filepath.Join(root, "b"), "b FAIL synth", "b edit")
	writeTurns(t, filepath.Join(root, "c"), "c synth", "c edit")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeChatRequest(t, r)
		last := req.Messages[len(req.Messages)-1].Content
		if strings.Contains(last, "FAIL") {
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return
		}
		writeChatResponse(t, w, "doc after "+last, 10, 5)
	}))
	defer server.Close()

	experiments, err := LoadCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{
		Client:          NewClient(server.URL, "test-key", "mock"),
		Flows:           []Flow{BaseFlow{}, StatelessFlow{}},
		ContinueOnError: true,
	}
	err = runner.Run(context.Background(), experiments)
	if err == nil || !strings.Contains(err.Error(), "b:") {
		t.Fatalf("expected joined error naming b, got %v", err)
	}
	if strings.Contains(err.Error(), "a:") || strings.Contains(err.Error(), "c:") {
		t.Errorf("only b should fail, got %v", err)
	}

	for _, id := range []string{"a", "c"} {
		if _, err := os.Stat(filepath.Join(root, id, "metrics.json")); err != nil {
			t.Errorf("%s: metrics.json missing: %v", id, err)
		}
		if _, err := os.Stat(filepath.Join(root, id, ErrorFile)); err == nil {
			t.Errorf("%s: unexpected error.json", id)
		}
	}
	var expErr ExperimentError
	raw, err := os.ReadFile(filepath.Join(root, "b", ErrorFile))
	if err != nil {
		t.Fatalf("b: error.json missing: %v", err)
	}
	if err := json.Unmarshal(raw, &expErr); err != nil {
		t.Fatal(err)
	}
	if expErr.ExperimentID != "b" || expErr.Flow != "base" || !strings.Contains(expErr.Error, "400") {
		t.Errorf("error.json: %+v", expErr)
	}

	var doc DefaultMetrics
	raw, err = os.ReadFile(filepath.Join(root, "a", "metrics.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, flow := range []string{"base", "stateless"} {
		report := doc.Flows[flow]
		if len(report.PerTurn) != 3 {
			t.Fatalf("%s: per_turn has %d entries, want 3", flow, len(report.PerTurn))
		}
		failed := report.PerTurn[1]
		if !failed.Failed || failed.FailureReason == nil || !strings.HasPrefix(*failed.FailureReason, "request failed") {
			t.Errorf("%s: per_turn[1] = %+v, want a request failure", flow, failed)
		}
		if report.PerTurn[0].Failed || report.PerTurn[2].Failed {
			t.Errorf("%s: only turn 2 should fail", flow)
		}
		if report.Reliability == nil || report.Reliability.RequestFailureCount != 1 || report.Reliability.EditTurns != 3 {
			t.Errorf("%s: reliability = %+v", flow, report.Reliability)
		}
	}

	// The turn after the failure edits the last good artifact.
	data, err := os.ReadFile(filepath.Join(root, "a", "outputs", "base", "turn-3.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "a edit three") {
		t.Errorf("turn-3 artifact = %q", data)
	}
	if _, err := os.Stat(filepath.Join(root, "a", "outputs", "base", "turn-2.md")); err == nil {
		t.Error("a failed turn must not write an artifact")
	}
}

func TestRunnerStopsOnErrorByDefault(t *testing.T) {
	root := t.TempDir()
	writeTurns(t, filepath.Join(root, "a"), "a FAIL synth")
	writeTurns(t, filepath.Join(root, "b"), "b synth")

	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		req := decodeChatRequest(t, r)
		if strings.Contains(req.Messages[len(req.Messages)-1].Content, "FAIL") {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		writeChatResponse(t, w, "doc", 1, 1)
	}))
	defer server.Close()

	experiments, err := LoadCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Client: NewClient(server.URL, "k", "mock"), Flows: []Flow{BaseFlow{}}}
	if err := runner.Run(context.Background(), experiments); err == nil {
		t.Fatal("expected an error")
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 (run stops at the first failure)", calls.Load())
	}
	if _, err := os.Stat(filepath.Join(root, "a", ErrorFile)); err != nil {
		t.Errorf("error.json should be written without ContinueOnError: %v", err)
	}
}

func TestComputeReliabilityRequestFailure(t *testing.T) {
	reason := "request failed: API error 400"
	report := ComputeReliability([]TurnResult{{Failed: true, FailureReason: &reason}, {}})
	if report.RequestFailureCount != 1 || report.UnknownMissCount != 0 {
		t.Errorf("report = %+v", report)
	}
}

func TestFlowReportReliabilityRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		report FlowReport
	}{
		{name: "with reliability", report: FlowReport{Reliability: &Reliability{EditTurns: 2, MissCount: 1, RequestFailureCount: 1}}},
		{name: "custom reliability extra", report: FlowReport{Extra: map[string]any{"reliability": "custom"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(tt.report)
			if err != nil {
				t.Fatal(err)
			}
			var back FlowReport
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatal(err)
			}
			if tt.report.Reliability != nil {
				if back.Reliability == nil || !reflect.DeepEqual(*back.Reliability, *tt.report.Reliability) {
					t.Errorf("reliability: got %+v", back.Reliability)
				}
				if back.Extra != nil {
					t.Errorf("extra should be empty, got %v", back.Extra)
				}
			} else if back.Reliability != nil || back.Extra["reliability"] != "custom" {
				t.Errorf("custom extra lost: %+v", back)
			}
		})
	}
	clash := FlowReport{Reliability: &Reliability{}, Extra: map[string]any{"reliability": 1}}
	if _, err := json.Marshal(clash); err == nil {
		t.Error("expected a collision error")
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		in   string
		max  int
		want string
	}{
		{"héllo", 2, "h"},
		{"héllo", 3, "hé"},
		{"hello", 10, "hello"},
		{"  hello  ", 3, "hel"},
		{"日本語", 4, "日"},
		{"abc", 0, ""},
	}
	for _, tt := range tests {
		if got := Truncate(tt.in, tt.max); got != tt.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
		}
	}
}

func TestClientHonorsRetryAfter(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// Longer than the 1s default first backoff, so honoring it shows.
			w.Header().Set("Retry-After", "2")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		writeChatResponse(t, w, "ok", 1, 1)
	}))
	defer server.Close()

	start := time.Now()
	result, err := NewClient(server.URL, "k", "mock").Chat(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 1900*time.Millisecond {
		t.Errorf("retry came after %v, want about 2s", elapsed)
	}
	if !result.Retried || result.Text != "ok" {
		t.Errorf("result = %+v", result)
	}
}

func TestClientCapsErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, strings.Repeat("x", 10*1024), http.StatusBadRequest)
	}))
	defer server.Close()

	_, err := NewClient(server.URL, "k", "mock").Chat(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(err.Error()) > maxErrorBody+128 {
		t.Errorf("error message is %d bytes, want about %d", len(err.Error()), maxErrorBody)
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("error should say the body was truncated: %.80s", err.Error())
	}
}

func TestFlowStopsOnCancellation(t *testing.T) {
	root := t.TempDir()
	writeTurns(t, filepath.Join(root, "a"), "synth", "edit one", "edit two")
	ctx, cancel := context.WithCancel(context.Background())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeChatRequest(t, r)
		if strings.Contains(req.Messages[len(req.Messages)-1].Content, "edit one") {
			cancel()
			<-r.Context().Done()
			return
		}
		writeChatResponse(t, w, "doc", 1, 1)
	}))
	defer server.Close()

	experiments, err := LoadCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = BaseFlow{}.Run(ctx, NewClient(server.URL, "k", "mock"), experiments[0], NewFlowContext())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation should fail the flow, got %v", err)
	}
}

func TestCompareTurns(t *testing.T) {
	ok := func(turn int, in, out, ms uint64) TurnResult {
		return TurnResult{Turn: turn, InputTokens: in, OutputTokens: out, LatencyMS: ms}
	}
	fail := func(turn int, ms uint64) TurnResult {
		reason := requestFailedPrefix + ": API error 500"
		return TurnResult{Turn: turn, LatencyMS: ms, Failed: true, FailureReason: &reason}
	}
	tests := []struct {
		name  string
		base  []TurnResult
		other []TurnResult
		want  Comparison
	}{
		{
			name:  "all turns succeed",
			base:  []TurnResult{ok(1, 100, 50, 1000), ok(2, 100, 50, 1000)},
			other: []TurnResult{ok(1, 50, 25, 500), ok(2, 50, 25, 500)},
			want:  Comparison{InputTokenSavingsPct: 50, OutputTokenSavingsPct: 50, LatencySavingsPct: 50, ComparedTurns: 2},
		},
		{
			name:  "other fails turn 2",
			base:  []TurnResult{ok(1, 100, 50, 1000), ok(2, 100, 50, 1000)},
			other: []TurnResult{ok(1, 100, 50, 1000), fail(2, 5000)},
			want:  Comparison{ComparedTurns: 1, ExcludedTurns: 1},
		},
		{
			name:  "base fails turn 1",
			base:  []TurnResult{fail(1, 10), ok(2, 100, 50, 1000)},
			other: []TurnResult{ok(1, 100, 50, 1000), ok(2, 80, 40, 800)},
			want:  Comparison{InputTokenSavingsPct: 20, OutputTokenSavingsPct: 20, LatencySavingsPct: 20, ComparedTurns: 1, ExcludedTurns: 1},
		},
		{
			name:  "turn missing from one flow",
			base:  []TurnResult{ok(1, 100, 50, 1000), ok(2, 100, 50, 1000)},
			other: []TurnResult{ok(1, 100, 50, 1000)},
			want:  Comparison{ComparedTurns: 1, ExcludedTurns: 1},
		},
		{
			name:  "every turn failed",
			base:  []TurnResult{ok(1, 100, 50, 1000)},
			other: []TurnResult{fail(1, 10)},
			want:  Comparison{ExcludedTurns: 1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CompareTurns(tc.base, tc.other); got != tc.want {
				t.Fatalf("CompareTurns = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestAssembleDefaultIgnoresFailedTurnSavings(t *testing.T) {
	reason := requestFailedPrefix + ": API error 500"
	results := map[string]FlowResult{
		"base": {Turns: []TurnResult{
			{Turn: 1, InputTokens: 100, OutputTokens: 50, LatencyMS: 1000},
			{Turn: 2, InputTokens: 100, OutputTokens: 50, LatencyMS: 1000},
		}},
		"stateless": {Turns: []TurnResult{
			{Turn: 1, InputTokens: 100, OutputTokens: 50, LatencyMS: 1000},
			{Turn: 2, LatencyMS: 4000, Failed: true, FailureReason: &reason},
		}},
	}
	doc := AssembleDefault("mock", []Flow{BaseFlow{}, StatelessFlow{}}, Experiment{ID: "x"}, results)

	stateless := doc.Flows["stateless"]
	if stateless.Reliability == nil || stateless.Reliability.RequestFailureCount != 1 {
		t.Fatalf("stateless reliability = %+v", stateless.Reliability)
	}
	got := doc.Comparison["stateless"]
	want := Comparison{ComparedTurns: 1, ExcludedTurns: 1}
	if got != want {
		t.Fatalf("comparison = %+v, want %+v (no savings from the failed turn)", got, want)
	}
}
