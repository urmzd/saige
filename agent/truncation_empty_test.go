package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// thought is a completed thinking part, as a provider that closes its blocks
// at the cut sends one.
func thought(index int) []types.Delta {
	return []types.Delta{
		types.PartStart{Index: index, Kind: types.KindThinking},
		types.PartDelta{Index: index, Thinking: "weighing every option at length"},
		types.PartEnd{Index: index},
	}
}

// A turn the output limit cut short before it kept anything a caller can use
// is not an answer. The run must not end as though it had finished: a caller
// would read the empty result as the agent's whole answer.
func TestATruncatedTurnThatKeptNothingFailsTheRun(t *testing.T) {
	tests := []struct {
		name     string
		response []types.Delta
	}{
		{
			name:     "no parts at all",
			response: []types.Delta{usage(10, 4096, "max_tokens")},
		},
		{
			// Open thinking is dropped at the cut: its signature would be
			// invalid.
			name: "thinking still open",
			response: []types.Delta{
				types.PartStart{Index: 0, Kind: types.KindThinking},
				types.PartDelta{Index: 0, Thinking: "weighing every option at length"},
				usage(10, 4096, "max_tokens"),
			},
		},
		{
			name: "a text part opened and never written",
			response: []types.Delta{
				types.PartStart{Index: 0, Kind: types.KindText},
				usage(10, 4096, "length"),
			},
		},
		{
			// The whole limit went to thinking, and the provider closed
			// the block at the cut. Thinking is not an answer.
			name:     "only a completed thinking part",
			response: withUsage(thought(0), usage(10, 4096, "max_tokens")),
		},
		{
			name: "only redacted thinking",
			response: []types.Delta{
				types.PartStart{Index: 0, Kind: types.KindThinking},
				types.PartEnd{Index: 0, Part: types.ThinkingPart{Redacted: true, Signature: "ciphertext"}},
				usage(10, 4096, "max_tokens"),
			},
		},
		{
			// A router's record of who served the turn is kept beside the
			// thinking. It is metadata, and no more of an answer.
			name: "only thinking, served through a router",
			response: append([]types.Delta{types.RouteDelta{Provider: "anthropic", Model: "m"}},
				withUsage(thought(0), usage(10, 4096, "max_tokens"))...),
		},
		{
			// A search the provider ran is work toward an answer, not one:
			// the caller has nothing to read and nothing to run.
			name: "thinking, a server tool call and its result",
			response: withUsage(append(thought(0),
				types.PartStart{Index: 1, Kind: types.KindServerToolCall, ID: "srv_1", Name: "web_search"},
				types.PartEnd{Index: 1, Part: types.ServerToolCallPart{ID: "srv_1", ToolKind: types.ServerToolWebSearch, Name: "web_search", Input: map[string]any{"query": "go"}}},
				types.PartStart{Index: 2, Kind: types.KindServerToolResult},
				types.PartEnd{Index: 2, Part: types.ServerToolResultPart{CallID: "srv_1", ToolKind: types.ServerToolWebSearch, Text: "3 results"}}),
				usage(10, 4096, "max_tokens")),
		},
		{
			name: "only a server tool call",
			response: []types.Delta{
				types.PartStart{Index: 0, Kind: types.KindServerToolCall, ID: "srv_1", Name: "web_search"},
				types.PartEnd{Index: 0, Part: types.ServerToolCallPart{ID: "srv_1", ToolKind: types.ServerToolWebSearch, Name: "web_search"}},
				usage(10, 4096, "max_tokens"),
			},
		},
		{
			name: "thinking, then a text part with nothing in it",
			response: withUsage(append(thought(0),
				types.PartStart{Index: 1, Kind: types.KindText}, types.PartEnd{Index: 1}),
				usage(10, 4096, "max_tokens")),
		},
	}
	for _, tt := range tests {
		for _, auto := range []int{0, 3} {
			name := tt.name
			if auto > 0 {
				name += ", with auto-continue"
			}
			t.Run(name, func(t *testing.T) {
				script := &agenttest.ScriptedProvider{Responses: [][]types.Delta{tt.response, agenttest.TextResponse("never")}}
				a := must.Get(New(Config{Provider: script, SystemPrompt: "sys"}, WithAutoContinue(auto)))
				stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))})
				deltas := agenttest.CollectDeltas(stream.Deltas())
				err := stream.Wait()

				var te *types.ResponseTruncatedError
				if !errors.As(err, &te) || !types.IsTruncated(err) {
					t.Fatalf("err = %v, want a ResponseTruncatedError", err)
				}
				if !te.Empty || te.FinishReason != types.FinishReasonMaxTokens || te.OutputTokens != 4096 {
					t.Errorf("error = %+v, want an empty turn, the normalized reason and 4096 output tokens", *te)
				}
				if msg := err.Error(); !strings.Contains(msg, "before any answer was produced") || !strings.Contains(msg, "4096") {
					t.Errorf("the error does not say what happened: %q", msg)
				}
				// Nothing was kept to continue, so the turn is not resumed:
				// the same request would meet the same limit.
				if got := script.CallCount(); got != 1 {
					t.Errorf("calls = %d, want 1", got)
				}
				var truncated *types.TruncatedDelta
				for _, d := range deltas {
					if td, ok := d.(types.TruncatedDelta); ok {
						truncated = &td
					}
				}
				if truncated == nil || truncated.NodeID != "" || truncated.Reason != types.FinishReasonMaxTokens {
					t.Errorf("truncated delta = %+v, want one that names no node", truncated)
				}
				tip, _ := a.Tree().Tip(a.Tree().Active())
				if _, ok := tip.Message.(types.AssistantMessage); ok {
					t.Errorf("an assistant turn was committed: %+v", tip.Message)
				}
			})
		}
	}
}

