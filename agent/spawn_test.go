package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// holdTool waits until release is closed or its context ends.
func holdTool(name string, release <-chan struct{}) types.Tool {
	return &types.ToolFunc{Def: types.ToolDef{Name: name}, Fn: func(ctx context.Context, _ map[string]any) (string, error) {
		select {
		case <-release:
			return "released", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}}
}

// spawnRun is the outcome of one parent run.
type spawnRun struct {
	stream *EventStream
	deltas []types.Delta
	err    error
}

// toolEnd returns the parent's result for the tool call id.
func (r spawnRun) toolEnd(id string) types.ToolExecEndDelta {
	for _, d := range r.deltas {
		if e, ok := d.(types.ToolExecEndDelta); ok && e.ToolCallID == id {
			return e
		}
	}
	return types.ToolExecEndDelta{}
}

func (r spawnRun) injected() []types.InjectedDelta {
	var out []types.InjectedDelta
	for _, d := range r.deltas {
		if in, ok := d.(types.InjectedDelta); ok && in.Mode == "subagent" {
			out = append(out, in)
		}
	}
	return out
}

// runSpawn runs a parent agent with one spawn definition named "worker" to
// completion. onDelta sees every parent delta as it arrives.
func runSpawn(t *testing.T, parent, child *agenttest.ScriptedProvider, def SubAgentDef, onDelta func(*EventStream, types.Delta), opts ...Option) spawnRun {
	t.Helper()
	def.Name, def.Provider, def.Mode = "worker", child, SubAgentSpawn
	a := must.Get(New(Config{Name: "lead", Provider: parent, SubAgents: []SubAgentDef{def}}, opts...))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("start"))})
	run := spawnRun{stream: stream}
	for d := range stream.Deltas() {
		run.deltas = append(run.deltas, d)
		if onDelta != nil {
			onDelta(stream, d)
		}
	}
	run.err = stream.Wait()
	return run
}

// lastUserText returns the text of the last user message of request i.
func lastUserText(p *agenttest.ScriptedProvider, i int) string {
	reqs := p.Requests()
	if i >= len(reqs) {
		return ""
	}
	texts := userTexts(reqs[i].Messages)
	if len(texts) == 0 {
		return ""
	}
	return texts[len(texts)-1]
}

func TestSpawnResultInjectedAtFinish(t *testing.T) {
	parent := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("s1", "spawn_worker", map[string]any{"task": "research"}),
		agenttest.TextResponse("waiting for the worker"),
		agenttest.TextResponse("final answer"),
	}}
	// The child holds until the parent's second turn is streaming, so its
	// result can only arrive at the finish of that turn.
	release := make(chan struct{})
	child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("hold", "hold", nil),
		agenttest.TextResponse("worker found it"),
	}}
	def := SubAgentDef{Tools: types.NewToolRegistry(holdTool("hold", release))}
	var once sync.Once
	run := runSpawn(t, parent, child, def, func(_ *EventStream, d types.Delta) {
		if pd, ok := d.(types.PartDelta); ok && pd.Text != "" {
			once.Do(func() { close(release) })
		}
	})
	if run.err != nil {
		t.Fatal(run.err)
	}
	var receipt spawnReceipt
	if err := json.Unmarshal([]byte(run.toolEnd("s1").Result), &receipt); err != nil || receipt.Handle != "s1" || receipt.Status != HandleRunning {
		t.Fatalf("spawn receipt = %q (%v)", run.toolEnd("s1").Result, err)
	}
	if got := run.injected(); len(got) != 1 || got[0].SubmissionID != "s1" {
		t.Fatalf("injected = %+v", got)
	}
	if parent.CallCount() != 3 {
		t.Fatalf("parent calls = %d, want 3", parent.CallCount())
	}
	msg := lastUserText(parent, 2)
	for _, want := range []string{`<subagent_result handle="s1" name="worker" status="completed" iterations="2">`, "worker found it"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("injected message %q lacks %q", msg, want)
		}
	}
	handles := run.stream.SubAgents()
	if len(handles) != 1 || handles[0].Status() != HandleCompleted {
		t.Fatalf("handles = %+v", handles)
	}
	// Child deltas reach the parent stream under the spawn call ID.
	forwarded := false
	for _, d := range run.deltas {
		if ex, ok := d.(types.ToolExecDelta); ok && ex.ToolCallID == "s1" {
			forwarded = true
		}
	}
	if !forwarded {
		t.Fatal("child deltas were not forwarded")
	}
}

