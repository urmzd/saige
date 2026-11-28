package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// cutText is a turn the output limit cut short with "partial ans" kept.
func cutText() []types.Delta {
	return []types.Delta{
		types.PartStart{Index: 0, Kind: types.KindText},
		types.PartDelta{Index: 0, Text: "partial ans"},
		usage(10, 4096, "max_tokens"),
	}
}

// branchOf is a's active branch after its system prompt, one line a message:
// who said it and what, with the loop's own messages named and not quoted.
func branchOf(t *testing.T, a *Agent) []string {
	t.Helper()
	msgs, err := a.Tree().FlattenBranch(a.Tree().Active())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range msgs {
		text := strings.TrimSpace(types.MessagesToText([]types.Message{m}))
		who, said, _ := strings.Cut(text, ": ")
		switch {
		case who == "System" && strings.HasPrefix(said, "<wrap_up>"):
			out = append(out, "wrap-up note")
		case who == "System":
			// The system prompt.
		case said == DefaultContinuePrompt:
			out = append(out, "continue prompt")
		case strings.HasPrefix(said, "<turn_stopped>"):
			out = append(out, "stop record")
		default:
			out = append(out, strings.ToLower(who)+": "+said)
		}
	}
	return out
}

// checkBranch fails when the branch is not want, holds the continue prompt
// twice, or has two assistant turns with nothing between them. A provider
// that takes a prefill continues a partial turn with no message between, so
// its branch is not held to the last rule.
func checkBranch(t *testing.T, a *Agent, prefill bool, want ...string) {
	t.Helper()
	want = without(prefill, want...)
	got := branchOf(t, a)
	if !slices.Equal(got, want) {
		t.Errorf("branch =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	prompts := 0
	for i, line := range got {
		if line == "continue prompt" {
			prompts++
		}
		if !prefill && i > 0 && strings.HasPrefix(line, "assistant: ") && strings.HasPrefix(got[i-1], "assistant: ") {
			t.Errorf("two assistant turns in a row: %q, %q", got[i-1], line)
		}
	}
	if prompts > 1 {
		t.Errorf("the continue prompt is on the branch %d times", prompts)
	}
}

// wantEmptyTurn fails unless err is the error of a turn the output limit cut
// short with no answer.
func wantEmptyTurn(t *testing.T, err error) {
	t.Helper()
	var te *types.ResponseTruncatedError
	if !errors.As(err, &te) || !te.Empty {
		t.Fatalf("Wait = %v, want the error of a turn cut short with no answer", err)
	}
}

// without drops "continue prompt" from lines for a provider that takes a
// prefill, which is never asked with it.
func without(prefill bool, lines ...string) []string {
	if !prefill {
		return lines
	}
	return slices.DeleteFunc(slices.Clone(lines), func(l string) bool { return l == "continue prompt" })
}

// A steering message that joins the branch before a continuation's call is
// what that call answers. The call is then a turn like any other: when the
// output limit cuts it short with no answer, the message got none, and the
// run says so. It does not end as though the partial answer before the
// message were the whole of it.
func TestASteerThatJoinsAContinuationIsAnswered(t *testing.T) {
	tests := []struct {
		name   string
		second []types.Delta
		empty  bool
	}{
		{name: "the call answers", second: agenttest.TextResponse("and the rest")},
		{name: "the call is cut short with no answer", second: thinkingCut(), empty: true},
	}
	for _, tt := range tests {
		for _, prefill := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/prefill=%v", tt.name, prefill), func(t *testing.T) {
				hold := make(chan struct{})
				sp := newStepProvider(stepCall{before: cutText()[:2], hold: hold, after: cutText()[2:]}, stepCall{before: tt.second})
				var provider types.Provider = sp
				if prefill {
					provider = prefillProvider{sp}
				}
				a := must.Get(New(Config{Provider: provider, SystemPrompt: "sys"}, WithAutoContinue(2)))
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("go"))})
				deltas := collect(stream)
				// The steer arrives while the first call is running, and
				// joins at the safe point before the continuation's call.
				waitStarted(t, sp, 0)
				if _, err := stream.Submit(types.UserMsg(types.Text("also do X")), SubmitSteer); err != nil {
					t.Fatal(err)
				}
				close(hold)
				<-deltas
				err := stream.Wait()

				if got := len(sp.requests()); got != 2 {
					t.Fatalf("requests = %d, want 2", got)
				}
				if tt.empty {
					wantEmptyTurn(t, err)
					checkBranch(t, a, prefill, "user: go", "assistant: partial ans", "continue prompt", "user: also do X")
					return
				}
				if err != nil {
					t.Fatalf("Wait = %v", err)
				}
				checkBranch(t, a, prefill, "user: go", "assistant: partial ans", "continue prompt", "user: also do X", "assistant: and the rest")
			})
		}
	}
}

