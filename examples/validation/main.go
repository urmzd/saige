// Command validation exercises SAIGE's agent-layer features against a live
// OpenAI model (gpt-4o-mini by default) and writes a Markdown report so users can
// see real, end-to-end sample runs. It is a manual validation tool, not a unit
// test: it makes real API calls and is skipped automatically when OPENAI_API_KEY
// is unset.
//
//	OPENAI_API_KEY=... go run ./examples/validation
//	SAIGE_VALIDATION_MODEL=gpt-4o-mini go run ./examples/validation
//
// Output: examples/validation/results/validation-report.md
package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/cache/memcache"
	"github.com/urmzd/saige/agent/provider/cache"
	"github.com/urmzd/saige/agent/provider/openai"
	"github.com/urmzd/saige/agent/types"
)

func model() string {
	if m := os.Getenv("SAIGE_VALIDATION_MODEL"); m != "" {
		return m
	}
	return "gpt-4o-mini"
}

// result captures one feature check's outcome.
type result struct {
	name    string
	status  string // PASS | FAIL | SKIP
	detail  string
	elapsed time.Duration
}

func main() {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		fmt.Println("OPENAI_API_KEY not set — skipping live validation.")
		return
	}
	newProvider := func() types.Provider { return openai.NewAdapter(apiKey, model()) }

	checks := []struct {
		name string
		fn   func(types.Provider) (string, error)
	}{
		{"basic_generation", checkBasicGeneration},
		{"tool_calling", checkToolCalling},
		{"response_caching", checkResponseCaching},
		{"token_metrics", checkTokenMetrics},
		{"agent_handoff", checkHandoff},
		{"durable_memoization", checkDurable},
		{"llm_timeout", checkTimeout},
		{"multimodal_tool_output", checkMultimodal},
	}

	var results []result
	for _, c := range checks {
		fmt.Printf("▶ %-24s ", c.name)
		start := time.Now()
		detail, err := c.fn(newProvider())
		elapsed := time.Since(start)
		r := result{name: c.name, detail: detail, elapsed: elapsed}
		switch {
		case err != nil && strings.HasPrefix(err.Error(), "skip:"):
			r.status = "SKIP"
			r.detail = strings.TrimPrefix(err.Error(), "skip:")
		case err != nil:
			r.status = "FAIL"
			r.detail = err.Error()
		default:
			r.status = "PASS"
		}
		results = append(results, r)
		fmt.Printf("%s (%s)\n", r.status, elapsed.Round(time.Millisecond))
	}

	report := buildReport(results)
	out := resultsPath("validation-report.md")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fmt.Println("mkdir:", err)
		return
	}
	if err := os.WriteFile(out, []byte(report), 0o644); err != nil {
		fmt.Println("write:", err)
		return
	}
	fmt.Printf("\nReport written to %s\n", out)
}

// ── Feature checks ──────────────────────────────────────────────────

func checkBasicGeneration(p types.Provider) (string, error) {
	a := agentsdk.NewAgent(agentsdk.AgentConfig{Provider: p, SystemPrompt: "Answer in one short sentence."})
	text, _, err := run(a, "What is the capital of France?")
	if err != nil {
		return "", err
	}
	if !strings.Contains(strings.ToLower(text), "paris") {
		return "", fmt.Errorf("expected 'Paris' in %q", text)
	}
	return "model replied: " + oneLine(text), nil
}

func checkToolCalling(p types.Provider) (string, error) {
	var called bool
	mul := &types.ToolFunc{
		Def: types.ToolDef{
			Name: "multiply", Description: "Multiply two integers",
			Parameters: types.ParameterSchema{
				Type: "object", Required: []string{"a", "b"},
				Properties: map[string]types.PropertyDef{
					"a": {Type: "number"}, "b": {Type: "number"},
				},
			},
		},
		Fn: func(_ context.Context, args map[string]any) (string, error) {
			called = true
			a, _ := args["a"].(float64)
			b, _ := args["b"].(float64)
			return fmt.Sprintf("%g", a*b), nil
		},
	}
	a := agentsdk.NewAgent(agentsdk.AgentConfig{
		Provider: p, Tools: types.NewToolRegistry(mul),
		SystemPrompt: "Use the multiply tool for arithmetic. Then state the result.",
	})
	text, _, err := run(a, "What is 17 times 23? Use the tool.")
	if err != nil {
		return "", err
	}
	if !called {
		return "", fmt.Errorf("multiply tool was not called")
	}
	if !strings.Contains(text, "391") {
		return "", fmt.Errorf("expected 391 in %q", text)
	}
	return "tool called; model answered 391", nil
}