// Thinking does not make an answer, and it does not unmake one: a truncated
// turn that kept something beside its thinking is handled as it was.
func TestATruncatedTurnThatKeptMoreThanThinkingIsUnchanged(t *testing.T) {
	t.Run("thinking and text end the run cleanly", func(t *testing.T) {
		response := withUsage(append(thought(0), types.PartStart{Index: 1, Kind: types.KindText},
			types.PartDelta{Index: 1, Text: "partial ans"}), usage(10, 4096, "max_tokens"))
		script := &agenttest.ScriptedProvider{Responses: [][]types.Delta{response, agenttest.TextResponse("never")}}
		a := must.Get(New(Config{Provider: script, SystemPrompt: "sys"}))
		stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))})
		agenttest.CollectDeltas(stream.Deltas())
		if err := stream.Wait(); err != nil {
			t.Fatalf("run failed: %v", err)
		}
		tip, _ := a.Tree().Tip(a.Tree().Active())
		am, ok := tip.Message.(types.AssistantMessage)
		if !ok || !strings.Contains(types.MessagesToText([]types.Message{am}), "partial ans") {
			t.Fatalf("tip = %+v, want the partial answer", tip.Message)
		}
	})

	t.Run("thinking and text are continued", func(t *testing.T) {
		response := withUsage(append(thought(0), types.PartStart{Index: 1, Kind: types.KindText},
			types.PartDelta{Index: 1, Text: "partial ans"}), usage(10, 4096, "max_tokens"))
		script := &agenttest.ScriptedProvider{Responses: [][]types.Delta{response, agenttest.TextResponse(" and the rest")}}
		a := must.Get(New(Config{Provider: script, SystemPrompt: "sys"}, WithAutoContinue(1)))
		stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))})
		agenttest.CollectDeltas(stream.Deltas())
		if err := stream.Wait(); err != nil {
			t.Fatalf("run failed: %v", err)
		}
		if got := script.CallCount(); got != 2 {
			t.Fatalf("calls = %d, want 2: the partial answer is resumed", got)
		}
	})

	t.Run("thinking and a cut-off tool call fail as a tool call does", func(t *testing.T) {
		response := append(thought(0),
			types.PartStart{Index: 1, Kind: types.KindToolCall, ID: "c1", Name: "act"},
			types.PartDelta{Index: 1, Args: `{"x":`},
			usage(10, 4096, "max_tokens"))
		script := &agenttest.ScriptedProvider{Responses: [][]types.Delta{response, agenttest.TextResponse("never")}}
		tool := &agenttest.MockTool{Def: types.ToolDef{Name: "act"}, Result: "acted"}
		a := must.Get(New(Config{Provider: script, SystemPrompt: "sys", Tools: types.NewToolRegistry(tool)}))
		stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))})
		agenttest.CollectDeltas(stream.Deltas())
		var te *types.ResponseTruncatedError
		if err := stream.Wait(); !errors.As(err, &te) || te.Empty {
			t.Fatalf("err = %v, want a ResponseTruncatedError that is not an empty turn's", err)
		}
		if tool.CallCount() != 0 {
			t.Fatal("a truncated tool call must never run")
		}
	})
}

// thinkingCut is a turn the output limit cut short with only its thinking
// kept.
func thinkingCut() []types.Delta {
	return withUsage(thought(0), usage(10, 4096, "max_tokens"))
}

