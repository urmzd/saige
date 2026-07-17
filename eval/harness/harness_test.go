package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func writeFixture(t *testing.T, path string, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeChatResponse(t *testing.T, w http.ResponseWriter, content string, promptTokens uint64, completionTokens uint64) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	err := json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{
			"message": map[string]string{"content": content},
		}},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

type chatRequest struct {
	Model          string    `json:"model"`
	Messages       []Message `json:"messages"`
	ResponseFormat any       `json:"response_format"`
}

func decodeChatRequest(t *testing.T, r *http.Request) chatRequest {
	t.Helper()
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return req
}

func writeSampleExperiment(t *testing.T, dir string) {
	t.Helper()
	writeFixture(t, filepath.Join(dir, "system.md"), "You produce Markdown.")
	writeFixture(t, filepath.Join(dir, "turn-0.md"), "Create greeting doc.")
	writeFixture(t, filepath.Join(dir, "turn-1.md"), "Change hello to world.")
}

func TestLoadCorpusDefaults(t *testing.T) {
	root := t.TempDir()
	writeSampleExperiment(t, filepath.Join(root, "002-second"))
	writeSampleExperiment(t, filepath.Join(root, "001-first"))
	writeFixture(t, filepath.Join(root, "stray.txt"), "not an experiment")

	experiments, err := LoadCorpus(root)
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}
	if len(experiments) != 2 {
		t.Fatalf("len(experiments) = %d, want 2", len(experiments))
	}
	if experiments[0].ID != "001-first" || experiments[1].ID != "002-second" {
		t.Fatalf("experiment order = %s, %s", experiments[0].ID, experiments[1].ID)
	}
	exp := experiments[0]
	if exp.Format != "text/markdown" {
		t.Fatalf("format = %q, want text/markdown", exp.Format)
	}
	if exp.Systems["base"] != "You produce Markdown." {
		t.Fatalf("base system = %q", exp.Systems["base"])
	}
	if len(exp.Turns) != 2 || exp.Turns[0].Index != 0 || exp.Turns[1].Index != 1 {
		t.Fatalf("turns = %#v", exp.Turns)
	}
	if exp.Turns[1].Prompt != "Change hello to world." {
		t.Fatalf("turn 1 prompt = %q", exp.Turns[1].Prompt)
	}
	if exp.Dir != filepath.Join(root, "001-first") {
		t.Fatalf("dir = %q", exp.Dir)
	}
}

func TestLoadCorpusExperimentJSON(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "001-html")
	writeFixture(t, filepath.Join(dir, "experiment.json"), `{"format":"text/html","systems":{"base":"prompts/base.md","alt":"prompts/alt.md"}}`)
	writeFixture(t, filepath.Join(dir, "prompts/base.md"), "Base system.")
	writeFixture(t, filepath.Join(dir, "prompts/alt.md"), "Alt system.")
	writeFixture(t, filepath.Join(dir, "turn-0.md"), "Create page.")

	experiments, err := LoadCorpus(root)
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}
	exp := experiments[0]
	if exp.Format != "text/html" {
		t.Fatalf("format = %q, want text/html", exp.Format)
	}
	if exp.Systems["base"] != "Base system." || exp.Systems["alt"] != "Alt system." {
		t.Fatalf("systems = %#v", exp.Systems)
	}
}

func TestLoadCorpusMissingTurnZero(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "001-broken")
	writeFixture(t, filepath.Join(dir, "system.md"), "System.")
	writeFixture(t, filepath.Join(dir, "turn-1.md"), "Edit.")

	if _, err := LoadCorpus(root); err == nil || !strings.Contains(err.Error(), "missing turn-0.md") {
		t.Fatalf("err = %v, want missing turn-0.md", err)
	}
}

func TestLoadCorpusEmpty(t *testing.T) {
	if _, err := LoadCorpus(t.TempDir()); err == nil || !strings.Contains(err.Error(), "no experiments") {
		t.Fatalf("err = %v, want no experiments", err)
	}
}

