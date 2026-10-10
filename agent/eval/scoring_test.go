package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	topeval "github.com/urmzd/saige/eval"
)

func TestToolRespondsWithinScorer(t *testing.T) {
	calls := []ToolCallRecord{
		{Name: "search", DurationMs: 120},
		{Name: "fetch", DurationMs: 900},
		{Name: "search", DurationMs: 300},
	}
	tests := []struct {
		name       string
		tool       string
		limit      time.Duration
		calls      []ToolCallRecord
		wantMetric string
		want       float64
		reason     string
		declined   bool
	}{
		{"within", "search", 500 * time.Millisecond, calls, "tool_responds_within:search<=500ms", 1, "2 calls, slowest 300ms", false},
		{"one slow call", "search", 200 * time.Millisecond, calls, "tool_responds_within:search<=200ms", 0, "search took 300ms", false},
		{"every tool", "", 500 * time.Millisecond, calls, "tool_responds_within:*<=500ms", 0, "fetch took 900ms", false},
		{"tool never called declines", "delete", time.Second, calls, "", 0, "", true},
		{"no annotation declines", "search", time.Second, nil, "", 0, "", true},
		{"unfinished call is over the limit", "slow", time.Millisecond, []ToolCallRecord{
			{Name: "slow", Exec: ExecUnfinished},
		}, "tool_responds_within:slow<=1ms", 0, "slow never finished", false},
		{"unfinished call fails among fast ones", "", time.Second, []ToolCallRecord{
			{Name: "search", DurationMs: 10, Exec: ExecFinished},
			{Name: "slow", Exec: ExecUnfinished},
		}, "tool_responds_within:*<=1000ms", 0, "slow never finished", false},
		{"call that never ran is left out", "", time.Second, []ToolCallRecord{
			{Name: "search", DurationMs: 10, Exec: ExecFinished},
			{Name: "planned", Exec: ExecNotRun},
		}, "tool_responds_within:*<=1000ms", 1, "1 calls, slowest 10ms", false},
		{"only calls that never ran declines", "planned", time.Second, []ToolCallRecord{
			{Name: "planned", Exec: ExecNotRun},
		}, "", 0, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obs := topeval.Observation{}
			if tt.calls != nil {
				obs = trajectoryObs(t, tt.calls...)
			}
			got, err := ToolRespondsWithinScorer(tt.tool, tt.limit).Score(context.Background(), obs)
			if err != nil {
				t.Fatal(err)
			}
			if tt.declined {
				if got.Name != "" {
					t.Fatalf("score = %+v, want declined", got)
				}
				return
			}
			if got.Name != tt.wantMetric || got.Value != tt.want || !strings.Contains(got.Reason, tt.reason) {
				t.Fatalf("score = %+v, want %s %v with %q", got, tt.wantMetric, tt.want, tt.reason)
			}
		})
	}
}

func TestToolRespondsWithinCollectedStream(t *testing.T) {
	tests := []struct {
		name     string
		deltas   []any
		want     float64
		reason   string
		declined bool
	}{
		{"started but never ended", []any{
			types.PartStart{Index: 0, Kind: types.KindToolCall, ID: "c1", Name: "slow"},
			types.PartEnd{Index: 0},
			types.ToolExecStartDelta{ToolCallID: "c1", Name: "slow"},
			types.DoneDelta{},
		}, 0, "slow never finished", false},
		{"announced but never executed", []any{
			types.PartStart{Index: 1, Kind: types.KindToolCall, ID: "c1", Name: "slow"},
			types.PartEnd{Index: 1},
			types.DoneDelta{},
		}, 0, "", true},
		{"finished", []any{
			types.PartStart{Index: 2, Kind: types.KindToolCall, ID: "c1", Name: "slow"},
			types.PartEnd{Index: 2},
			types.ToolExecStartDelta{ToolCallID: "c1", Name: "slow"},
			types.ToolExecEndDelta{ToolCallID: "c1", Name: "slow", Result: "ok"},
			types.DoneDelta{},
		}, 1, "1 calls", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var obs topeval.Observation
			if err := AnnotateObservation(&obs, CollectAgentRun(feed(tt.deltas...))); err != nil {
				t.Fatal(err)
			}
			got, err := ToolRespondsWithinScorer("slow", time.Hour).Score(context.Background(), obs)
			if err != nil {
				t.Fatal(err)
			}
			if tt.declined {
				if got.Name != "" {
					t.Fatalf("score = %+v, want declined", got)
				}
				return
			}
			if got.Value != tt.want || !strings.Contains(got.Reason, tt.reason) {
				t.Fatalf("score = %+v, want %v with %q", got, tt.want, tt.reason)
			}
		})
	}
}

