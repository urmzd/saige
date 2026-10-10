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

func TestReconcileInput(t *testing.T) {
	a, b, c := types.UserMsg(types.Text("a")), types.UserMsg(types.Text("b")), types.UserMsg(types.Text("c"))
	tests := []struct {
		name     string
		log      [][]types.Message
		input    []types.Message
		want     int // total segments after reconciliation
		appended bool
		err      error
	}{
		{name: "same input resumes", log: [][]types.Message{{a}}, input: []types.Message{a}, want: 1},
		{name: "nil input resumes", log: [][]types.Message{{a}, {b}}, input: nil, want: 2},
		{name: "prefix resumes", log: [][]types.Message{{a}, {b}}, input: []types.Message{a}, want: 2},
		{name: "spanning prefix resumes", log: [][]types.Message{{a}, {b, c}}, input: []types.Message{a, b}, want: 2},
		{name: "longer input appends", log: [][]types.Message{{a}}, input: []types.Message{a, b, c}, want: 2, appended: true},
		{name: "divergent input conflicts", log: [][]types.Message{{a}, {b}}, input: []types.Message{a, c}, err: ErrConflict},
		{name: "divergent first message conflicts", log: [][]types.Message{{a}}, input: []types.Message{b}, err: ErrConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first, err := encode(tt.log[0])
			if err != nil {
				t.Fatal(err)
			}
			s := State{Version: 1, Input: first}
			for _, seg := range tt.log[1:] {
				if err := appendSegment(&s, "", seg); err != nil {
					t.Fatal(err)
				}
			}
			before := len(s.Inputs)
			log, err := reconcileInput(&s, tt.input)
			if !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			if tt.err != nil {
				return
			}
			if len(log) != tt.want {
				t.Fatalf("segments = %d, want %d", len(log), tt.want)
			}
			if got := len(s.Inputs) > before; got != tt.appended {
				t.Fatalf("appended = %v, want %v", got, tt.appended)
			}
			stored, err := inputLog(&s)
			if err != nil {
				t.Fatal(err)
			}
			if len(stored) != len(log) {
				t.Fatalf("stored %d segments, returned %d", len(stored), len(log))
			}
		})
	}
}

