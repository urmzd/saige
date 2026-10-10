package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

// hookLog records the hook events a run produced, in order.
type hookLog struct {
	mu     sync.Mutex
	events []string
}

func (l *hookLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, s)
}

func (l *hookLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

// recordingHooks returns a hook set that logs every event under name.
func recordingHooks(name string, log *hookLog) Hooks {
	at := func(event HookEvent) { log.add(name + ":" + string(event)) }
	return Hooks{
		Name:              name,
		RunStart:          func(context.Context, *RunStartEvent) error { at(HookRunStart); return nil },
		UserInput:         func(context.Context, *UserInputEvent) error { at(HookUserInput); return nil },
		BeforeCompaction:  func(context.Context, *CompactionEvent) error { at(HookBeforeCompaction); return nil },
		AfterCompaction:   func(context.Context, *CompactionEvent) error { at(HookAfterCompaction); return nil },
		BeforeModelCall:   func(context.Context, *BeforeModelCallEvent) error { at(HookBeforeModelCall); return nil },
		AfterModelCall:    func(context.Context, *AfterModelCallEvent) error { at(HookAfterModelCall); return nil },
		BeforeTool:        func(context.Context, *BeforeToolEvent) error { at(HookBeforeTool); return nil },
		AfterTool:         func(context.Context, *AfterToolEvent) error { at(HookAfterTool); return nil },
		SubagentStart:     func(context.Context, *SubagentStartEvent) error { at(HookSubagentStart); return nil },
		SubagentEnd:       func(context.Context, *SubagentEndEvent) error { at(HookSubagentEnd); return nil },
		InterruptRaised:   func(context.Context, *InterruptEvent) error { at(HookInterruptRaised); return nil },
		InterruptResolved: func(context.Context, *InterruptEvent) error { at(HookInterruptResolved); return nil },
		TurnEnd:           func(context.Context, *TurnEndEvent) error { at(HookTurnEnd); return nil },
		RunStop:           func(context.Context, *RunStopEvent) error { at(HookRunStop); return nil },
	}
}

func hookTool(name string) *agenttest.MockTool {
	return &agenttest.MockTool{
		Def: types.ToolDef{Name: name, Parameters: types.ParameterSchema{Type: types.SchemaObject, Properties: map[string]types.PropertyDef{
			"q": {Type: types.SchemaString},
		}}},
		Result: "tool says hi",
	}
}

func toolThenText(text string) *agenttest.ScriptedProvider {
	return &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("call-1", "lookup", map[string]any{"q": "x"}),
		agenttest.TextResponse(text),
	}}
}

func runText(t *testing.T, a *Agent, input string) (Transcript, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return Collect(a.Invoke(ctx, []types.Message{types.UserMsg(types.Text(input))}), nil)
}

func TestHooksRunInOrder(t *testing.T) {
	log := &hookLog{}
	a := NewAgent(AgentConfig{
		Name: "root", SystemPrompt: "s", Provider: toolThenText("done"),
		Tools: types.NewToolRegistry(hookTool("lookup")),
	}, WithHooks(recordingHooks("a", log), recordingHooks("b", log)))

	if _, err := runText(t, a, "hi"); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, e := range []HookEvent{
		HookRunStart, HookUserInput,
		HookBeforeModelCall, HookAfterModelCall, HookBeforeTool, HookAfterTool, HookTurnEnd,
		HookBeforeModelCall, HookAfterModelCall, HookTurnEnd,
		HookRunStop,
	} {
		want = append(want, "a:"+string(e), "b:"+string(e))
	}
	if got := log.list(); !slices.Equal(got, want) {
		t.Fatalf("hook order\n got: %v\nwant: %v", got, want)
	}
}