// An automatic continuation that adds nothing does not undo the partial
// answer the run already kept. The run ends as it does when its continuations
// are used up: cleanly, on the partial answer, with nothing after it, so that
// Continue can resume it later.
func TestAContinuationThatAddsNothingKeepsThePartialAnswer(t *testing.T) {
	for _, prefill := range []bool{false, true} {
		name := "continue prompt"
		if prefill {
			name = "prefill"
		}
		t.Run(name, func(t *testing.T) {
			sp := newStepProvider(
				stepCall{before: maxTokensResponse("part one")},
				stepCall{before: thinkingCut()},
				stepCall{before: agenttest.TextResponse(" part two")},
			)
			var provider types.Provider = sp
			if prefill {
				provider = prefillProvider{sp}
			}
			a := must.Get(New(Config{Provider: provider}, WithAutoContinue(2)))
			ctx := context.Background()
			stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("write"))})
			deltas := agenttest.CollectDeltas(stream.Deltas())
			if err := stream.Wait(); err != nil {
				t.Fatalf("Wait = %v, want the run to end on its partial answer", err)
			}
			// The empty continuation is not continued in its turn.
			if got := len(sp.requests()); got != 2 {
				t.Fatalf("requests = %d, want 2", got)
			}

			tip, err := a.Tree().Tip(a.Tree().Active())
			if err != nil {
				t.Fatal(err)
			}
			am, ok := tip.Message.(types.AssistantMessage)
			if !ok || textOf(am) != "part one" {
				t.Fatalf("the branch ends with %#v, want the partial answer", tip.Message)
			}
			msgs, _ := a.Tree().FlattenBranch(a.Tree().Active())
			if text := types.MessagesToText(msgs); strings.Contains(text, DefaultContinuePrompt) {
				t.Errorf("the continue prompt was left on the branch:\n%s", text)
			}
			var last *types.TruncatedDelta
			for _, d := range deltas {
				if td, ok := d.(types.TruncatedDelta); ok {
					last = &td
				}
			}
			if last == nil || last.NodeID != string(tip.ID) || last.Reason != types.FinishReasonMaxTokens {
				t.Errorf("last truncated delta = %+v, want one naming the partial answer %s", last, tip.ID)
			}

			// What was kept can still be continued.
			cont, err := a.Continue(ctx, "")
			if err != nil {
				t.Fatalf("Continue = %v", err)
			}
			for range cont.Deltas() {
			}
			if err := cont.Wait(); err != nil {
				t.Fatal(err)
			}
			msgs, _ = a.Tree().FlattenBranch(a.Tree().Active())
			if text := types.MessagesToText(msgs); !strings.Contains(text, "part one") || !strings.Contains(text, " part two") {
				t.Errorf("the continued branch is:\n%s", text)
			}
		})
	}
}

// A continuation that does add to the answer is recorded as before: the
// prompt it was asked with, then what it said.
func TestAContinuationThatAddsToTheAnswerKeepsItsPrompt(t *testing.T) {
	sp := newStepProvider(stepCall{before: maxTokensResponse("part one")}, stepCall{before: agenttest.TextResponse(" part two")})
	a := must.Get(New(Config{Provider: sp}, WithAutoContinue(1)))
	stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("write"))})
	for range stream.Deltas() {
	}
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	req := sp.requests()[1]
	if got := textOf(req[len(req)-1]); got != DefaultContinuePrompt {
		t.Fatalf("the continuation was asked with %q, want the continue prompt", got)
	}
	msgs, _ := a.Tree().FlattenBranch(a.Tree().Active())
	want := "System: \nUser: write\nAssistant: part one\nUser: " + DefaultContinuePrompt + "\nAssistant:  part two\n"
	if got := types.MessagesToText(msgs); got != want {
		t.Errorf("branch =\n%q\nwant\n%q", got, want)
	}
}

// A continuation whose call fails leaves the branch on the partial answer:
// the prompt it was asked with was never an instruction of the user's.
func TestAFailedContinuationLeavesNoPromptBehind(t *testing.T) {
	sp := newStepProvider(stepCall{before: maxTokensResponse("part one")},
		stepCall{before: []types.Delta{types.ErrorDelta{Error: errors.New("boom")}}})
	a := must.Get(New(Config{Provider: sp}, WithAutoContinue(1)))
	stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("write"))})
	for range stream.Deltas() {
	}
	if err := stream.Wait(); err == nil {
		t.Fatal("the run did not fail")
	}
	tip, _ := a.Tree().Tip(a.Tree().Active())
	if am, ok := tip.Message.(types.AssistantMessage); !ok || textOf(am) != "part one" {
		t.Errorf("the branch ends with %#v, want the partial answer", tip.Message)
	}
}

// The forced final answer follows the same rule when it is the continuation.
func TestAForcedFinalContinuationThatAddsNothingKeepsThePartialAnswer(t *testing.T) {
	sp := newStepProvider(stepCall{before: maxTokensResponse("part one")}, stepCall{before: thinkingCut()})
	a := must.Get(New(Config{Provider: sp, MaxIter: 1, OnMaxIter: MaxIterForceFinal}, WithAutoContinue(1)))
	stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("write"))})
	for range stream.Deltas() {
	}
	if err := stream.Wait(); err != nil {
		t.Fatalf("Wait = %v, want the run to end on its partial answer", err)
	}
	if got := len(sp.requests()); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
	tip, _ := a.Tree().Tip(a.Tree().Active())
	if am, ok := tip.Message.(types.AssistantMessage); !ok || textOf(am) != "part one" {
		t.Errorf("the branch ends with %#v, want the partial answer", tip.Message)
	}
}
