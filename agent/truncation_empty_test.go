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

// A turn the output limit cut short before it kept anything is not an
// answer. The run must not end as though it had finished: a caller would
// read the empty result as the agent's whole answer.
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
