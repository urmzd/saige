package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// pricedScripted is a ScriptedProvider with a rate card, so budget charges
// are recorded.
type pricedScripted struct {
	*agenttest.ScriptedProvider
}

func (pricedScripted) Model() string { return "scripted-model" }
func (pricedScripted) Capabilities() types.ModelCapabilities {
	return types.ModelCapabilities{
		Provider: "scripted",
		Model:    "scripted-model",
		Pricing:  types.Pricing{InputPerMTok: 1, OutputPerMTok: 1, AsOf: "2026-07-01"},
	}
}

func usage(prompt, completion int, reasons ...string) types.UsageDelta {
	return types.UsageDelta{PromptTokens: prompt, CompletionTokens: completion, FinishReasons: reasons}
}

func withUsage(deltas []types.Delta, u types.UsageDelta) []types.Delta {
	return append(append([]types.Delta(nil), deltas...), u)
}

func isSummaryRequest(call agenttest.ScriptedCall) bool {
	return len(call.Tools) == 0 && len(call.Messages) == 2 &&
		strings.Contains(types.MessagesToText(call.Messages[:1]), "Summarize the following conversation")
}

func TestTokenPressureCompaction(t *testing.T) {
	tests := []struct {
		name           string
		maxInputTokens int
		firstPrompt    int
		wantCompaction bool
	}{
		{name: "under the limit", maxInputTokens: 10_000, firstPrompt: 500, wantCompaction: false},
		{name: "reported usage over the limit", maxInputTokens: 1_000, firstPrompt: 5_000, wantCompaction: true},
		{name: "estimate over the limit before any report", maxInputTokens: 1, firstPrompt: 0, wantCompaction: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			responses := [][]types.Delta{
				withUsage(agenttest.ToolCallResponse("c1", "lookup", nil), usage(tt.firstPrompt, 5)),
			}
			// The estimate case compacts before the first turn too. Before
			// the second turn it compacts again, and the summary takes the
			// tool call together with its result.
			if tt.wantCompaction {
				responses = append(responses, withUsage(agenttest.TextResponse("summary of the lookup"), usage(70, 30)))
			}
			responses = append(responses, withUsage(agenttest.TextResponse("done"), usage(100, 10)))
			if tt.firstPrompt == 0 && tt.wantCompaction {
				responses = append([][]types.Delta{withUsage(agenttest.TextResponse("summary of the request"), usage(70, 30))}, responses...)
			}
			script := &agenttest.ScriptedProvider{Responses: responses}
			budget := types.NewBudget(types.BudgetPolicy{})
			tool := &agenttest.MockTool{Def: types.ToolDef{Name: "lookup"}, Result: "found"}
			a := must.Get(New(Config{
				Provider:     pricedScripted{script},
				SystemPrompt: "sys",
				Tools:        types.NewToolRegistry(tool),
				// Two model turns are needed; compaction must not use one.
				MaxIter:    2,
				CompactCfg: &types.CompactConfig{Strategy: types.CompactSummarize, MaxInputTokens: tt.maxInputTokens},
				Budget:     budget,
			}))
			before := a.Tree().Active()
			stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("look it up"))})
			deltas := agenttest.CollectDeltas(stream.Deltas())
			if err := stream.Wait(); err != nil {
				t.Fatalf("run failed: %v", err)
			}
			agenttest.AssertNoErrors(t, deltas)

			summaries := 0
			for _, call := range script.Requests() {
				if isSummaryRequest(call) {
					summaries++
				}
			}
			if got := summaries > 0; got != tt.wantCompaction {
				t.Fatalf("compacted = %v, want %v (calls %d)", got, tt.wantCompaction, script.CallCount())
			}
			if moved := a.Tree().Active() != before; moved != tt.wantCompaction {
				t.Fatalf("active branch moved = %v, want %v", moved, tt.wantCompaction)
			}
			if script.CallCount() != len(responses) {
				t.Fatalf("calls = %d, want %d", script.CallCount(), len(responses))
			}

			// Every call, summaries included, is charged.
			var wantPrompt int
			for _, r := range responses {
				for _, d := range r {
					if u, ok := d.(types.UsageDelta); ok {
						wantPrompt += u.PromptTokens
					}
				}
			}
			got := budget.Usage()
			if got.InputTokens+got.CachedInputTokens != wantPrompt {
				t.Fatalf("charged prompt tokens = %d, want %d", got.InputTokens, wantPrompt)
			}
		})
	}
}