func checkResponseCaching(p types.Provider) (string, error) {
	counter := &countingProvider{inner: p}
	cached := cache.New(counter, cache.Config{Cache: memcache.New[cache.CachedResponse]()})
	req := []types.Message{
		types.NewSystemMessage("Answer in exactly one word."),
		types.NewUserMessage("What color is a clear daytime sky?"),
	}
	// Miss, then hit.
	_ = drainText(mustStream(cached.ChatStream(context.Background(), req, nil)))
	hitDeltas := drainAll(mustStream(cached.ChatStream(context.Background(), req, nil)))

	if counter.calls != 1 {
		return "", fmt.Errorf("upstream called %d times, want 1 (second served from cache)", counter.calls)
	}
	var cacheHit bool
	for _, d := range hitDeltas {
		if u, ok := d.(types.UsageDelta); ok && u.CacheHit {
			cacheHit = true
		}
	}
	if !cacheHit {
		return "", fmt.Errorf("second call did not report UsageDelta.CacheHit")
	}
	return "identical request served from cache (1 upstream call, CacheHit=true)", nil
}

func checkTokenMetrics(p types.Provider) (string, error) {
	m := &recordingMetrics{}
	a := agentsdk.NewAgent(agentsdk.AgentConfig{Provider: p, SystemPrompt: "Be brief."}, agentsdk.WithMetrics(m))
	if _, _, err := run(a, "Say hello."); err != nil {
		return "", err
	}
	if m.tokenCalls == 0 {
		return "", fmt.Errorf("RecordTokenUsage never fired")
	}
	if m.inputTokens == 0 || m.outputTokens == 0 {
		return "", fmt.Errorf("token counts not populated: in=%d out=%d", m.inputTokens, m.outputTokens)
	}
	return fmt.Sprintf("recorded usage: %d input + %d output tokens", m.inputTokens, m.outputTokens), nil
}

func checkHandoff(p types.Provider) (string, error) {
	a := agentsdk.NewAgent(agentsdk.AgentConfig{
		Name: "triage", Provider: p,
		SystemPrompt: "You are ONLY a router and cannot do arithmetic yourself. " +
			"For ANY math or calculation request you MUST call the handoff_to_math tool " +
			"to transfer control to the math specialist. Never compute an answer yourself; " +
			"always hand off first.",
	}, agentsdk.WithHandoffs(agentsdk.HandoffDef{
		Name: "math", Description: "Math specialist that solves arithmetic and math problems.", Provider: p,
		SystemPrompt: "You are a math expert. Answer concisely with the number.",
	}))
	_, deltas, err := run(a, "Please calculate 6 times 7 for me.")
	if err != nil {
		return "", err
	}
	for _, d := range deltas {
		if h, ok := d.(types.HandoffDelta); ok {
			return fmt.Sprintf("control transferred %s → %s", h.From, h.To), nil
		}
	}
	return "", fmt.Errorf("no HandoffDelta observed (model may not have invoked the handoff tool)")
}

func checkDurable(p types.Provider) (string, error) {
	counter := &countingProvider{inner: p}
	a := agentsdk.NewAgent(agentsdk.AgentConfig{Provider: counter, SystemPrompt: "Be brief."})
	runner := newMemoRunner()
	input := []types.Message{types.NewUserMessage("Name one planet.")}

	first, err := a.RunDurable(context.Background(), runner, input, "")
	if err != nil {
		return "", err
	}
	// "Replay": a fresh agent on a fresh tree with the SAME runner must reuse the
	// recorded LLM step and NOT call the provider again.
	a2 := agentsdk.NewAgent(agentsdk.AgentConfig{Provider: counter, SystemPrompt: "Be brief."})
	second, err := a2.RunDurable(context.Background(), runner, input, "")
	if err != nil {
		return "", err
	}
	if counter.calls != 1 {
		return "", fmt.Errorf("provider called %d times, want 1 (second run memoized)", counter.calls)
	}
	_ = first
	_ = second
	return "second durable run replayed the memoized LLM step (0 extra API calls)", nil
}

func checkTimeout(p types.Provider) (string, error) {
	a := agentsdk.NewAgent(agentsdk.AgentConfig{Provider: p, SystemPrompt: "Be brief."},
		agentsdk.WithLLMTimeout(time.Millisecond))
	_, deltas, _ := run(a, "Write a long essay about the ocean.")
	for _, d := range deltas {
		if e, ok := d.(types.ErrorDelta); ok && e.Error != nil {
			if strings.Contains(strings.ToLower(e.Error.Error()), "timeout") ||
				strings.Contains(e.Error.Error(), "deadline") {
				return "1ms LLM timeout fired as expected: " + oneLine(e.Error.Error()), nil
			}
			return "call aborted under tight deadline: " + oneLine(e.Error.Error()), nil
		}
	}
	return "", fmt.Errorf("expected a timeout/deadline error under a 1ms LLMTimeout")
}

func checkMultimodal(p types.Provider) (string, error) {
	red := solidPNG(color.RGBA{R: 220, G: 20, B: 20, A: 255})
	imgTool := &imageTool{data: red}
	a := agentsdk.NewAgent(agentsdk.AgentConfig{
		Provider: p, Tools: types.NewToolRegistry(imgTool),
		SystemPrompt: "When asked about an image, call make_image, then describe the dominant color you see in one word.",
	})
	text, _, err := run(a, "Generate the image with make_image and tell me its dominant color.")
	if err != nil {
		return "", err
	}
	if !strings.Contains(strings.ToLower(text), "red") {
		return "", fmt.Errorf("model did not identify the image as red: %q", oneLine(text))
	}
	return "model received the tool's image and identified it as red", nil
}