func TestAgentRunUsageModelsAndCost(t *testing.T) {
	run := CollectAgentRun(feed(
		types.UsageDelta{PromptTokens: 1000, CachedPromptTokens: 400, CompletionTokens: 100, ResponseModel: "model-a"},
		types.UsageDelta{PromptTokens: 500, CompletionTokens: 50, ResponseModel: "model-a"},
		types.UsageDelta{PromptTokens: 999, CompletionTokens: 999, ResponseModel: "model-a", CacheHit: true},
		types.DoneDelta{},
	))
	want := types.TokenUsage{InputTokens: 1100, CachedInputTokens: 400, OutputTokens: 150, Requests: 2}
	if run.Usage != want {
		t.Fatalf("Usage = %+v, want %+v", run.Usage, want)
	}
	if fmt.Sprint(run.Models) != "[model-a]" {
		t.Fatalf("Models = %v", run.Models)
	}
	// The cache replay counts toward neither the timing tokens nor usage.
	if run.Timing.InputTokens != 1500 || run.Timing.OutputTokens != 150 {
		t.Fatalf("Timing tokens = %d in, %d out; want 1500, 150", run.Timing.InputTokens, run.Timing.OutputTokens)
	}
	if run.CostUSD != nil {
		t.Fatal("an unpriced run has a cost")
	}

	rates := types.Pricing{InputPerMTok: 2, OutputPerMTok: 10, CachedInputPerMTok: 0.5}
	// 1100*2 + 400*0.5 + 150*10 = 3900 per million tokens.
	wantCost := 0.0039
	tests := []struct {
		name    string
		pricing types.Pricing
		want    *float64
	}{
		{"priced", rates, &wantCost},
		{"free", types.Pricing{Free: true}, new(float64)},
		{"unpriced", types.Pricing{}, nil},
		{"other currency", types.Pricing{Currency: "EUR", InputPerMTok: 1}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			priced := run.Priced(tt.pricing)
			switch {
			case tt.want == nil && priced.CostUSD != nil:
				t.Fatalf("CostUSD = %v, want nil", *priced.CostUSD)
			case tt.want != nil && (priced.CostUSD == nil || abs(*priced.CostUSD-*tt.want) > 1e-9):
				t.Fatalf("CostUSD = %v, want %v", priced.CostUSD, *tt.want)
			}
		})
	}

	var obs topeval.Observation
	if err := AnnotateObservation(&obs, run.Priced(rates)); err != nil {
		t.Fatal(err)
	}
	if obs.Timing.CostUSD == nil || abs(*obs.Timing.CostUSD-wantCost) > 1e-9 {
		t.Fatalf("annotated cost = %v", obs.Timing.CostUSD)
	}
	budget, err := topeval.CostBudgetScorer(0.001).Score(context.Background(), obs)
	if err != nil || budget.Value != 0 {
		t.Fatalf("cost budget = %+v, %v; want a failed budget", budget, err)
	}
	total, err := topeval.TotalTokensScorer().Score(context.Background(), obs)
	if err != nil || total.Value != 1650 {
		t.Fatalf("total tokens = %+v, %v; want 1650", total, err)
	}
}

func TestAgentRunCacheReplayOnly(t *testing.T) {
	run := CollectAgentRun(feed(
		types.UsageDelta{PromptTokens: 999, CompletionTokens: 999, ResponseModel: "m", CacheHit: true},
		types.DoneDelta{},
	))
	var obs topeval.Observation
	if err := AnnotateObservation(&obs, run.Priced(types.Pricing{InputPerMTok: 1, OutputPerMTok: 1})); err != nil {
		t.Fatal(err)
	}
	if _, ok := obs.Timing.TotalTokens(); ok {
		t.Fatalf("a replayed call recorded tokens: %+v", obs.Timing)
	}
	if obs.Timing.CostUSD == nil || *obs.Timing.CostUSD != 0 {
		t.Fatalf("replay cost = %v, want 0", obs.Timing.CostUSD)
	}
}

