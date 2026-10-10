package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

// approvalReadGate requires approval for the "read" tool only.
var approvalReadGate = types.GateFunc(func(_ context.Context, def types.ToolDef, _ map[string]any) types.GateDecision {
	if def.Name == "read" {
		return types.RequireApproval("reads need review")
	}
	return types.Allow()
})

// gatedChildAgent builds a parent whose child calls a gated "read" tool.
func gatedChildAgent(opts ...Option) *Agent {
	parent := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("delegate", "delegate_to_child", map[string]any{"task": "read"}),
		agenttest.TextResponse("finished"),
	}}
	child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("read", "read", nil),
		agenttest.TextResponse("child done"),
	}}
	return must.Get(New(Config{
		Provider: parent,
		ToolGate: approvalReadGate,
		SubAgents: []SubAgentDef{{
			Name: "child", Provider: child,
			Tools: types.NewToolRegistry(&agenttest.MockTool{Def: types.ToolDef{Name: "read"}, Result: "ok"}),
		}},
	}, opts...))
}

// withinDeadline runs fn and fails the test if it does not return in time,
// so a hang shows up as a failure instead of a stuck test binary.
func withinDeadline[T any](t *testing.T, d time.Duration, fn func() T) T {
	t.Helper()
	done := make(chan T, 1)
	go func() { done <- fn() }()
	select {
	case v := <-done:
		return v
	case <-time.After(d):
		t.Fatalf("did not return within %s", d)
		var zero T
		return zero
	}
}

// A delegated child that needs approval under a run with no consumer must
// fail at once, not wait for a decision nobody can make.
func TestNonStreamingDelegationFailsFastOnChildApproval(t *testing.T) {
	t.Run("RunDurable", func(t *testing.T) {
		a := gatedChildAgent()
		err := withinDeadline(t, 2*time.Second, func() error {
			_, err := a.RunDurable(context.Background(), nil, []types.Message{types.UserMsg(types.Text("go"))}, "")
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "ApprovalRunner") {
			t.Fatalf("err = %v, want an error naming ApprovalRunner", err)
		}
	})
	t.Run("Execute", func(t *testing.T) {
		a := gatedChildAgent()
		tool, ok := a.tools.Get("delegate_to_child")
		if !ok {
			t.Fatal("delegate tool missing")
		}
		err := withinDeadline(t, 2*time.Second, func() error {
			_, err := tool.Execute(context.Background(), map[string]any{"task": "read"})
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "ApprovalRunner") {
			t.Fatalf("err = %v, want an error naming ApprovalRunner", err)
		}
	})
}

// approvingRunner is a recording runner that also resolves approvals.
type approvingRunner struct {
	*recordingRunner
	requests []string
}

func (r *approvingRunner) ResolveApproval(_ context.Context, req types.ApprovalRequest) (types.ApprovalDecision, error) {
	r.requests = append(r.requests, req.ID)
	return types.ApprovalDecision{Approved: true}, nil
}

func TestChildStepRunnerMatchesInnerApprovalSupport(t *testing.T) {
	tests := []struct {
		name         string
		runner       types.StepRunner
		wantNil      bool
		wantApproval bool
	}{
		{name: "noop", runner: types.NoopStepRunner{}, wantNil: true},
		{name: "durable without approvals", runner: newRecordingRunner()},
		{name: "durable with approvals", runner: &approvingRunner{recordingRunner: newRecordingRunner()}, wantApproval: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &Agent{cfg: Config{StepRunner: tt.runner}}
			child := a.childStepRunner("call")
			if (child == nil) != tt.wantNil {
				t.Fatalf("child runner = %v, wantNil %v", child, tt.wantNil)
			}
			if child == nil {
				return
			}
			if _, ok := child.(types.ApprovalRunner); ok != tt.wantApproval {
				t.Errorf("child implements ApprovalRunner = %v, want %v", ok, tt.wantApproval)
			}
			if _, ok := asPrefixRunner(child); !ok {
				t.Error("child runner is not recognized as a prefix runner")
			}
		})
	}
}

// Under a durable runner without durable approvals, a child behaves like its
// parent: compaction is allowed and approvals stream as scoped markers.
func TestDurableChildWithoutApprovalRunner(t *testing.T) {
	t.Run("compaction allowed", func(t *testing.T) {
		parent := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
			agenttest.ToolCallResponse("delegate", "delegate_to_child", map[string]any{"task": "work"}),
			agenttest.TextResponse("finished"),
		}}
		child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("child done")}}
		a := must.Get(New(Config{
			Provider:   parent,
			StepRunner: newRecordingRunner(),
			CompactCfg: &types.CompactConfig{Strategy: types.CompactSlidingWindow, WindowSize: 50},
			SubAgents:  []SubAgentDef{{Name: "child", Provider: child}},
		}))
		stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))})
		deltas := agenttest.CollectDeltas(stream.Deltas())
		if err := stream.Wait(); err != nil {
			t.Fatal(err)
		}
		if end, _ := endDeltaFor(deltas, "delegate"); end.Error != "" {
			t.Fatalf("delegation failed: %s", end.Error)
		}
	})
	t.Run("approval streams as a marker", func(t *testing.T) {
		a := gatedChildAgent(WithStepRunner(newRecordingRunner()))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("go"))})
		var markers []string
		var deltas []types.Delta
		for d := range stream.Deltas() {
			deltas = append(deltas, d)
			if m, ok := d.(types.MarkerDelta); ok {
				markers = append(markers, m.ToolCallID)
				if err := stream.ResolveMarkerErr(m.ToolCallID, Resolution{Approved: true}); err != nil {
					t.Errorf("resolve: %v", err)
				}
			}
		}
		if err := stream.Wait(); err != nil {
			t.Fatal(err)
		}
		if len(markers) != 1 || markers[0] != "delegate/read" {
			t.Fatalf("markers = %v, want [delegate/read]", markers)
		}
		if end, _ := endDeltaFor(deltas, "delegate"); end.Error != "" {
			t.Fatalf("delegation failed: %s", end.Error)
		}
	})
}

