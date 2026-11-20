package local

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// echoProvider answers each call with the text of the last user message, so a
// test can see which input a provider call served.
type echoProvider struct{ calls *atomic.Int32 }

func (p echoProvider) Stream(_ context.Context, req types.Request) (<-chan types.Delta, error) {
	m := req.Messages
	p.calls.Add(1)
	last := ""
	for _, msg := range m {
		if u, ok := msg.(types.UserMessage); ok {
			for _, c := range u.Parts {
				if tc, ok := c.(types.TextPart); ok {
					last = tc.Text
				}
			}
		}
	}
	out := make(chan types.Delta, 8)
	for _, d := range agenttest.TextResponse("re: " + last) {
		out <- d
	}
	close(out)
	return out, nil
}

func lastText(t *testing.T, m *types.AssistantMessage) string {
	t.Helper()
	if m == nil {
		t.Fatal("no assistant message")
	}
	var b strings.Builder
	for _, c := range m.Parts {
		if tc, ok := c.(types.TextPart); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestRunAppendsInputWithoutRepeatingEarlierSteps(t *testing.T) {
	ctx := context.Background()
	e := New(t.TempDir())
	var calls atomic.Int32
	factory := func() *agent.Agent { return must.Get(agent.New(agent.Config{Provider: echoProvider{&calls}})) }
	first := []types.Message{types.UserMsg(types.Text("one"))}
	got, err := e.Run(ctx, "run", "v1", factory, first)
	if err != nil {
		t.Fatal(err)
	}
	if lastText(t, got) != "re: one" || calls.Load() != 1 {
		t.Fatalf("first run: %q after %d calls", lastText(t, got), calls.Load())
	}
	extended := append(append([]types.Message(nil), first...), types.UserMsg(types.Text("two")))
	got, err = e.Run(ctx, "run", "v1", factory, extended)
	if err != nil {
		t.Fatal(err)
	}
	if lastText(t, got) != "re: two" || calls.Load() != 2 {
		t.Fatalf("appended run: %q after %d calls", lastText(t, got), calls.Load())
	}
	// Resuming with the original input, or none, replays the whole log.
	for _, input := range [][]types.Message{first, nil, extended} {
		got, err = e.Run(ctx, "run", "v1", factory, input)
		if err != nil {
			t.Fatal(err)
		}
		if lastText(t, got) != "re: two" || calls.Load() != 2 {
			t.Fatalf("replay: %q after %d calls", lastText(t, got), calls.Load())
		}
	}
	state, err := e.Inspect("run")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Inputs) != 1 || state.Inputs[0].Sequence != 1 {
		t.Fatalf("inputs = %+v", state.Inputs)
	}
	if _, ok := state.Steps["llm-main-0"]; !ok {
		t.Fatalf("first segment step missing: %v", state.Steps)
	}
	if _, ok := state.Steps["input-1/llm-main-0"]; !ok {
		t.Fatalf("appended segment step missing: %v", state.Steps)
	}
	diverged := []types.Message{types.UserMsg(types.Text("other"))}
	if _, err := e.Run(ctx, "run", "v1", factory, diverged); !errors.Is(err, ErrConflict) {
		t.Fatalf("divergent input: %v", err)
	}
}

// errAny marks a table case that expects some error without a sentinel.
var errAny = errors.New("any error")