func TestHookEventsCarryRequestAndOutcome(t *testing.T) {
	var (
		mu     sync.Mutex
		before []BeforeModelCallEvent
		after  []AfterModelCallEvent
		stop   RunStopEvent
		final  TurnEndEvent
	)
	p := toolThenText("done")
	p.Responses[1] = append(p.Responses[1], types.UsageDelta{PromptTokens: 7, CompletionTokens: 3})
	a := NewAgent(AgentConfig{Name: "root", SystemPrompt: "s", Provider: p, Tools: types.NewToolRegistry(hookTool("lookup"))},
		WithToolChoice(types.ToolChoice{Mode: types.ToolChoiceRequired}),
		WithHooks(Hooks{
			BeforeModelCall: func(_ context.Context, ev *BeforeModelCallEvent) error {
				mu.Lock()
				defer mu.Unlock()
				before = append(before, *ev)
				return nil
			},
			AfterModelCall: func(_ context.Context, ev *AfterModelCallEvent) error {
				mu.Lock()
				defer mu.Unlock()
				after = append(after, *ev)
				return nil
			},
			TurnEnd: func(_ context.Context, ev *TurnEndEvent) error {
				if ev.Final {
					final = *ev
				}
				return nil
			},
			RunStop: func(_ context.Context, ev *RunStopEvent) error { stop = *ev; return nil },
		}))
	if _, err := runText(t, a, "hi"); err != nil {
		t.Fatal(err)
	}
	if len(before) != 2 || len(after) != 2 {
		t.Fatalf("model call events: %d before, %d after", len(before), len(after))
	}
	if before[0].Options == nil || before[0].Options.ToolChoice == nil || before[0].Options.ToolChoice.Mode != types.ToolChoiceRequired {
		t.Errorf("BeforeModelCall should see the forced tool choice, got %+v", before[0].Options)
	}
	if before[0].Step != "llm-main-0" || len(before[0].Tools) != 1 {
		t.Errorf("BeforeModelCall step %q tools %d", before[0].Step, len(before[0].Tools))
	}
	if after[1].Usage.PromptTokens != 7 || after[1].Message == nil || after[1].Replayed {
		t.Errorf("AfterModelCall = %+v", after[1])
	}
	if !final.Final || assistantText(&final.Message) != "done" {
		t.Errorf("final TurnEnd = %+v", final)
	}
	if stop.Reason != RunStopCompleted || stop.Err != nil || len(stop.Messages) < 4 || stop.Agent != "root" || stop.RunID == "" {
		t.Errorf("RunStop = %+v", stop)
	}
}

// Every abortable point stops the run with a *HookAbortError and leaves
// each tool call paired with a result.
func TestHookAbortStopsTheRun(t *testing.T) {
	abort := func(context.Context, any) error { return Abort("policy says no") }
	cases := []struct {
		event HookEvent
		hooks Hooks
	}{
		{HookRunStart, Hooks{RunStart: func(ctx context.Context, ev *RunStartEvent) error { return abort(ctx, ev) }}},
		{HookUserInput, Hooks{UserInput: func(ctx context.Context, ev *UserInputEvent) error { return abort(ctx, ev) }}},
		{HookBeforeModelCall, Hooks{BeforeModelCall: func(ctx context.Context, ev *BeforeModelCallEvent) error { return abort(ctx, ev) }}},
		{HookBeforeTool, Hooks{BeforeTool: func(ctx context.Context, ev *BeforeToolEvent) error { return abort(ctx, ev) }}},
		{HookAfterTool, Hooks{AfterTool: func(ctx context.Context, ev *AfterToolEvent) error { return abort(ctx, ev) }}},
		{HookTurnEnd, Hooks{TurnEnd: func(ctx context.Context, ev *TurnEndEvent) error { return abort(ctx, ev) }}},
	}
	for _, tc := range cases {
		t.Run(string(tc.event), func(t *testing.T) {
			tc.hooks.Name = "guard"
			var stop RunStopEvent
			p := toolThenText("done")
			a := NewAgent(AgentConfig{SystemPrompt: "s", Provider: p, Tools: types.NewToolRegistry(hookTool("lookup"))},
				WithHooks(tc.hooks, Hooks{RunStop: func(_ context.Context, ev *RunStopEvent) error { stop = *ev; return nil }}))
			_, err := runText(t, a, "hi")
			var ab *HookAbortError
			if !errors.Is(err, ErrHookAborted) || !errors.As(err, &ab) {
				t.Fatalf("err = %v, want a hook abort", err)
			}
			if ab.Event != tc.event || ab.Hook != "guard" || ab.Reason != "policy says no" {
				t.Errorf("abort = %+v", ab)
			}
			if stop.Reason != RunStopAborted {
				t.Errorf("RunStop reason = %q, want aborted", stop.Reason)
			}
			msgs, err := a.Tree().FlattenBranch(a.Tree().Active())
			if err != nil {
				t.Fatal(err)
			}
			if err := toolPairingError(msgs); err != nil {
				t.Errorf("tool pairing broken: %v", err)
			}
			if p.CallCount() > 1 {
				t.Errorf("the model was called %d times after the abort", p.CallCount())
			}
		})
	}
}