// sleepTool sleeps for a fixed time, honoring cancellation.
type sleepTool struct{ d time.Duration }

func (sleepTool) Definition() types.ToolDef { return types.ToolDef{Name: "sleep"} }
func (s sleepTool) Execute(ctx context.Context, _ map[string]any) (string, error) {
	select {
	case <-time.After(s.d):
		return "slept", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestDelegationTimeouts(t *testing.T) {
	childScript := func() *agenttest.ScriptedProvider {
		return &agenttest.ScriptedProvider{Responses: [][]types.Delta{
			agenttest.ToolCallResponse("s1", "sleep", nil),
			agenttest.ToolCallResponse("s2", "sleep", nil),
			agenttest.ToolCallResponse("s3", "sleep", nil),
			agenttest.TextResponse("child done"),
		}}
	}
	tests := []struct {
		name        string
		toolTimeout time.Duration
		subTimeout  time.Duration
		wantErr     string
	}{
		{name: "parent ToolTimeout bounds each child tool, not the delegation", toolTimeout: 50 * time.Millisecond},
		{name: "SubAgentDef.Timeout bounds the whole child run", subTimeout: 50 * time.Millisecond, wantErr: "exceeded timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
				agenttest.ToolCallResponse("delegate", "delegate_to_child", map[string]any{"task": "work"}),
				agenttest.TextResponse("finished"),
			}}
			a := must.Get(New(Config{
				Provider:    parent,
				ToolTimeout: tt.toolTimeout,
				SubAgents: []SubAgentDef{{
					Name: "child", Provider: childScript(), Timeout: tt.subTimeout,
					Tools: types.NewToolRegistry(sleepTool{d: 30 * time.Millisecond}),
				}},
			}))
			stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("go"))})
			deltas := agenttest.CollectDeltas(stream.Deltas())
			if err := stream.Wait(); err != nil {
				t.Fatal(err)
			}
			end, ok := endDeltaFor(deltas, "delegate")
			if !ok {
				t.Fatal("no delegation result")
			}
			if tt.wantErr == "" && end.Error != "" {
				t.Fatalf("delegation failed: %s", end.Error)
			}
			if tt.wantErr != "" && !strings.Contains(end.Error, tt.wantErr) {
				t.Fatalf("delegation error = %q, want it to contain %q", end.Error, tt.wantErr)
			}
		})
	}
}

// Time a child spends waiting for a human does not count against its limit.
func TestDelegationTimeoutExcludesApprovalWait(t *testing.T) {
	parent := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("delegate", "delegate_to_child", map[string]any{"task": "read"}),
		agenttest.TextResponse("finished"),
	}}
	child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("read", "read", nil),
		agenttest.TextResponse("child done"),
	}}
	a := must.Get(New(Config{
		Provider: parent, ToolGate: approvalReadGate, ToolTimeout: 50 * time.Millisecond,
		SubAgents: []SubAgentDef{{
			Name: "child", Provider: child, Timeout: 100 * time.Millisecond,
			Tools: types.NewToolRegistry(&agenttest.MockTool{Def: types.ToolDef{Name: "read"}, Result: "ok"}),
		}},
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("go"))})
	var deltas []types.Delta
	for d := range stream.Deltas() {
		deltas = append(deltas, d)
		if m, ok := d.(types.MarkerDelta); ok {
			time.Sleep(300 * time.Millisecond) // a slow human
			if err := stream.ResolveMarkerErr(m.ToolCallID, Resolution{Approved: true}); err != nil {
				t.Errorf("resolve: %v", err)
			}
		}
	}
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}
	if end, _ := endDeltaFor(deltas, "delegate"); end.Error != "" {
		t.Fatalf("delegation failed while waiting for approval: %s", end.Error)
	}
}