func TestClearToolResultsUnderPressure(t *testing.T) {
	big := strings.Repeat("x", 4_000)
	script := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		withUsage(agenttest.ToolCallResponse("c1", "fetch", nil), usage(100, 5)),
		withUsage(agenttest.ToolCallResponse("c2", "fetch", nil), usage(1_200, 5)),
		withUsage(agenttest.TextResponse("done"), usage(300, 5)),
	}}
	tool := &agenttest.MockTool{Def: types.ToolDef{Name: "fetch"}, Result: big}
	a := must.Get(New(Config{
		Provider:     script,
		SystemPrompt: "sys",
		Tools:        types.NewToolRegistry(tool),
		CompactCfg: &types.CompactConfig{
			Strategy: types.CompactClearToolResults, MaxInputTokens: 1_500, KeepToolResults: 1,
		},
	}))
	stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("fetch twice"))})
	agenttest.CollectDeltas(stream.Deltas())
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	calls := script.Requests()
	if len(calls) != 3 {
		t.Fatalf("calls = %d, want 3 (clearing makes no model call)", len(calls))
	}
	last := types.MessagesToText(calls[2].Messages)
	if !strings.Contains(last, types.ClearedToolResultText("fetch", "c1")) {
		t.Fatalf("old result not cleared:\n%.300s", last)
	}
	if strings.Count(last, big) != 1 {
		t.Fatal("the newest result must be kept")
	}
}

func contextLengthErr(n int) error {
	return &types.ProviderError{Provider: "scripted", Kind: types.ErrorKindContextLength,
		Err: fmt.Errorf("prompt is too long (%d)", n)}
}

func TestContextLengthRecovery(t *testing.T) {
	history := func() []types.Message {
		var msgs []types.Message
		for i := range 8 {
			msgs = append(msgs, types.UserMsg(types.Text(fmt.Sprintf("question %d", i))), types.AssistantMsg(types.Text(fmt.Sprintf("answer %d", i))))
		}
		return append(msgs, types.UserMsg(types.Text("final question")))
	}
	summary := withUsage(agenttest.TextResponse("summary"), usage(10, 5))
	tests := []struct {
		name      string
		compact   *types.CompactConfig
		errors    []error
		responses [][]types.Delta
		wantErr   error // nil means success
		wantCalls int
	}{
		{
			name:      "compact once and retry",
			compact:   &types.CompactConfig{Strategy: types.CompactSummarize, Threshold: 1000},
			errors:    []error{contextLengthErr(1)},
			responses: [][]types.Delta{nil, summary, agenttest.TextResponse("answer")},
			wantCalls: 3,
		},
		{
			name:    "original error after three attempts",
			compact: &types.CompactConfig{Strategy: types.CompactSummarize, Threshold: 1000},
			errors: []error{
				contextLengthErr(1), nil, contextLengthErr(2), nil, contextLengthErr(3), nil, contextLengthErr(4),
			},
			responses: [][]types.Delta{nil, summary, nil, summary, nil, summary, nil},
			wantErr:   contextLengthErr(1),
			wantCalls: 7,
		},
		{
			name:      "none strategy makes no summary call",
			compact:   &types.CompactConfig{Strategy: types.CompactNone},
			errors:    []error{contextLengthErr(1)},
			wantErr:   contextLengthErr(1),
			wantCalls: 1,
		},
		{
			name:      "none strategy under a token limit makes no summary call",
			compact:   &types.CompactConfig{Strategy: types.CompactNone, MaxInputTokens: 1},
			errors:    []error{contextLengthErr(1)},
			wantErr:   contextLengthErr(1),
			wantCalls: 1,
		},
		{
			name:      "no compaction configured",
			errors:    []error{contextLengthErr(1)},
			wantErr:   contextLengthErr(1),
			wantCalls: 1,
		},
		{
			name:      "other errors are not retried",
			compact:   &types.CompactConfig{Strategy: types.CompactSummarize, Threshold: 1000},
			errors:    []error{&types.ProviderError{Provider: "scripted", Kind: types.ErrorKindAuth, Err: errors.New("bad key")}},
			wantErr:   &types.ProviderError{Provider: "scripted", Kind: types.ErrorKindAuth, Err: errors.New("bad key")},
			wantCalls: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script := &agenttest.ScriptedProvider{Responses: tt.responses, Errors: tt.errors}
			a := must.Get(New(Config{Provider: script, SystemPrompt: "sys", MaxIter: 1, CompactCfg: tt.compact}))
			stream := a.Invoke(context.Background(), history())
			agenttest.CollectDeltas(stream.Deltas())
			err := stream.Wait()
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("run failed: %v", err)
				}
			} else if err == nil || err.Error() != tt.wantErr.Error() {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got := script.CallCount(); got != tt.wantCalls {
				t.Fatalf("calls = %d, want %d", got, tt.wantCalls)
			}
		})
	}
}