func TestRunnerBaseAndStatelessEndToEnd(t *testing.T) {
	root := t.TempDir()
	expDir := filepath.Join(root, "001-md-smoke")
	writeSampleExperiment(t, expDir)

	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		req := decodeChatRequest(t, r)
		if req.ResponseFormat != nil {
			t.Errorf("unexpected response_format: %#v", req.ResponseFormat)
		}
		switch calls.Add(1) {
		case 1:
			want := []Message{
				{Role: "system", Content: "You produce Markdown."},
				{Role: "user", Content: "Create greeting doc."},
			}
			if !reflect.DeepEqual(req.Messages, want) {
				t.Errorf("base turn-0 messages = %#v", req.Messages)
			}
			writeChatResponse(t, w, "hello doc", 10, 8)
		case 2:
			want := []Message{
				{Role: "system", Content: "You produce Markdown."},
				{Role: "user", Content: "Create greeting doc."},
				{Role: "assistant", Content: "hello doc"},
				{Role: "user", Content: "Change hello to world."},
			}
			if !reflect.DeepEqual(req.Messages, want) {
				t.Errorf("base turn-1 messages = %#v", req.Messages)
			}
			writeChatResponse(t, w, "world doc", 20, 6)
		case 3:
			wantPrompt := "## Current Artifact\n\n```\nworld doc\n```\n\n## Edit Instruction\n\nChange hello to world.\n\nReturn the complete updated artifact, raw, with no commentary."
			want := []Message{
				{Role: "system", Content: "You produce Markdown."},
				{Role: "user", Content: wantPrompt},
			}
			if !reflect.DeepEqual(req.Messages, want) {
				t.Errorf("stateless turn-1 messages = %#v", req.Messages)
			}
			writeChatResponse(t, w, "final doc", 15, 5)
		default:
			t.Errorf("unexpected call %d", calls.Load())
		}
	}))
	defer server.Close()

	experiments, err := LoadCorpus(root)
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}
	runner := &Runner{
		Client: NewClient(server.URL, "test-key", "mock"),
		Flows:  []Flow{BaseFlow{}, StatelessFlow{}},
	}
	if err := runner.Run(context.Background(), experiments); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3 (stateless turn-0 seeds from base)", calls.Load())
	}

	outputs := map[string]string{
		"outputs/base/turn-0.md":      "hello doc",
		"outputs/base/turn-1.md":      "world doc",
		"outputs/stateless/turn-0.md": "world doc",
		"outputs/stateless/turn-1.md": "final doc",
	}
	for rel, want := range outputs {
		data, err := os.ReadFile(filepath.Join(expDir, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if string(data) != want {
			t.Fatalf("%s = %q, want %q", rel, data, want)
		}
	}

	metricsData, err := os.ReadFile(filepath.Join(expDir, "metrics.json"))
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	var metrics DefaultMetrics
	if err := json.Unmarshal(metricsData, &metrics); err != nil {
		t.Fatalf("parse metrics: %v", err)
	}
	if metrics.ExperimentID != "001-md-smoke" || metrics.Model != "mock" || metrics.Provider != "openai-compatible" {
		t.Fatalf("metrics header = %#v", metrics)
	}
	if metrics.Format != "text/markdown" {
		t.Fatalf("format = %q", metrics.Format)
	}
	base, ok := metrics.Flows["base"]
	if !ok {
		t.Fatalf("metrics missing base flow: %s", metricsData)
	}
	if base.Turn0.InputTokens != 10 || base.Turn0.OutputTokens != 8 {
		t.Fatalf("base turn0 = %#v", base.Turn0)
	}
	if base.TotalInputTokens != 20 || base.TotalOutputTokens != 6 || len(base.PerTurn) != 1 {
		t.Fatalf("base flow = %#v", base.FlowMetrics)
	}
	stateless, ok := metrics.Flows["stateless"]
	if !ok {
		t.Fatalf("metrics missing stateless flow: %s", metricsData)
	}
	if stateless.Turn0.InputTokens != 10 {
		t.Fatalf("stateless turn0 not seeded from base: %#v", stateless.Turn0)
	}
	comparison, ok := metrics.Comparison["stateless"]
	if !ok {
		t.Fatalf("metrics missing stateless comparison: %s", metricsData)
	}
	if comparison.InputTokenSavingsPct != 25 {
		t.Fatalf("input savings = %v, want 25", comparison.InputTokenSavingsPct)
	}
	if comparison.OutputTokenSavingsPct != Pct(6, 5) {
		t.Fatalf("output savings = %v", comparison.OutputTokenSavingsPct)
	}
}

func TestRunnerSkipAndForce(t *testing.T) {
	root := t.TempDir()
	writeSampleExperiment(t, filepath.Join(root, "001-md-smoke"))

	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeChatResponse(t, w, "doc", 10, 5)
	}))
	defer server.Close()

	experiments, err := LoadCorpus(root)
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}
	runner := &Runner{
		Client: NewClient(server.URL, "test-key", "mock"),
		Flows:  []Flow{BaseFlow{}},
	}
	if err := runner.Run(context.Background(), experiments); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	after := calls.Load()
	if after != 2 {
		t.Fatalf("calls after first run = %d, want 2", after)
	}

	if err := runner.Run(context.Background(), experiments); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if calls.Load() != after {
		t.Fatalf("skip did not skip: calls = %d, want %d", calls.Load(), after)
	}

	runner.Force = true
	if err := runner.Run(context.Background(), experiments); err != nil {
		t.Fatalf("forced Run: %v", err)
	}
	if calls.Load() != after+2 {
		t.Fatalf("force did not re-run: calls = %d, want %d", calls.Load(), after+2)
	}
}