func TestAwaitSubAgentDeliversOnce(t *testing.T) {
	parent := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("s1", "spawn_worker", map[string]any{"task": "research"}),
		agenttest.ToolCallResponse("w1", AwaitSubAgentTool, map[string]any{"handle": "s1"}),
		agenttest.TextResponse("final answer"),
	}}
	child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("worker found it")}}
	run := runSpawn(t, parent, child, SubAgentDef{}, nil)
	if run.err != nil {
		t.Fatal(run.err)
	}
	if end := run.toolEnd("w1"); end.Result != "worker found it" || end.Error != "" {
		t.Fatalf("await result = %+v", end)
	}
	if got := run.injected(); len(got) != 0 {
		t.Fatalf("an awaited result was injected again: %+v", got)
	}
	if parent.CallCount() != 3 {
		t.Fatalf("parent calls = %d, want 3", parent.CallCount())
	}
}

func TestHandleToolErrors(t *testing.T) {
	tests := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{"await unknown handle", AwaitSubAgentTool, map[string]any{"handle": "nope"}, ErrUnknownHandle.Error()},
		{"send unknown handle", SendSubAgentTool, map[string]any{"handle": "nope", "message": "x"}, ErrUnknownHandle.Error()},
		{"cancel unknown handle", CancelSubAgentTool, map[string]any{"handle": "nope"}, ErrUnknownHandle.Error()},
		{"search unknown handle", SearchSubAgentTool, map[string]any{"handle": "nope", "query": "x"}, ErrUnknownHandle.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
				agenttest.ToolCallResponse("c1", tt.tool, tt.args),
				agenttest.TextResponse("done"),
			}}
			run := runSpawn(t, parent, &agenttest.ScriptedProvider{}, SubAgentDef{}, nil)
			if run.err != nil {
				t.Fatal(run.err)
			}
			if end := run.toolEnd("c1"); !strings.Contains(end.Error, tt.want) {
				t.Fatalf("result = %+v, want error %q", end, tt.want)
			}
		})
	}
	// Outside the agent loop the handle tools have no registry.
	reg := types.NewToolRegistry()
	registerSubAgentControls(reg)
	tool, _ := reg.Get(ListSubAgentsTool)
	if _, err := tool.Execute(context.Background(), nil); !errors.Is(err, ErrSpawnUnsupported) {
		t.Fatalf("outside the loop: %v", err)
	}
}

func TestSendSubAgentSteersChild(t *testing.T) {
	release := make(chan struct{})
	parent := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("s1", "spawn_worker", map[string]any{"task": "measure"}),
		agenttest.ToolCallResponse("m1", SendSubAgentTool, map[string]any{"handle": "s1", "message": "use metric units"}),
		agenttest.TextResponse("waiting"),
		agenttest.TextResponse("final"),
	}}
	child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("hold", "hold", nil),
		agenttest.TextResponse("42 metres"),
	}}
	def := SubAgentDef{Tools: types.NewToolRegistry(holdTool("hold", release))}
	run := runSpawn(t, parent, child, def, func(_ *EventStream, d types.Delta) {
		if e, ok := d.(types.ToolExecEndDelta); ok && e.ToolCallID == "m1" {
			close(release)
		}
	})
	if run.err != nil {
		t.Fatal(run.err)
	}
	if end := run.toolEnd("m1"); end.Error != "" {
		t.Fatalf("send failed: %+v", end)
	}
	if got := lastUserText(child, 1); got != "use metric units" {
		t.Fatalf("child's second request ends with %q", got)
	}
}