func TestRunAppendsInputWithoutRepeatingEarlierSteps(t *testing.T) {
	ctx := context.Background()
	e := New(t.TempDir())
	var calls atomic.Int32
	factory := func() *agent.Agent { return agent.NewAgent(agent.AgentConfig{Provider: echoProvider{&calls}}) }
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

func TestAppend(t *testing.T) {
	ctx := context.Background()
	two := []types.Message{types.UserMsg(types.Text("two"))}
	tests := []struct {
		name  string
		setup func(t *testing.T, e *Engine)
		rev   string
		key   string
		msgs  []types.Message
		err   error
		segs  int
	}{
		{name: "appends to a completed run", key: "k", msgs: two, segs: 1},
		{name: "same key and messages is a no-op", key: "k", msgs: two, segs: 1, setup: func(t *testing.T, e *Engine) {
			if err := e.Append("run", "v1", "k", two); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "same key with other messages conflicts", key: "k", msgs: []types.Message{types.UserMsg(types.Text("three"))}, err: ErrConflict, setup: func(t *testing.T, e *Engine) {
			if err := e.Append("run", "v1", "k", two); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "cancelled run is closed", key: "k", msgs: two, err: ErrClosed, setup: func(t *testing.T, e *Engine) {
			if err := e.update("run", "v1", func(s *State) error { s.Status = statusCancelled; return nil }); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "stale revision conflicts", rev: "v2", key: "k", msgs: two, err: ErrConflict},
		{name: "key is required", msgs: two, err: errAny},
		{name: "messages are required", key: "k", err: errAny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := New(t.TempDir())
			var calls atomic.Int32
			factory := func() *agent.Agent { return agent.NewAgent(agent.AgentConfig{Provider: echoProvider{&calls}}) }
			if _, err := e.Run(ctx, "run", "v1", factory, []types.Message{types.UserMsg(types.Text("one"))}); err != nil {
				t.Fatal(err)
			}
			if tt.setup != nil {
				tt.setup(t, e)
			}
			rev := tt.rev
			if rev == "" {
				rev = "v1"
			}
			err := e.Append("run", rev, tt.key, tt.msgs)
			if tt.err == errAny {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			if tt.err != nil {
				return
			}
			state, err := e.Inspect("run")
			if err != nil {
				t.Fatal(err)
			}
			if len(state.Inputs) != tt.segs || state.Status != "ready" {
				t.Fatalf("status %s with %d segments", state.Status, len(state.Inputs))
			}
			got, err := e.Run(ctx, "run", "v1", factory, nil)
			if err != nil {
				t.Fatal(err)
			}
			if lastText(t, got) != "re: two" || calls.Load() != 2 {
				t.Fatalf("queued input: %q after %d calls", lastText(t, got), calls.Load())
			}
		})
	}
}

func TestTruncatedStepCommitsInsteadOfIndeterminate(t *testing.T) {
	partial := &types.AssistantMessage{Parts: []types.AssistantPart{
		types.TextPart{Text: "partial"},
		types.TruncationPart{Reason: "interrupted"},
	}}
	tests := []struct {
		name      string
		result    types.StepResult
		err       error
		replayErr error // what a second RunStep with the same name returns
	}{
		{name: "truncated provider turn is committed", result: types.StepResult{Kind: types.StepKindLLM, Message: partial}, err: context.Canceled},
		{name: "truncated turn with another error stays indeterminate", result: types.StepResult{Kind: types.StepKindLLM, Message: partial}, err: types.ErrResponseTruncated, replayErr: ErrIndeterminate},
		{name: "truncated turn with open tool calls stays indeterminate", result: types.StepResult{Kind: types.StepKindLLM, Message: &types.AssistantMessage{Parts: []types.AssistantPart{
			types.TextPart{Text: "partial"},
			types.ToolCallPart{ID: "call", Name: "write"},
			types.TruncationPart{Reason: "interrupted"},
		}}}, err: context.Canceled, replayErr: ErrIndeterminate},
		{name: "unmarked partial stays indeterminate", result: types.StepResult{Kind: types.StepKindLLM, Message: &types.AssistantMessage{Parts: []types.AssistantPart{types.TextPart{Text: "partial"}}}}, err: context.Canceled, replayErr: ErrIndeterminate},
		{name: "cancelled tool stays indeterminate", result: types.StepResult{Kind: types.StepKindTool, ToolCallID: "c"}, err: context.Canceled, replayErr: ErrIndeterminate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := New(t.TempDir())
			path, release, err := e.acquire("run")
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			r := &runner{path: path, state: State{Version: 1, RunID: "run", Revision: "v1", Steps: map[string]Step{}, Interrupts: map[string]Interrupt{}}}
			got, err := r.RunStep(context.Background(), "llm-main-0", func(context.Context) (types.StepResult, error) { return tt.result, tt.err })
			if !errors.Is(err, tt.err) {
				t.Fatalf("first err = %v, want %v", err, tt.err)
			}
			replayed, replayErr := r.RunStep(context.Background(), "llm-main-0", func(context.Context) (types.StepResult, error) {
				t.Fatal("step ran again")
				return types.StepResult{}, nil
			})
			if !errors.Is(replayErr, tt.replayErr) || (tt.replayErr == nil && replayErr != nil) {
				t.Fatalf("replay err = %v, want %v", replayErr, tt.replayErr)
			}
			if tt.replayErr != nil {
				return
			}
			if lastText(t, got.Message) != "partial" || lastText(t, replayed.Message) != "partial" {
				t.Fatalf("partial text lost: %+v / %+v", got.Message, replayed.Message)
			}
			if !truncatedLLM(replayed) {
				t.Fatal("truncation marker lost on replay")
			}
			if r.state.Steps["llm-main-0"].Status != statusCompleted {
				t.Fatalf("status = %s", r.state.Steps["llm-main-0"].Status)
			}
		})
	}
}