// Observing hooks cannot stop the run: their errors and panics are logged.
func TestObservingHookErrorsDoNotStopTheRun(t *testing.T) {
	a := NewAgent(AgentConfig{SystemPrompt: "s", Provider: &mockProvider{response: "ok"}},
		WithHooks(Hooks{
			AfterModelCall: func(context.Context, *AfterModelCallEvent) error { return Abort("ignored") },
			RunStop:        func(context.Context, *RunStopEvent) error { panic("boom") },
		}))
	tr, err := runText(t, a, "hi")
	if err != nil || tr.Text != "ok" {
		t.Fatalf("text %q err %v", tr.Text, err)
	}
}

func TestHooksChangeInputArgumentsAndResults(t *testing.T) {
	p := toolThenText("done")
	tool := hookTool("lookup")
	a := NewAgent(AgentConfig{SystemPrompt: "s", Provider: p, Tools: types.NewToolRegistry(tool)},
		WithHooks(
			Hooks{UserInput: func(_ context.Context, ev *UserInputEvent) error {
				ev.Message = types.UserMsg(types.Text(userText(ev.Message) + " [annotated]"))
				return nil
			}},
			Hooks{
				// Hooks see the changes of the hooks before them.
				UserInput: func(_ context.Context, ev *UserInputEvent) error {
					if !strings.HasSuffix(userText(ev.Message), "[annotated]") {
						return errors.New("second hook did not see the first one's change")
					}
					return nil
				},
				BeforeTool: func(_ context.Context, ev *BeforeToolEvent) error {
					ev.Arguments = map[string]any{"q": "narrowed"}
					return nil
				},
				AfterTool: func(_ context.Context, ev *AfterToolEvent) error {
					ev.Result += " (checked)"
					return nil
				},
			},
		))
	if _, err := runText(t, a, "hi"); err != nil {
		t.Fatal(err)
	}
	first := p.Requests()[0].Messages
	if got := userText(first[len(first)-1].(types.UserMessage)); got != "hi [annotated]" {
		t.Errorf("model saw %q", got)
	}
	if tool.CallCount() != 1 || tool.Calls[0]["q"] != "narrowed" {
		t.Errorf("tool ran with %v", tool.Calls)
	}
	res := resultsIn(p.Requests()[1].Messages)
	if len(res) != 1 || res[0].Text() != "tool says hi (checked)" {
		t.Errorf("recorded results %+v", res)
	}
}

func TestBeforeToolArgumentsAreValidatedAgain(t *testing.T) {
	a := NewAgent(AgentConfig{SystemPrompt: "s", Provider: toolThenText("done"), Tools: types.NewToolRegistry(hookTool("lookup"))},
		WithHooks(Hooks{BeforeTool: func(_ context.Context, ev *BeforeToolEvent) error {
			ev.Arguments = map[string]any{"q": 42}
			return nil
		}}))
	if _, err := runText(t, a, "hi"); err != nil {
		t.Fatal(err)
	}
	msgs, _ := a.Tree().FlattenBranch(a.Tree().Active())
	res := resultsIn(msgs)
	if len(res) != 1 || !res[0].IsError {
		t.Errorf("a call with arguments that do not fit the schema ran: %+v", res)
	}
}