func TestCancelSubAgent(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	parent := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("s1", "spawn_worker", map[string]any{"task": "slow"}),
		agenttest.ToolCallResponse("x1", CancelSubAgentTool, map[string]any{"handle": "s1"}),
		agenttest.TextResponse("final"),
	}}
	child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.ToolCallResponse("hold", "hold", nil)}}
	def := SubAgentDef{Tools: types.NewToolRegistry(holdTool("hold", release))}
	run := runSpawn(t, parent, child, def, nil)
	if run.err != nil {
		t.Fatal(run.err)
	}
	if got := run.injected(); len(got) != 0 {
		t.Fatalf("a cancelled result was injected: %+v", got)
	}
	if parent.CallCount() != 3 {
		t.Fatalf("parent calls = %d, want 3", parent.CallCount())
	}
	if h := run.stream.SubAgents(); len(h) != 1 || h[0].Status() != HandleCanceled {
		t.Fatalf("handle status = %v", h[0].Status())
	}
}

func TestRunEndCancelsSpawnedChildren(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	parent := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		append(agenttest.ToolCallResponse("s1", "spawn_worker", map[string]any{"task": "slow"}),
			agenttest.ToolCallResponse("stop", "submit", nil)...),
	}}
	child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.ToolCallResponse("hold", "hold", nil)}}
	def := SubAgentDef{Tools: types.NewToolRegistry(holdTool("hold", release))}
	submit := &agenttest.MockTool{Def: types.ToolDef{Name: "submit"}, Result: "ok"}
	run := runSpawn(t, parent, child, def, nil, func(c *Config) {
		c.Tools = types.NewToolRegistry(submit)
		c.StopAtTools = []string{"submit"}
	})
	if run.err != nil {
		t.Fatal(run.err)
	}
	h := run.stream.SubAgents()
	if len(h) != 1 {
		t.Fatalf("handles = %d", len(h))
	}
	select {
	case <-h[0].Done():
	default:
		t.Fatal("the run ended with its child still running")
	}
	if h[0].Status() != HandleCanceled {
		t.Fatalf("child status = %v", h[0].Status())
	}
}

func TestSpawnBudgetAdmission(t *testing.T) {
	// Three requests in all: the parent's first turn uses one, the first
	// spawn holds one, and the second spawn would need a fourth.
	budget := types.NewBudget(types.BudgetPolicy{MaxRequests: 3})
	parent := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		append(agenttest.ToolCallResponse("s1", "spawn_worker", map[string]any{"task": "a"}),
			append(agenttest.ToolCallResponse("s2", "spawn_worker", map[string]any{"task": "b"}),
				agenttest.ToolCallResponse("s3", "spawn_worker", map[string]any{"task": "c"})...)...),
		agenttest.TextResponse("done"),
	}}
	child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("x"), agenttest.TextResponse("y")}}
	run := runSpawn(t, parent, child, SubAgentDef{}, nil, WithBudget(budget), WithSequentialTools())
	// The third spawn is refused either while the first two children hold
	// their reservations (busy) or after they settled them (exceeded).
	admitted := 0
	for _, id := range []string{"s1", "s2", "s3"} {
		end := run.toolEnd(id)
		switch {
		case end.Error == "":
			admitted++
		case !strings.Contains(end.Error, types.ErrBudgetBusy.Error()) && !strings.Contains(end.Error, types.ErrBudgetExceeded.Error()):
			t.Fatalf("%s: %+v", id, end)
		}
	}
	if admitted != 2 {
		t.Fatalf("admitted %d spawns, want 2", admitted)
	}
	if budget.Uncertain() == 0 && budget.Usage().Requests == 0 {
		t.Fatal("budget recorded nothing")
	}
}

func TestChildAdmissionRelease(t *testing.T) {
	budget := types.NewBudget(types.BudgetPolicy{MaxRequests: 1})
	child := must.Get(New(Config{Provider: &agenttest.ScriptedProvider{}, Budget: budget}))
	admission, err := admitChild(child, "h1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := budget.Reserve("other", types.Pricing{}); !errors.Is(err, types.ErrBudgetBusy) {
		t.Fatalf("held admission did not occupy the allowance: %v", err)
	}
	admission.release(budget)
	admission.release(budget) // a second release is a no-op
	if _, err := budget.Reserve("other", types.Pricing{}); err != nil {
		t.Fatalf("released admission still held: %v", err)
	}
	if budget.Usage().Requests != 0 {
		t.Fatalf("an unused admission was charged: %+v", budget.Usage())
	}
}