func TestStatelessFlowWithoutSeed(t *testing.T) {
	root := t.TempDir()
	expDir := filepath.Join(root, "001-md-smoke")
	writeSampleExperiment(t, expDir)

	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			writeChatResponse(t, w, "hello doc", 10, 8)
		default:
			writeChatResponse(t, w, "world doc", 15, 5)
		}
	}))
	defer server.Close()

	experiments, err := LoadCorpus(root)
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}
	client := NewClient(server.URL, "test-key", "mock")
	result, err := StatelessFlow{}.Run(context.Background(), client, experiments[0], NewFlowContext())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (synthesis plus one edit)", calls.Load())
	}
	if result.Turn0.InputTokens != 10 || result.Turn0.OutputTokens != 8 {
		t.Fatalf("turn0 = %#v", result.Turn0)
	}
	if result.Artifact != "world doc" {
		t.Fatalf("artifact = %q", result.Artifact)
	}
	data, err := os.ReadFile(filepath.Join(expDir, "outputs/stateless/turn-0.md"))
	if err != nil {
		t.Fatalf("read turn-0: %v", err)
	}
	if string(data) != "hello doc" {
		t.Fatalf("turn-0 = %q", data)
	}
}

func TestClientWithJSONSchema(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		format, ok := req["response_format"].(map[string]any)
		if !ok {
			t.Fatalf("missing response_format: %#v", req)
		}
		if format["type"] != "json_schema" {
			t.Errorf("type = %v", format["type"])
		}
		schema := format["json_schema"].(map[string]any)
		if schema["name"] != "custom_doc" || schema["strict"] != true {
			t.Errorf("json_schema = %#v", schema)
		}
		writeChatResponse(t, w, `{"ok":true}`, 5, 3)
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-key", "mock")
	result, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "go"}},
		WithJSONSchema("custom_doc", map[string]any{"type": "object"}))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if result.Text != `{"ok":true}` || result.InputTokens != 5 || result.OutputTokens != 3 {
		t.Fatalf("result = %#v", result)
	}
}