// resultsIn returns the tool results in msgs, in order.
func resultsIn(msgs []types.Message) []types.ToolResultPart {
	var out []types.ToolResultPart
	for _, m := range msgs {
		if sm, ok := m.(types.SystemMessage); ok {
			for _, c := range sm.Parts {
				if r, ok := c.(types.ToolResultPart); ok {
					out = append(out, r)
				}
			}
		}
	}
	return out
}

func TestHookTimeoutAborts(t *testing.T) {
	a := NewAgent(AgentConfig{SystemPrompt: "s", Provider: &mockProvider{response: "ok"}},
		WithHookTimeout(20*time.Millisecond),
		WithHooks(Hooks{BeforeModelCall: func(ctx context.Context, _ *BeforeModelCallEvent) error {
			<-ctx.Done()
			return nil
		}}))
	_, err := runText(t, a, "hi")
	if !errors.Is(err, ErrHookTimeout) || !errors.Is(err, ErrHookAborted) {
		t.Fatalf("err = %v, want a hook timeout abort", err)
	}
}

func TestHookPanicAborts(t *testing.T) {
	a := NewAgent(AgentConfig{SystemPrompt: "s", Provider: &mockProvider{response: "ok"}},
		WithHooks(Hooks{RunStart: func(context.Context, *RunStartEvent) error { panic("bad hook") }}))
	_, err := runText(t, a, "hi")
	if !errors.Is(err, ErrHookAborted) || !strings.Contains(err.Error(), "bad hook") {
		t.Fatalf("err = %v", err)
	}
}

func TestCompactionHooksCanSkip(t *testing.T) {
	for _, skip := range []bool{false, true} {
		t.Run(fmt.Sprintf("skip=%v", skip), func(t *testing.T) {
			var before, after []CompactionEvent
			p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
				agenttest.ToolCallResponse("c1", "lookup", map[string]any{"q": "a"}),
				agenttest.ToolCallResponse("c2", "lookup", map[string]any{"q": "b"}),
				agenttest.TextResponse("done"),
			}}
			a := NewAgent(AgentConfig{SystemPrompt: "s", Provider: p, Tools: types.NewToolRegistry(hookTool("lookup")),
				CompactCfg: &types.CompactConfig{Strategy: types.CompactSlidingWindow, WindowSize: 3}},
				WithHooks(Hooks{
					BeforeCompaction: func(_ context.Context, ev *CompactionEvent) error {
						before = append(before, *ev)
						ev.Skip = skip
						return nil
					},
					AfterCompaction: func(_ context.Context, ev *CompactionEvent) error {
						after = append(after, *ev)
						return nil
					},
				}))
			if _, err := runText(t, a, "hi"); err != nil {
				t.Fatal(err)
			}
			if len(before) == 0 {
				t.Fatal("BeforeCompaction was not called")
			}
			switch {
			case skip && (len(after) != 0 || a.Tree().Active() != "main"):
				t.Errorf("a skipped compaction ran: after=%v active=%s", after, a.Tree().Active())
			case !skip && !slices.ContainsFunc(after, func(ev CompactionEvent) bool { return ev.Compacted && ev.NewBranch != "" }):
				t.Errorf("AfterCompaction = %+v", after)
			}
		})
	}
}