func TestSearchAndReadSubAgentTranscript(t *testing.T) {
	parent := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("s1", "spawn_worker", map[string]any{"task": "find the capital"}),
		agenttest.ToolCallResponse("w1", AwaitSubAgentTool, map[string]any{"handle": "s1"}),
		append(agenttest.ToolCallResponse("q1", SearchSubAgentTool, map[string]any{"handle": "s1", "query": "Paris"}),
			append(agenttest.ToolCallResponse("r1", ReadSubAgentTool, map[string]any{"handle": "s1", "index": 2, "window": 1}),
				agenttest.ToolCallResponse("l1", ListSubAgentsTool, nil)...)...),
		agenttest.TextResponse("done"),
	}}
	child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("The capital is Paris.")}}
	run := runSpawn(t, parent, child, SubAgentDef{}, nil)
	if run.err != nil {
		t.Fatal(run.err)
	}
	tests := []struct {
		id   string
		want []string
	}{
		{"q1", []string{"[2] assistant: The capital is Paris."}},
		{"r1", []string{"[2] assistant:", "The capital is Paris.", "(messages 2 to 2 of 3)"}},
		{"l1", []string{`"handle":"s1"`, `"status":"completed"`, `"task":"find the capital"`}},
	}
	for _, tt := range tests {
		end := run.toolEnd(tt.id)
		for _, want := range tt.want {
			if end.Error != "" || !strings.Contains(end.Result, want) {
				t.Fatalf("%s = %+v, want %q", tt.id, end, want)
			}
		}
	}
}

func TestSearchDelegationTranscript(t *testing.T) {
	parent := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("d1", "delegate_to_helper", map[string]any{"task": "look up"}),
		agenttest.ToolCallResponse("q1", SearchSubAgentTool, map[string]any{"handle": "d1", "query": "answer"}),
		agenttest.TextResponse("done"),
	}}
	helper := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("the answer is 7")}}
	a := must.Get(New(Config{Name: "lead", Provider: parent, SubAgents: []SubAgentDef{
		{Name: "helper", Provider: helper},
		{Name: "worker", Provider: &agenttest.ScriptedProvider{}, Mode: SubAgentSpawn},
	}}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("go"))})
	run := spawnRun{stream: stream}
	for d := range stream.Deltas() {
		run.deltas = append(run.deltas, d)
	}
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	if end := run.toolEnd("q1"); !strings.Contains(end.Result, "the answer is 7") {
		t.Fatalf("search = %+v", end)
	}
}

// inlineRunner is a durable-looking StepRunner that runs every step inline.
type inlineRunner struct{}

func (inlineRunner) RunStep(ctx context.Context, _ string, fn func(context.Context) (types.StepResult, error)) (types.StepResult, error) {
	return fn(ctx)
}

func TestSpawnRefusedUnderDurableRunner(t *testing.T) {
	parent := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("s1", "spawn_worker", map[string]any{"task": "x"}),
		agenttest.TextResponse("done"),
	}}
	a := must.Get(New(Config{Provider: parent, SubAgents: []SubAgentDef{{Name: "worker", Provider: &agenttest.ScriptedProvider{}, Mode: SubAgentSpawn}}}))
	if _, err := a.RunDurable(context.Background(), inlineRunner{}, []types.Message{types.UserMsg(types.Text("go"))}, ""); err != nil {
		t.Fatal(err)
	}
	msgs, _ := a.Tree().FlattenBranch(a.Tree().Active())
	found := false
	for _, r := range msgs {
		for _, res := range toolResultsOf(r) {
			if res.CallID == "s1" && strings.Contains(res.Text(), ErrSpawnUnsupported.Error()) {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("spawn under a durable runner was not refused")
	}
}

func TestInvokeSubAgentFindsSpawnDefinition(t *testing.T) {
	child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("direct")}}
	a := must.Get(New(Config{
		Name:      "lead",
		Provider:  &agenttest.ScriptedProvider{},
		SubAgents: []SubAgentDef{{Name: "worker", Provider: child, Mode: SubAgentSpawn}},
	}))
	stream, err := a.InvokeSubAgent(context.Background(), "worker", "task")
	if err != nil {
		t.Fatal(err)
	}
	for range stream.Deltas() {
	}
	result, err := stream.SubAgentResult()
	if err != nil || result.Output != "direct" {
		t.Fatalf("result = %+v, %v", result, err)
	}
	info := a.Info()
	if len(info.Tools) != 0 || len(info.SubAgents) != 1 {
		t.Fatalf("info = %+v", info)
	}
}