func TestCompactionKeepsToolPairs(t *testing.T) {
	// Candidates: user, assistant(call a), result a, assistant(call b),
	// result b. Half of five is two, which would leave result a without its
	// call, so the summary also takes result a and the kept suffix starts at
	// call b.
	script := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.TextResponse("summary of the first lookup"),
		agenttest.TextResponse("done"),
	}}
	a := must.Get(New(Config{
		Provider:     script,
		SystemPrompt: "sys",
		MaxIter:      1,
		CompactCfg:   &types.CompactConfig{MaxInputTokens: 1},
	}))
	input := []types.Message{
		types.UserMsg(types.Text("go")),
		types.AssistantMessage{Parts: []types.AssistantPart{types.ToolCallPart{ID: "a", Name: "t"}}},
		types.ToolResults(types.ToolResultPart{CallID: "a", Parts: []types.ToolOutputPart{types.Text("1")}}),
		types.AssistantMessage{Parts: []types.AssistantPart{types.ToolCallPart{ID: "b", Name: "t"}}},
		types.ToolResults(types.ToolResultPart{CallID: "b", Parts: []types.ToolOutputPart{types.Text("2")}}),
	}
	before := a.Tree().Active()
	stream := a.Invoke(context.Background(), input)
	agenttest.CollectDeltas(stream.Deltas())
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	summaries := 0
	for _, call := range script.Requests() {
		if isSummaryRequest(call) {
			summaries++
		}
	}
	if summaries != 1 || script.CallCount() != 2 {
		t.Fatalf("summaries = %d, calls = %d, want 1 and 2", summaries, script.CallCount())
	}
	after := a.Tree().Active()
	if after == before {
		t.Fatal("the compacted branch did not become active")
	}
	if err := checkToolPairing(a.Tree(), after); err != nil {
		t.Fatal(err)
	}
	// The turn after the summary saw call b with its result.
	last := script.Requests()[script.CallCount()-1]
	if err := toolPairingError(last.Messages); err != nil {
		t.Fatalf("the provider received a split history: %v", err)
	}
	if !strings.Contains(types.MessagesToText(last.Messages), "2") {
		t.Fatal("the kept suffix lost the last tool result")
	}
}

func TestCheckCompactionSplit(t *testing.T) {
	call := func(id string) types.Message {
		return types.AssistantMessage{Parts: []types.AssistantPart{types.ToolCallPart{ID: id, Name: "t"}}}
	}
	result := func(id string) types.Message {
		return types.ToolResults(types.ToolResultPart{CallID: id, Parts: []types.ToolOutputPart{types.Text("r")}})
	}
	tests := []struct {
		name      string
		history   []types.Message
		wantSplit bool
	}{
		{name: "text only", history: []types.Message{
			types.UserMsg(types.Text("a")), types.AssistantMsg(types.Text("b")), types.UserMsg(types.Text("c")), types.AssistantMsg(types.Text("d")),
		}},
		{name: "boundary between pairs", history: []types.Message{
			call("a"), result("a"), call("b"), result("b"),
		}},
		{name: "half falls inside a pair", history: []types.Message{
			types.UserMsg(types.Text("go")), call("a"), result("a"), call("b"), result("b"),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := must.Get(New(Config{Provider: &agenttest.ScriptedProvider{}, SystemPrompt: "sys"}))
			tr := a.Tree()
			branch := tr.Active()
			for _, m := range tt.history {
				if _, err := tr.AddChildOnBranch(context.Background(), branch, m); err != nil {
					t.Fatal(err)
				}
			}
			err := checkCompactionSplit(tr, branch)
			if got := errors.Is(err, errSplitToolCall); got != tt.wantSplit {
				t.Fatalf("split = %v (%v), want %v", got, err, tt.wantSplit)
			}
		})
	}
}

// reservingTool holds a reservation on a shared budget when it runs, the
// way a concurrent subagent would while its own call is in flight.
type reservingTool struct {
	budget *types.Budget
}

func (reservingTool) Definition() types.ToolDef { return types.ToolDef{Name: "spawn"} }

func (r reservingTool) Execute(context.Context, map[string]any) (string, error) {
	if _, err := r.budget.Reserve("sibling", types.Pricing{InputPerMTok: 1, OutputPerMTok: 1, AsOf: "2026-07-01"}); err != nil {
		return "", err
	}
	return "spawned", nil
}