func TestTurnResultExtraRoundTrip(t *testing.T) {
	retried := false
	reason := "apply failed: missing target"
	original := TurnResult{
		Turn:              2,
		Edit:              "Change hello to world.",
		InputTokens:       12,
		OutputTokens:      6,
		CachedInputTokens: 3,
		LatencyMS:         40,
		OutputBytes:       9,
		Retried:           &retried,
		Failed:            true,
		FailureReason:     &reason,
		RepairAttempts:    1,
		Extra: map[string]any{
			"envelope_parsed": true,
			"apply_succeeded": false,
			"envelope_name":   "edit",
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var flat map[string]any
	if err := json.Unmarshal(data, &flat); err != nil {
		t.Fatalf("Unmarshal flat: %v", err)
	}
	if flat["envelope_parsed"] != true || flat["envelope_name"] != "edit" {
		t.Fatalf("extra keys not flattened: %s", data)
	}
	if flat["turn"] != float64(2) || flat["failure_reason"] != reason {
		t.Fatalf("fixed keys wrong: %s", data)
	}
	if _, nested := flat["Extra"]; nested {
		t.Fatalf("Extra leaked as nested key: %s", data)
	}

	var decoded TurnResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Fatalf("round trip mismatch:\n got %#v\nwant %#v", decoded, original)
	}
}

func TestTurnResultExtraCollision(t *testing.T) {
	result := TurnResult{Extra: map[string]any{"turn": 5}}
	if _, err := json.Marshal(result); err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("err = %v, want collision error", err)
	}
}