func TestAgentRunPricedMultipleModels(t *testing.T) {
	rates := types.Pricing{InputPerMTok: 2, OutputPerMTok: 10}
	tests := []struct {
		name    string
		models  []string
		wantNil bool
	}{
		{"one model", []string{"a", "a"}, false},
		{"no model reported", []string{"", ""}, false},
		{"fallback across models", []string{"a", "b"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var deltas []any
			for _, m := range tt.models {
				deltas = append(deltas, types.UsageDelta{PromptTokens: 100, CompletionTokens: 10, ResponseModel: m})
			}
			deltas = append(deltas, types.DoneDelta{})
			priced := CollectAgentRun(feed(deltas...)).Priced(rates)
			if (priced.CostUSD == nil) != tt.wantNil {
				t.Fatalf("CostUSD = %v, want nil %v", priced.CostUSD, tt.wantNil)
			}
		})
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// schemaProvider adds structured output to a scripted provider and records
// the schema it was asked for.
type schemaProvider struct {
	*agenttest.ScriptedProvider
	schemas []*types.ParameterSchema
}

func (p *schemaProvider) Stream(ctx context.Context, req types.Request) (<-chan types.Delta, error) {
	p.schemas = append(p.schemas, req.Schema)
	return p.ScriptedProvider.Stream(ctx, types.Request{Messages: req.Messages, Tools: req.Tools})
}

func (p *schemaProvider) SupportsSchema() bool { return true }

func TestGenerator(t *testing.T) {
	verdict := `{"reasoning": "good", "score": 0.9}`
	t.Run("plain provider", func(t *testing.T) {
		p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse(verdict)}}
		gen := NewGenerator(p, WithSystemPrompt("be strict"))
		score, err := topeval.NewJudgeScorer(gen).Score(context.Background(), topeval.Observation{Output: json.RawMessage(`"x"`)})
		if err != nil {
			t.Fatal(err)
		}
		if score.Value != 0.9 || score.Reason != "good" {
			t.Fatalf("score = %+v", score)
		}
		calls := p.Requests()
		if len(calls) != 1 || len(calls[0].Messages) != 2 || calls[0].Messages[0].Role() != types.RoleSystem {
			t.Fatalf("requests = %+v", calls)
		}
	})
	t.Run("structured provider gets the judge schema", func(t *testing.T) {
		p := &schemaProvider{ScriptedProvider: &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse(verdict)}}}
		score, err := topeval.NewJudgeScorer(NewGenerator(p)).Score(context.Background(), topeval.Observation{Output: json.RawMessage(`"x"`)})
		if err != nil {
			t.Fatal(err)
		}
		if score.Value != 0.9 {
			t.Fatalf("score = %+v", score)
		}
		if len(p.schemas) != 1 || p.schemas[0].Type != "object" || fmt.Sprint(p.schemas[0].Required) != "[reasoning score]" {
			t.Fatalf("schemas = %+v", p.schemas)
		}
	})
	t.Run("stream error", func(t *testing.T) {
		boom := errors.New("boom")
		p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{{types.ErrorDelta{Error: boom}}}}
		if _, err := NewGenerator(p).Generate(context.Background(), "q"); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
	})
	t.Run("no text", func(t *testing.T) {
		p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{{types.DoneDelta{}}}}
		if _, err := NewGenerator(p).Generate(context.Background(), "q"); err == nil {
			t.Fatal("empty reply accepted")
		}
	})
}

func TestRegisteredAgentScorers(t *testing.T) {
	obs := trajectoryObs(t,
		ToolCallRecord{Name: "search", Arguments: map[string]any{"q": "go"}, DurationMs: 10},
		ToolCallRecord{Name: "answer"},
	)
	tests := []struct {
		spec string
		want float64
	}{
		{`{"kind": "calls_in_order", "params": {"tools": ["search", "answer"]}}`, 1},
		{`{"kind": "does_not_call", "params": {"tools": ["delete"]}}`, 1},
		{`{"kind": "only_calls", "params": {"tools": ["search"]}}`, 0},
		{`{"kind": "calls_with", "params": {"tool": "search", "args": {"q": "go"}}}`, 1},
		{`{"kind": "tool_responds_within", "params": {"tool": "search", "max_ms": 5}}`, 0},
		{`{"kind": "tool_call_count"}`, 2},
		{`{"kind": "tool_success_rate", "name": "tools_ok"}`, 1},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			var spec topeval.ScorerSpec
			if err := json.Unmarshal([]byte(tt.spec), &spec); err != nil {
				t.Fatal(err)
			}
			s, err := topeval.BuildScorer(spec)
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.Score(context.Background(), obs)
			if err != nil {
				t.Fatal(err)
			}
			if got.Value != tt.want || (spec.Name != "" && got.Name != spec.Name) {
				t.Fatalf("score = %+v, want %v", got, tt.want)
			}
		})
	}
	if _, err := topeval.BuildScorer(topeval.ScorerSpec{Kind: "calls_in_order"}); err == nil {
		t.Fatal("calls_in_order without tools accepted")
	}
	if err := RegisterScorers(topeval.DefaultRegistry); err == nil {
		t.Fatal("registering twice did not fail")
	}
	if err := RegisterScorers(topeval.NewRegistry()); err != nil {
		t.Fatalf("private registry: %v", err)
	}
}