func TestInterruptHooksSeeApprovals(t *testing.T) {
	var (
		mu     sync.Mutex
		events []InterruptEvent
	)
	record := func(_ context.Context, ev *InterruptEvent) error {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, *ev)
		return nil
	}
	tool := &types.MarkedTool{Inner: hookTool("lookup"), Markers: []types.Marker{{Kind: "human_approval", Message: "ok?"}}}
	a := NewAgent(AgentConfig{SystemPrompt: "s", Provider: toolThenText("done"), Tools: types.NewToolRegistry(tool)},
		WithHooks(Hooks{InterruptRaised: record, InterruptResolved: record}))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("hi"))})
	for d := range stream.Deltas() {
		if m, ok := d.(types.MarkerDelta); ok {
			if err := stream.ResolveMarkerErr(m.ToolCallID, Resolution{Approved: true, Approver: "ana"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Approved || !events[1].Approved || events[1].Approver != "ana" ||
		events[0].Interrupt.ID == "" || events[0].Interrupt.ID != events[1].Interrupt.ID {
		t.Fatalf("interrupt events = %+v", events)
	}
}

// Hook outcomes that change the run are recorded: a replay applies them
// without calling the hooks, so hooks that would now decide otherwise do
// not change the replayed run.
func TestHooksAreDeterministicUnderDurableReplay(t *testing.T) {
	runner := newRecordingRunner()
	calls, replaying := 0, false
	build := func(p types.Provider) *Agent {
		return NewAgent(AgentConfig{SystemPrompt: "s", Provider: p, Tools: types.NewToolRegistry(hookTool("lookup"))},
			WithHooks(Hooks{
				UserInput: func(_ context.Context, ev *UserInputEvent) error {
					calls++
					ev.Message = types.UserMsg(types.Text(fmt.Sprintf("%s #%d", userText(ev.Message), calls)))
					return nil
				},
				BeforeTool: func(_ context.Context, ev *BeforeToolEvent) error {
					calls++
					ev.Arguments = map[string]any{"q": fmt.Sprintf("v%d", calls)}
					return nil
				},
				TurnEnd: func(_ context.Context, ev *TurnEndEvent) error {
					calls++
					if ev.Final && replaying {
						return Abort("changed its mind")
					}
					return nil
				},
			}))
	}
	input := []types.Message{types.UserMsg(types.Text("hi"))}

	first := build(toolThenText("done"))
	final, err := first.RunDurable(context.Background(), runner, input, "")
	if err != nil {
		t.Fatal(err)
	}
	if assistantText(final) != "done" {
		t.Fatalf("final %q", assistantText(final))
	}
	if !runner.has("hook-user_input-0") || !runner.has("hook-before_tool-call-1") {
		t.Fatal("hook outcomes were not recorded as steps")
	}
	recorded := calls
	replaying = true

	replay := build(panicProvider{})
	final, err = replay.RunDurable(context.Background(), runner, input, "")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if calls != recorded {
		t.Errorf("hooks ran %d more times on replay", calls-recorded)
	}
	if assistantText(final) != "done" {
		t.Errorf("replayed final %q", assistantText(final))
	}
	want, _ := first.Tree().FlattenBranch(first.Tree().Active())
	got, _ := replay.Tree().FlattenBranch(replay.Tree().Active())
	if fmt.Sprint(stripRoutes(got)) != fmt.Sprint(stripRoutes(want)) {
		t.Errorf("replayed transcript differs\n got: %v\nwant: %v", got, want)
	}

	// A recorded abort replays as an abort.
	abortRunner := newRecordingRunner()
	aborting := NewAgent(AgentConfig{SystemPrompt: "s", Provider: &mockProvider{response: "x"}},
		WithHooks(Hooks{Name: "once", TurnEnd: func(context.Context, *TurnEndEvent) error { return Abort("first run") }}))
	if _, err := aborting.RunDurable(context.Background(), abortRunner, input, ""); !errors.Is(err, ErrHookAborted) {
		t.Fatalf("err = %v", err)
	}
	relaxed := NewAgent(AgentConfig{SystemPrompt: "s", Provider: panicProvider{}},
		WithHooks(Hooks{TurnEnd: func(context.Context, *TurnEndEvent) error { return nil }}))
	_, err = relaxed.RunDurable(context.Background(), abortRunner, input, "")
	var ab *HookAbortError
	if !errors.As(err, &ab) || ab.Hook != "once" || ab.Reason != "first run" {
		t.Fatalf("replayed err = %v, want the recorded abort", err)
	}
}

// stripRoutes drops route metadata, which carries no hook outcome.
func stripRoutes(msgs []types.Message) []types.Message {
	out := make([]types.Message, 0, len(msgs))
	for _, m := range msgs {
		if am, ok := m.(types.AssistantMessage); ok {
			am.Parts = slices.DeleteFunc(slices.Clone(am.Parts), func(c types.AssistantPart) bool {
				_, route := c.(types.RoutePart)
				return route
			})
			m = am
		}
		out = append(out, m)
	}
	return out
}

func TestHooksAndGuardrailsAreInherited(t *testing.T) {
	parent := AgentConfig{
		Name: "parent", Provider: &namedProvider{id: "p"},
		Hooks:            []Hooks{{Name: "audit"}},
		HookTimeout:      time.Second,
		InputGuardrails:  []InputGuardrail{{Guardrail: NewGuardrail("in", nil)}},
		OutputGuardrails: []OutputGuardrail{{Guardrail: NewGuardrail("out", nil)}},
	}
	child := childConfig(t, parent, SubAgentDef{Name: "worker", Description: "w",
		Options: []AgentOption{WithHooks(Hooks{Name: "own"})}})
	if len(child.Hooks) != 2 || child.Hooks[0].Name != "audit" || child.Hooks[1].Name != "own" {
		t.Errorf("child hooks = %v, want the parent's then its own", child.Hooks)
	}
	if child.HookTimeout != time.Second || len(child.InputGuardrails) != 1 || len(child.OutputGuardrails) != 1 {
		t.Errorf("child guardrails or hook timeout not inherited: %+v", child)
	}
}

func TestSubagentHooksAndChildRunHooks(t *testing.T) {
	log := &hookLog{}
	var childStart RunStartEvent
	parentProvider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("call-d", "delegate_to_worker", map[string]any{"task": "do it"}),
		agenttest.TextResponse("all done"),
	}}
	childProvider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("child result")}}
	a := NewAgent(AgentConfig{Name: "parent", SystemPrompt: "s", Provider: parentProvider,
		SubAgents: []SubAgentDef{{Name: "worker", Description: "w", Provider: childProvider}},
		Hooks: []Hooks{{
			SubagentStart: func(_ context.Context, ev *SubagentStartEvent) error {
				log.add("start:" + ev.Name + ":" + ev.Task)
				ev.Task = "do it carefully"
				return nil
			},
			SubagentEnd: func(_ context.Context, ev *SubagentEndEvent) error {
				log.add("end:" + ev.Name + ":" + ev.Output)
				return nil
			},
			RunStart: func(_ context.Context, ev *RunStartEvent) error {
				if len(ev.Path) > 0 {
					childStart = *ev
				}
				return nil
			},
		}},
	})
	if _, err := runText(t, a, "hi"); err != nil {
		t.Fatal(err)
	}
	if got := log.list(); !slices.Equal(got, []string{"start:worker:do it", "end:worker:child result"}) {
		t.Errorf("subagent events = %v", got)
	}
	if childStart.Agent != "worker" || !slices.Equal(childStart.Path, []string{"call-d"}) {
		t.Errorf("child RunStart = %+v", childStart)
	}
	seed := childProvider.Requests()[0].Messages
	if !strings.Contains(userText(seed[len(seed)-1].(types.UserMessage)), "do it carefully") {
		t.Errorf("child did not get the changed task: %v", seed[len(seed)-1])
	}
}

func TestHookAbortErrorCrossesTheWire(t *testing.T) {
	for _, err := range []error{
		&HookAbortError{Event: HookTurnEnd, Hook: "h", Reason: "r"},
		&GuardrailTrippedError{Guardrail: "g", Phase: types.GuardrailPhaseInput, Reason: "r"},
	} {
		b, mErr := types.MarshalDelta(types.ErrorDelta{Error: err})
		if mErr != nil {
			t.Fatal(mErr)
		}
		d, uErr := types.UnmarshalDelta(b)
		if uErr != nil {
			t.Fatal(uErr)
		}
		got := d.(types.ErrorDelta).Error
		if !errors.Is(got, ErrHookAborted) && !errors.Is(got, ErrGuardrailTripped) {
			t.Errorf("%T lost its code on the wire: %v", err, got)
		}
	}
}