func TestCleanArtifact(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"plain", "hello", "hello"},
		{"fenced", "```html\n<p>hi</p>\n```", "<p>hi</p>"},
		{"fence no language", "```\nbody\n```", "body"},
		{"think block", "<think>reasoning</think>\nanswer", "answer"},
		{"think inside fence", "<think>a\nb</think>\n```\ndoc\n```", "doc"},
		{"whitespace", "  padded  ", "padded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CleanArtifact(tc.input); got != tc.want {
				t.Fatalf("CleanArtifact(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestExtractJSONObject(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"bare object", `{"a":1}`, `{"a":1}`},
		{"surrounded", "prefix {\"a\":1} suffix", `{"a":1}`},
		{"no object", "plain text", "plain text"},
		{"nested braces", `note {"a":{"b":2}} tail`, `{"a":{"b":2}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractJSONObject(tc.input); got != tc.want {
				t.Fatalf("ExtractJSONObject(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestCompare(t *testing.T) {
	base := FlowMetrics{TotalInputTokens: 200, TotalOutputTokens: 100, TotalLatencyMillis: 1000}
	other := FlowMetrics{TotalInputTokens: 150, TotalOutputTokens: 40, TotalLatencyMillis: 900}
	got := Compare(base, other)
	want := Comparison{OutputTokenSavingsPct: 60, InputTokenSavingsPct: 25, LatencySavingsPct: 10}
	if got != want {
		t.Fatalf("Compare = %#v, want %#v", got, want)
	}
	if zero := Compare(FlowMetrics{}, other); zero != (Comparison{}) {
		t.Fatalf("Compare with zero base = %#v", zero)
	}
}

func TestComputeReliability(t *testing.T) {
	mkFail := func(reason string, extra map[string]any) TurnResult {
		tr := TurnResult{Failed: true, Extra: extra}
		if reason != "" {
			tr.FailureReason = &reason
		}
		return tr
	}
	turns := []TurnResult{
		{Failed: false},
		mkFail("envelope parse failed: bad json", nil),
		mkFail("validation failed: missing section", nil),
		mkFail("invalid envelope: wrong name", nil),
		mkFail("apply failed: no target", nil),
		mkFail("", map[string]any{"envelope_parsed": false}),
		mkFail("", map[string]any{"envelope_parsed": true, "apply_succeeded": false}),
		mkFail("", nil),
	}

	report := ComputeReliability(turns)
	if report.EditTurns != 8 || report.MissCount != 7 {
		t.Fatalf("report = %#v", report)
	}
	if report.ParseMissCount != 1 || report.ValidationMissCount != 1 || report.InvalidEnvelopeCount != 1 {
		t.Fatalf("prefix classification wrong: %#v", report)
	}
	if report.ApplyMissCount != 2 {
		t.Fatalf("ApplyMissCount = %d, want 2 (prefix plus extra)", report.ApplyMissCount)
	}
	if report.RequestFailureCount != 1 || report.UnknownMissCount != 1 {
		t.Fatalf("request/unknown wrong: %#v", report)
	}
	if report.ByReason["apply failed: no target"] != 1 {
		t.Fatalf("ByReason = %#v", report.ByReason)
	}
	if want := Round1(700.0/8) / 100; report.MissRate != want {
		t.Fatalf("MissRate = %v, want %v", report.MissRate, want)
	}

	clean := ComputeReliability([]TurnResult{{Failed: false}})
	if clean.MissCount != 0 || clean.ByReason != nil || clean.MissRate != 0 {
		t.Fatalf("clean report = %#v", clean)
	}
}

func TestFilterExperiments(t *testing.T) {
	experiments := []Experiment{{ID: "001-a"}, {ID: "002-b"}, {ID: "010-c"}}
	if got := FilterExperiments(experiments, "00", 0); len(got) != 2 {
		t.Fatalf("prefix filter = %#v", got)
	}
	if got := FilterExperiments(experiments, "", 2); len(got) != 2 || got[1].ID != "002-b" {
		t.Fatalf("count filter = %#v", got)
	}
	if got := FilterExperiments(experiments, "zzz", 0); len(got) != 0 {
		t.Fatalf("no-match filter = %#v", got)
	}
	if got := FilterExperiments(experiments, "", 0); len(got) != 3 {
		t.Fatalf("no-op filter = %#v", got)
	}
}

func TestFlowReportExtraRoundTrip(t *testing.T) {
	report := FlowReport{
		Turn0:       TurnMetrics{InputTokens: 10, OutputTokens: 8, LatencyMS: 5, ArtifactBytes: 9},
		FlowMetrics: FlowMetrics{TotalInputTokens: 20, TotalOutputTokens: 6},
		Extra:       map[string]any{"envelope_parse_rate": 0.5},
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var flat map[string]any
	if err := json.Unmarshal(data, &flat); err != nil {
		t.Fatalf("Unmarshal flat: %v", err)
	}
	if flat["envelope_parse_rate"] != 0.5 {
		t.Fatalf("extra not flattened: %s", data)
	}
	if _, ok := flat["turn0"]; !ok {
		t.Fatalf("turn0 missing: %s", data)
	}
	var decoded FlowReport
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Extra["envelope_parse_rate"] != 0.5 || decoded.Turn0.InputTokens != 10 {
		t.Fatalf("round trip mismatch: %#v", decoded)
	}
}

func TestFormatExt(t *testing.T) {
	cases := map[string]string{
		"text/html":     ".html",
		"text/markdown": ".md",
		"text/x-go":     ".go",
		"unknown/blob":  ".txt",
	}
	for format, want := range cases {
		if got := FormatExt(format); got != want {
			t.Fatalf("FormatExt(%q) = %q, want %q", format, got, want)
		}
	}
}

func TestCustomFlowAndAssemble(t *testing.T) {
	root := t.TempDir()
	expDir := filepath.Join(root, "001-md-smoke")
	writeSampleExperiment(t, expDir)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeChatResponse(t, w, "doc", 10, 5)
	}))
	defer server.Close()

	experiments, err := LoadCorpus(root)
	if err != nil {
		t.Fatalf("LoadCorpus: %v", err)
	}
	runner := &Runner{
		Client: NewClient(server.URL, "test-key", "mock"),
		Flows:  []Flow{countingFlow{}},
		Assemble: func(exp Experiment, results map[string]FlowResult) (any, error) {
			return map[string]any{
				"experiment_id": exp.ID,
				"turns_seen":    results["counting"].Extra["turns_seen"],
			}, nil
		},
	}
	if err := runner.Run(context.Background(), experiments); err != nil {
		t.Fatalf("Run: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(expDir, "metrics.json"))
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	if !strings.Contains(string(data), `"turns_seen": 2`) {
		t.Fatalf("custom assemble output = %s", data)
	}
}

// countingFlow is a minimal custom Flow used to exercise Runner.Assemble.
type countingFlow struct{}

func (countingFlow) Name() string { return "counting" }

func (countingFlow) Run(ctx context.Context, c *Client, exp Experiment, fc *FlowContext) (FlowResult, error) {
	if len(exp.Turns) == 0 {
		return FlowResult{}, fmt.Errorf("no turns")
	}
	return FlowResult{Extra: map[string]any{"turns_seen": len(exp.Turns)}}, nil
}