func TestCompactionIsAdmittedByTheBudget(t *testing.T) {
	// The budget has room for two requests. The first turn uses one and a
	// sibling holds the other, so the summary must be refused before the
	// provider sees it.
	script := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		withUsage(agenttest.ToolCallResponse("c1", "spawn", nil), usage(5_000, 5)),
		withUsage(agenttest.TextResponse("summary"), usage(70, 30)),
		withUsage(agenttest.TextResponse("done"), usage(100, 10)),
	}}
	budget := types.NewBudget(types.BudgetPolicy{MaxRequests: 2})
	a := must.Get(New(Config{
		Provider:     pricedScripted{script},
		SystemPrompt: "sys",
		Tools:        types.NewToolRegistry(reservingTool{budget: budget}),
		MaxIter:      2,
		CompactCfg:   &types.CompactConfig{Strategy: types.CompactSummarize, MaxInputTokens: 1_000},
		Budget:       budget,
	}))
	stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("spawn one"))})
	agenttest.CollectDeltas(stream.Deltas())
	err := stream.Wait()
	if !errors.Is(err, types.ErrBudgetAdmission) || !errors.Is(err, types.ErrBudgetBusy) {
		t.Fatalf("err = %v, want a busy admission refusal", err)
	}
	if got := script.CallCount(); got != 1 {
		t.Fatalf("calls = %d, want 1: the summary must not reach the provider", got)
	}
	if got := budget.Usage().Requests; got != 1 {
		t.Fatalf("charged requests = %d, want 1", got)
	}
}

func TestOutputTruncation(t *testing.T) {
	tests := []struct {
		name      string
		response  []types.Delta
		wantErr   bool
		wantText  string
		wantCalls int
	}{
		{
			name:     "text only ends cleanly with a marker",
			response: withUsage(agenttest.TextResponse("partial ans"), usage(10, 100, "max_tokens")),
			wantText: "partial ans",
		},
		{
			name: "closed tool call is not run",
			response: withUsage(append(agenttest.TextResponse("let me"), agenttest.ToolCallResponse("c1", "act", map[string]any{})...),
				usage(10, 100, "length")),
			wantErr:  true,
			wantText: "let me",
		},
		{
			name: "open tool call is not run",
			response: []types.Delta{
				types.PartStart{Index: 0, Kind: types.KindToolCall, ID: "c1", Name: "act"},
				types.PartDelta{Index: 0, Args: `{"x":`},
				usage(10, 100, "MAX_TOKENS"),
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script := &agenttest.ScriptedProvider{Responses: [][]types.Delta{tt.response, agenttest.TextResponse("never")}}
			tool := &agenttest.MockTool{Def: types.ToolDef{Name: "act"}, Result: "acted"}
			a := must.Get(New(Config{Provider: script, SystemPrompt: "sys", Tools: types.NewToolRegistry(tool)}))
			stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))})
			deltas := agenttest.CollectDeltas(stream.Deltas())
			err := stream.Wait()
			if tt.wantErr {
				var te *types.ResponseTruncatedError
				if !errors.As(err, &te) || !types.IsTruncated(err) || te.FinishReason != types.FinishReasonMaxTokens {
					t.Fatalf("err = %v, want a ResponseTruncatedError with the normalized reason", err)
				}
			} else if err != nil {
				t.Fatalf("run failed: %v", err)
			}
			if tool.CallCount() != 0 {
				t.Fatal("a truncated tool call must never run")
			}
			if script.CallCount() != 1 {
				t.Fatalf("calls = %d, want 1", script.CallCount())
			}
			var truncated *types.TruncatedDelta
			for _, d := range deltas {
				if td, ok := d.(types.TruncatedDelta); ok {
					truncated = &td
				}
			}
			if truncated == nil {
				t.Fatal("no TruncatedDelta")
			}
			// Every provider spelling reports the same reason.
			if truncated.Reason != types.FinishReasonMaxTokens {
				t.Fatalf("reason = %q, want %q", truncated.Reason, types.FinishReasonMaxTokens)
			}

			tip, _ := a.Tree().Tip(a.Tree().Active())
			if tt.wantText == "" {
				if truncated.NodeID != "" {
					t.Fatal("nothing was committed, so the delta must not name a node")
				}
				return
			}
			if truncated.NodeID != string(tip.ID) {
				t.Fatalf("delta node = %q, tip = %q", truncated.NodeID, tip.ID)
			}
			am, ok := tip.Message.(types.AssistantMessage)
			if !ok {
				t.Fatalf("tip is %T", tip.Message)
			}
			for _, c := range am.Parts {
				if _, ok := c.(types.ToolCallPart); ok {
					t.Fatal("a truncated tool call must not be committed")
				}
			}
			if truncated.Reason == "" || !strings.Contains(types.MessagesToText([]types.Message{am}), tt.wantText) {
				t.Fatalf("committed turn = %+v, delta = %+v", am, *truncated)
			}
			// The conversation still encodes and reloads.
			data, err := a.Tree().MarshalJSON()
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var reloaded tree.Tree
			if err := reloaded.UnmarshalJSON(data); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			msgs, err := reloaded.FlattenBranch(reloaded.Active())
			if err != nil || !strings.Contains(types.MessagesToText(msgs), tt.wantText) {
				t.Fatalf("reloaded history = %v, %v", msgs, err)
			}
		})
	}
}