// A spawned child waiting for an approval nobody gives must not keep the
// parent run open once the child is cancelled, by the run ending or by
// cancel_subagent, and its forwarded interrupt must leave the parent's list.
func TestCancelledChildWithPendingApproval(t *testing.T) {
	tests := []struct {
		name   string
		parent [][]types.Delta // responses after the spawn
		stop   bool            // the parent ends through a stop tool
	}{
		{
			name:   "parent stop tool ends the run",
			parent: [][]types.Delta{agenttest.ToolCallResponse("stop", "submit", nil)},
			stop:   true,
		},
		{
			name: "cancel_subagent",
			parent: [][]types.Delta{
				agenttest.ToolCallResponse("x1", CancelSubAgentTool, map[string]any{"handle": "s1"}),
				agenttest.TextResponse("final"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			marked := make(chan struct{})
			var once sync.Once
			// wait holds the parent until the child's approval is pending.
			wait := &types.ToolFunc{Def: types.ToolDef{Name: "wait"}, Fn: func(ctx context.Context, _ map[string]any) (string, error) {
				select {
				case <-marked:
					return "marked", nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}}
			submit := &agenttest.MockTool{Def: types.ToolDef{Name: "submit"}, Result: "ok"}
			responses := [][]types.Delta{
				agenttest.ToolCallResponse("s1", "spawn_worker", map[string]any{"task": "read"}),
				agenttest.ToolCallResponse("w1", "wait", nil),
			}
			parent := &agenttest.ScriptedProvider{Responses: append(responses, tt.parent...)}
			child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
				agenttest.ToolCallResponse("read", "read", nil),
				agenttest.TextResponse("child done"),
			}}
			def := SubAgentDef{Tools: types.NewToolRegistry(&agenttest.MockTool{Def: types.ToolDef{Name: "read"}, Result: "ok"})}
			run := withinDeadline(t, 5*time.Second, func() spawnRun {
				return runSpawn(t, parent, child, def, func(_ *EventStream, d types.Delta) {
					if _, ok := d.(types.MarkerDelta); ok {
						once.Do(func() { close(marked) })
					}
				}, func(c *Config) {
					c.ToolGate = approvalReadGate
					c.Tools = types.NewToolRegistry(wait, submit)
					if tt.stop {
						c.StopAtTools = []string{"submit"}
					}
				})
			})
			if run.err != nil {
				t.Fatal(run.err)
			}
			h := run.stream.SubAgents()
			if len(h) != 1 || h[0].Status() != HandleCanceled {
				t.Fatalf("handles = %+v", h)
			}
			if pending := run.stream.PendingInterrupts(); len(pending) != 0 {
				t.Fatalf("forwarded interrupt still pending: %+v", pending)
			}
		})
	}
}

// cancel_subagent on a child that already finished leaves its result for
// injection.
func TestCancelFinishedSubAgentKeepsResult(t *testing.T) {
	reg := newSpawnRegistry()
	h := reg.add("s1", "worker", "task", nil)
	reg.finish(h, SubAgentResult{Output: "found"}, nil)
	got, err := cancelSubAgent(context.Background(), reg, map[string]any{"handle": "s1"})
	if err != nil || !strings.Contains(got, "already finished") {
		t.Fatalf("cancel = %q, %v", got, err)
	}
	if finished := reg.takeFinished(); len(finished) != 1 || finished[0] != h {
		t.Fatalf("finished result was not left for injection: %+v", finished)
	}
}