// The wrap-up note joins the branch the same way, and ends the continuation
// the same way.
func TestAWrapUpNoteThatJoinsAContinuationIsAnswered(t *testing.T) {
	tests := []struct {
		name   string
		second []types.Delta
		empty  bool
	}{
		{name: "the call answers", second: agenttest.TextResponse("and the rest")},
		{name: "the call is cut short with no answer", second: thinkingCut(), empty: true},
	}
	for _, tt := range tests {
		for _, prefill := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/prefill=%v", tt.name, prefill), func(t *testing.T) {
				sp := newStepProvider(stepCall{before: cutText()}, stepCall{before: tt.second})
				var provider types.Provider = sp
				if prefill {
					provider = prefillProvider{sp}
				}
				a := must.Get(New(Config{Provider: provider, SystemPrompt: "sys", MaxIter: 5, WrapUpAt: 1}, WithAutoContinue(2)))
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("go"))})
				<-collect(stream)
				err := stream.Wait()

				if got := len(sp.requests()); got != 2 {
					t.Fatalf("requests = %d, want 2", got)
				}
				if tt.empty {
					wantEmptyTurn(t, err)
					checkBranch(t, a, prefill, "user: go", "assistant: partial ans", "continue prompt", "wrap-up note")
					return
				}
				if err != nil {
					t.Fatalf("Wait = %v", err)
				}
				checkBranch(t, a, prefill, "user: go", "assistant: partial ans", "continue prompt", "wrap-up note", "assistant: and the rest")
			})
		}
	}
}

// A stop or an interrupting message that cuts a continuation's call writes
// what the call had said, and the prompt the call was asked with comes
// before it: the branch reads in the order the model was asked.
func TestACallOfAContinuationThatIsCutIsWrittenAfterItsPrompt(t *testing.T) {
	said := []types.Delta{types.PartStart{Index: 0, Kind: types.KindText}, types.PartDelta{Index: 0, Text: "more"}}
	tests := []struct {
		name      string
		before    []types.Delta
		interrupt bool
		want      []string
	}{
		{name: "stopped with text", before: said,
			want: []string{"user: go", "assistant: partial ans", "continue prompt", "assistant: more", "stop record"}},
		{name: "stopped with nothing said",
			want: []string{"user: go", "assistant: partial ans", "continue prompt", "stop record"}},
		{name: "interrupted with text", before: said, interrupt: true,
			want: []string{"user: go", "assistant: partial ans", "continue prompt", "assistant: more", "user: new direction", "assistant: replaced"}},
		{name: "interrupted with nothing said", interrupt: true,
			want: []string{"user: go", "assistant: partial ans", "continue prompt", "user: new direction", "assistant: replaced"}},
	}
	for _, tt := range tests {
		for _, prefill := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/prefill=%v", tt.name, prefill), func(t *testing.T) {
				hold := make(chan struct{})
				defer close(hold)
				sp := newStepProvider(stepCall{before: cutText()}, stepCall{before: tt.before, hold: hold}, stepCall{before: agenttest.TextResponse("replaced")})
				var provider types.Provider = sp
				if prefill {
					provider = prefillProvider{sp}
				}
				a := must.Get(New(Config{Provider: provider, SystemPrompt: "sys"}, WithAutoContinue(2)))
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("go"))})

				// The call is cut once the run has streamed what it said,
				// or once it has started when it says nothing.
				streamed, done := make(chan struct{}), make(chan struct{})
				go func() {
					defer close(done)
					seen := false
					for d := range stream.Deltas() {
						if pd, ok := d.(types.PartDelta); ok && pd.Text == "more" && !seen {
							seen = true
							close(streamed)
						}
					}
				}()
				waitStarted(t, sp, 1)
				if len(tt.before) > 0 {
					select {
					case <-streamed:
					case <-ctx.Done():
						t.Fatal("the continuation's text never streamed")
					}
				}
				if tt.interrupt {
					if _, err := stream.Submit(types.UserMsg(types.Text("new direction")), SubmitInterruptReplace); err != nil {
						t.Fatal(err)
					}
				} else if err := stream.Stop(StopRequest{Reason: "enough"}); err != nil {
					t.Fatal(err)
				}
				<-done
				if err := stream.Wait(); err != nil {
					t.Fatalf("Wait = %v", err)
				}
				checkBranch(t, a, prefill, tt.want...)
			})
		}
	}
}