// ── Multimodal tool ─────────────────────────────────────────────────

// imageTool implements types.RichTool, returning a generated PNG.
type imageTool struct{ data []byte }

func (t *imageTool) Definition() types.ToolDef {
	return types.ToolDef{Name: "make_image", Description: "Generate an image and return it.",
		Parameters: types.ParameterSchema{Type: "object"}}
}
func (t *imageTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	r, err := t.ExecuteRich(ctx, args)
	return r.Text, err
}
func (t *imageTool) ExecuteRich(context.Context, map[string]any) (types.ToolResult, error) {
	return types.ImageResult("Here is the generated image.", types.MediaPNG, t.data), nil
}

func solidPNG(c color.Color) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// ── Helpers: providers, metrics, runner ─────────────────────────────

type countingProvider struct {
	inner types.Provider
	calls int
}

func (c *countingProvider) ChatStream(ctx context.Context, m []types.Message, t []types.ToolDef) (<-chan types.Delta, error) {
	c.calls++
	return c.inner.ChatStream(ctx, m, t)
}

type recordingMetrics struct {
	tokenCalls                int
	inputTokens, outputTokens int
}

func (m *recordingMetrics) RecordTokenUsage(_ context.Context, _, _ string, in, out int) {
	m.tokenCalls++
	m.inputTokens += in
	m.outputTokens += out
}
func (m *recordingMetrics) RecordToolCall(context.Context, string, time.Duration, error) {}
func (m *recordingMetrics) RecordProviderCall(context.Context, string, string, time.Duration, error) {
}
func (m *recordingMetrics) RecordAgentInvocation(context.Context, string, time.Duration) {}

// memoRunner is an in-memory durable StepRunner: it records each step's result
// by name and replays it on subsequent calls without re-running fn.
type memoRunner struct{ recorded map[string]types.StepResult }

func newMemoRunner() *memoRunner { return &memoRunner{recorded: map[string]types.StepResult{}} }

func (r *memoRunner) RunStep(ctx context.Context, name string, fn func(context.Context) (types.StepResult, error)) (types.StepResult, error) {
	if res, ok := r.recorded[name]; ok {
		return res, nil
	}
	res, err := fn(ctx)
	if err != nil {
		return res, err
	}
	r.recorded[name] = res
	return res, nil
}

// ── Stream helpers ──────────────────────────────────────────────────

func run(a *agentsdk.Agent, prompt string) (text string, deltas []types.Delta, err error) {
	stream := a.Invoke(context.Background(), []types.Message{types.NewUserMessage(prompt)})
	var sb strings.Builder
	for d := range stream.Deltas() {
		deltas = append(deltas, d)
		switch v := d.(type) {
		case types.TextContentDelta:
			sb.WriteString(v.Content)
		case types.ErrorDelta:
			if v.Error != nil {
				err = v.Error
			}
		}
	}
	return sb.String(), deltas, err
}

func mustStream(ch <-chan types.Delta, _ error) <-chan types.Delta { return ch }

func drainText(ch <-chan types.Delta) string {
	var sb strings.Builder
	for d := range ch {
		if t, ok := d.(types.TextContentDelta); ok {
			sb.WriteString(t.Content)
		}
	}
	return sb.String()
}

func drainAll(ch <-chan types.Delta) []types.Delta {
	var out []types.Delta
	for d := range ch {
		out = append(out, d)
	}
	return out
}

func oneLine(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > 140 {
		s = s[:137] + "..."
	}
	return s
}

func resultsPath(name string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "results", name)
}

func buildReport(results []result) string {
	var b strings.Builder
	pass, fail, skip := 0, 0, 0
	for _, r := range results {
		switch r.status {
		case "PASS":
			pass++
		case "FAIL":
			fail++
		case "SKIP":
			skip++
		}
	}
	b.WriteString("# SAIGE Live Validation Report\n\n")
	fmt.Fprintf(&b, "Model: `%s` (OpenAI) — real end-to-end runs of the agent SDK.\n\n", model())
	fmt.Fprintf(&b, "**%d passed, %d failed, %d skipped** of %d feature checks.\n\n", pass, fail, skip, len(results))
	b.WriteString("| Feature | Result | Detail | Time |\n|---|---|---|---|\n")
	for _, r := range results {
		badge := map[string]string{"PASS": "✅ PASS", "FAIL": "❌ FAIL", "SKIP": "⏭️ SKIP"}[r.status]
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", r.name, badge, escape(r.detail), r.elapsed.Round(time.Millisecond))
	}
	b.WriteString("\n> Regenerate with `OPENAI_API_KEY=... go run ./examples/validation`.\n")
	return b.String()
}

func escape(s string) string {
	return strings.ReplaceAll(oneLine(s), "|", "\\|")
}