func TestPausableDeadline(t *testing.T) {
	fired := make(chan struct{}, 1)
	p := newPausableDeadline(40*time.Millisecond, func() { fired <- struct{}{} })
	p.pause()
	p.pause() // nested
	time.Sleep(80 * time.Millisecond)
	p.resume()
	select {
	case <-fired:
		t.Fatal("fired while a pause was still held")
	case <-time.After(60 * time.Millisecond):
	}
	p.resume()
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("did not fire after resuming")
	}
	p.stop()

	var nilClock *pausableDeadline
	nilClock.pause()
	nilClock.resume()
	nilClock.stop()

	stopped := newPausableDeadline(10*time.Millisecond, func() { t.Error("a stopped deadline fired") })
	stopped.stop()
	time.Sleep(30 * time.Millisecond)
}

// A custom invoker whose child emits a marker under a run with no consumer
// is cancelled instead of waiting.
type markerInvoker struct{}

func (markerInvoker) Definition() types.ToolDef {
	return types.ToolDef{Name: "custom", Parameters: types.ParameterSchema{Type: "object"}}
}
func (markerInvoker) Execute(context.Context, map[string]any) (string, error) { return "", nil }
func (markerInvoker) InvokeAgent(ctx context.Context, _ string) *EventStream {
	ctx, cancel := context.WithCancel(ctx)
	s := newEventStream(ctx, cancel)
	go func() {
		s.send(types.MarkerDelta{ToolCallID: "inner", ToolName: "x"})
		<-ctx.Done()
		s.close(ctx.Err())
	}()
	return s
}

func TestNonStreamingCustomInvokerMarkerFails(t *testing.T) {
	provider := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
		agenttest.ToolCallResponse("c1", "custom", nil),
		agenttest.TextResponse("finished"),
	}}
	a := must.Get(New(Config{Provider: provider, Tools: types.NewToolRegistry(markerInvoker{})}))
	err := withinDeadline(t, 2*time.Second, func() error {
		_, err := a.RunDurable(context.Background(), nil, []types.Message{types.UserMsg(types.Text("go"))}, "")
		return err
	})
	if !errors.Is(err, errNonStreamingApproval) {
		t.Fatalf("err = %v, want the non-streaming approval error", err)
	}
}

// TestChildParallelApprovalsListedAtRoot checks that a child waiting on two
// approvals at once has both forwarded and listed at the root, rather than
// the second staying hidden until the first is answered.
func TestChildParallelApprovalsListedAtRoot(t *testing.T) {
	for _, order := range []string{"first answered first", "second answered first"} {
		t.Run(order, func(t *testing.T) {
			parent := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
				agenttest.ToolCallResponse("delegate", "delegate_to_child", map[string]any{"task": "read"}),
				agenttest.TextResponse("finished"),
			}}
			child := &agenttest.ScriptedProvider{Responses: [][]types.Delta{
				append(agenttest.ToolCallResponse("r1", "read", nil), agenttest.ToolCallResponse("r2", "read", nil)...),
				agenttest.TextResponse("child done"),
			}}
			read := &agenttest.MockTool{Def: types.ToolDef{Name: "read"}, Result: "ok"}
			a := must.Get(New(Config{
				Provider: parent,
				ToolGate: approvalReadGate,
				SubAgents: []SubAgentDef{{
					Name: "child", Provider: child, Tools: types.NewToolRegistry(read),
				}},
			}))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream := a.Invoke(ctx, []types.Message{types.UserMsg(types.Text("go"))})
			first, second := nextMarker(t, stream), nextMarker(t, stream)
			ids := map[string]bool{first.ToolCallID: true, second.ToolCallID: true}
			if !ids["delegate/r1"] || !ids["delegate/r2"] {
				t.Fatalf("markers = %q, %q; want both child calls", first.ToolCallID, second.ToolCallID)
			}
			if pending := stream.PendingInterrupts(); len(pending) != 2 {
				t.Fatalf("root pending = %+v, want 2", pending)
			}
			answer := []types.MarkerDelta{first, second}
			if order == "second answered first" {
				answer[0], answer[1] = second, first
			}
			for _, m := range answer {
				if err := stream.ReplyInterrupt(ctx, types.InterruptReply{ID: m.Interrupt.ID, Decision: types.ApprovalDecision{Approved: true}}); err != nil {
					t.Fatal(err)
				}
			}
			for range stream.Deltas() {
			}
			if err := stream.Wait(); err != nil {
				t.Fatal(err)
			}
			if read.CallCount() != 2 {
				t.Errorf("child tool calls = %d, want 2", read.CallCount())
			}
			if pending := stream.PendingInterrupts(); len(pending) != 0 {
				t.Errorf("pending after the run = %+v", pending)
			}
		})
	}
}
