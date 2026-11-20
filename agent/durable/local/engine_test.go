package local

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/internal/must"
)

type noArgs struct{}

// oneTool is an agent that calls tool "work" once, then answers.
func oneTool(work func() (string, error), idempotent bool) Factory {
	return func() *agent.Agent {
		var opts []agent.FuncOption
		if idempotent {
			opts = append(opts, agent.Idempotent())
		}
		tool := agent.Func("work", "does the work", func(agent.RunContext[agent.NoDeps], noArgs) (string, error) { return work() }, opts...)
		return must.Get(agent.New(agent.Config{Name: "worker", Tools: types.NewToolRegistry(tool), Provider: toolThenText{}}))
	}
}

// toolThenText calls tool "work" until the conversation holds its result,
// then answers. It reads the conversation, so a replayed model call and a
// live one get the same answer.
type toolThenText struct{}

func (toolThenText) Stream(_ context.Context, req types.Request) (<-chan types.Delta, error) {
	deltas := agenttest.ToolCallResponse("c1", "work", map[string]any{})
	if len(req.Messages) > 2 {
		deltas = agenttest.TextResponse("worked")
	}
	out := make(chan types.Delta, len(deltas))
	re := agenttest.Reindexer()
	for _, d := range deltas {
		out <- re(d)
	}
	close(out)
	return out, nil
}

func input() []types.Message { return []types.Message{types.UserMsg(types.Text("go"))} }

// helperProcess runs this test binary as another process with env set, for
// tests that need a second process or a crash.
func helperProcess(t *testing.T, test string, env ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+test+"$")
	cmd.Env = append(os.Environ(), env...)
	return cmd
}

// A process that dies while a step runs leaves it uncertain: the run is not
// leased, Run reports ErrIndeterminate without repeating the step, and the
// host's Reconcile lets the run finish.
func TestACrashLeavesTheStepForReconcile(t *testing.T) {
	if dir := os.Getenv("SAIGE_LOCAL_CRASH"); dir != "" {
		e := New(dir)
		_, _ = e.Run(context.Background(), "crash", "v1", oneTool(func() (string, error) { os.Exit(17); return "", nil }, false), input())
		os.Exit(3)
	}
	dir := t.TempDir()
	if err := helperProcess(t, "TestACrashLeavesTheStepForReconcile", "SAIGE_LOCAL_CRASH="+dir).Run(); err == nil {
		t.Fatal("helper did not crash")
	}
	e := New(dir)
	if leased, err := e.Leased("crash"); err != nil || leased {
		t.Fatalf("crashed run leased=%v err=%v, want an unleased orphan", leased, err)
	}
	var calls atomic.Int32
	factory := oneTool(func() (string, error) { calls.Add(1); return "done", nil }, false)
	if _, err := e.Run(context.Background(), "crash", "v1", factory, nil); !errors.Is(err, ErrIndeterminate) {
		t.Fatalf("Run after the crash: %v, want ErrIndeterminate", err)
	}
	s, err := e.Inspect("crash")
	if err != nil || s.Status != statusFailed || s.Steps["tool-c1"].Status != "indeterminate" {
		t.Fatalf("state %s, step %+v, %v", s.Status, s.Steps["tool-c1"], err)
	}
	if err := e.Reconcile("crash", "v1", "tool-c1", &types.StepResult{Kind: types.StepKindTool, ToolResult: "checked: done"}); err != nil {
		t.Fatal(err)
	}
	final, err := e.Run(context.Background(), "crash", "v1", factory, nil)
	if err != nil || final == nil || calls.Load() != 0 {
		t.Fatalf("final %+v, %v; tool ran again %d times", final, err, calls.Load())
	}
}

// One process executes a run at a time. While another holds it, Run, Append
// and Cancel return ErrBusy and Leased reports it; when that process dies,
// the run is free at once.
func TestARunExecutesInOneProcessAtATime(t *testing.T) {
	if dir := os.Getenv("SAIGE_LOCAL_HOLD"); dir != "" {
		e := New(dir)
		_, _ = e.Run(context.Background(), "held", "v1", oneTool(func() (string, error) {
			if err := os.WriteFile(dir+"/holding", nil, 0o600); err != nil {
				os.Exit(2)
			}
			select {} // hold the run until killed
		}, true), input())
		os.Exit(3)
	}
	dir := t.TempDir()
	cmd := helperProcess(t, "TestARunExecutesInOneProcessAtATime", "SAIGE_LOCAL_HOLD="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	for deadline := time.Now().Add(10 * time.Second); ; {
		if _, err := os.Stat(dir + "/holding"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the other process never started the run")
		}
		time.Sleep(10 * time.Millisecond)
	}
	e := New(dir)
	if leased, err := e.Leased("held"); err != nil || !leased {
		t.Fatalf("leased = %v, %v", leased, err)
	}
	if s, err := e.Inspect("held"); err != nil || s.Status != statusRunning {
		t.Fatalf("status %s, %v", s.Status, err)
	}
	factory := oneTool(func() (string, error) { return "done", nil }, true)
	if _, err := e.Run(context.Background(), "held", "v1", factory, nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("Run: %v, want ErrBusy", err)
	}
	if err := e.Append("held", "v1", "k", input()); !errors.Is(err, ErrBusy) {
		t.Fatalf("Append: %v, want ErrBusy", err)
	}
	if err := e.Cancel("held", "v1"); !errors.Is(err, ErrBusy) {
		t.Fatalf("Cancel: %v, want ErrBusy", err)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if leased, err := e.Leased("held"); err != nil || leased {
		t.Fatalf("after the holder died: leased = %v, %v", leased, err)
	}
	if final, err := e.Run(context.Background(), "held", "v1", factory, nil); err != nil || final == nil {
		t.Fatalf("Run after the holder died: %+v, %v", final, err)
	}
}

// An idempotent step that fails is retried by running the run again.
func TestAFailedIdempotentStepRunsAgain(t *testing.T) {
	e := New(t.TempDir())
	var calls atomic.Int32
	factory := oneTool(func() (string, error) {
		if calls.Add(1) == 1 {
			panic("first try fails")
		}
		return "stored", nil
	}, true)
	if _, err := e.Run(context.Background(), "run", "v1", factory, input()); err == nil {
		t.Fatal("the first failure was hidden")
	}
	final, err := e.Run(context.Background(), "run", "v1", factory, nil)
	if err != nil || final == nil || calls.Load() != 2 {
		t.Fatalf("final %+v, %v after %d calls", final, err, calls.Load())
	}
	if s, _ := e.Inspect("run"); s.Status != statusCompleted || s.Steps["tool-c1"].Status != statusCompleted {
		t.Fatalf("state %s, step %+v", s.Status, s.Steps["tool-c1"])
	}
}

// An approval that expires cannot be decided, and a cancelled run cannot be
// run again.
func TestApprovalExpiryAndCancellation(t *testing.T) {
	ctx := context.Background()
	e := &Engine{Directory: t.TempDir(), ApprovalTTL: 50 * time.Millisecond}
	var calls atomic.Int32
	factory := func() *agent.Agent { return approvalAgent(&calls) }
	if _, err := e.Run(ctx, "run", "v1", factory, input()); !errors.Is(err, types.ErrSuspended) {
		t.Fatalf("Run: %v, want ErrSuspended", err)
	}
	s, _ := e.Inspect("run")
	var id string
	for k := range s.Interrupts {
		id = k
	}
	time.Sleep(100 * time.Millisecond)
	err := e.Decide("run", "v1", id, "late", types.ApprovalDecision{Approved: true})
	if !errors.Is(err, ErrClosed) || !errors.Is(err, types.ErrInterruptExpired) {
		t.Fatalf("late decision: %v", err)
	}

	if _, err := e.Run(ctx, "other", "v1", factory, input()); !errors.Is(err, types.ErrSuspended) {
		t.Fatalf("Run: %v", err)
	}
	if err := e.Cancel("other", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := e.Cancel("other", "v1"); err != nil {
		t.Fatalf("cancelling twice: %v", err)
	}
	if _, err := e.Run(ctx, "other", "v1", factory, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Run after Cancel: %v, want ErrClosed", err)
	}
	if s, _ := e.Inspect("other"); s.Status != statusCancelled {
		t.Fatalf("status %s", s.Status)
	}
	if calls.Load() != 0 {
		t.Fatal("the gated tool ran")
	}
}

// approvalAgent calls a tool that needs approval.
func approvalAgent(calls *atomic.Int32) *agent.Agent {
	write := agent.Func("work", "writes", func(agent.RunContext[agent.NoDeps], noArgs) (string, error) { calls.Add(1); return "written", nil })
	return must.Get(agent.New(agent.Config{
		Tools:    types.NewToolRegistry(types.WithMarkers(write, types.Marker{Kind: "approval"})),
		Provider: toolThenText{},
	}))
}

// Append adds input idempotently by key; a completed run answers it and is
// completed again; a stale revision and a cancelled run are refused.
func TestAppendRules(t *testing.T) {
	ctx := context.Background()
	e := New(t.TempDir())
	var calls atomic.Int32
	echo := func() *agent.Agent { return must.Get(agent.New(agent.Config{Provider: echoProvider{&calls}})) }
	if _, err := e.Run(ctx, "run", "v1", echo, input()); err != nil {
		t.Fatal(err)
	}
	more := []types.Message{types.UserMsg(types.Text("more"))}
	if err := e.Append("run", "v1", "k1", more); err != nil {
		t.Fatal(err)
	}
	if err := e.Append("run", "v1", "k1", more); err != nil {
		t.Fatalf("same key and messages: %v", err)
	}
	if err := e.Append("run", "v1", "k1", input()); !errors.Is(err, ErrConflict) {
		t.Fatalf("same key, other messages: %v", err)
	}
	if err := e.Append("run", "v2", "k2", more); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	if s, _ := e.Inspect("run"); s.Status != statusReady || len(s.Inputs) != 1 || s.Inputs[0].Sequence != 1 {
		t.Fatalf("after append: %s with %+v", s.Status, s.Inputs)
	}
	final, err := e.Run(ctx, "run", "v1", echo, nil)
	if err != nil || lastText(t, final) != "re: more" {
		t.Fatalf("answer to the appended input: %+v, %v", final, err)
	}
	if s, _ := e.Inspect("run"); s.Status != statusCompleted {
		t.Fatalf("status %s", s.Status)
	}
	// The run's input log is checked message by message.
	if _, err := e.Run(ctx, "run", "v1", echo, input()); err != nil {
		t.Fatalf("a prefix of the log: %v", err)
	}
	if _, err := e.Run(ctx, "run", "v1", echo, []types.Message{types.UserMsg(types.Text("not the log"))}); !errors.Is(err, ErrConflict) {
		t.Fatalf("input that differs from the log: %v", err)
	}
	if _, err := e.Run(ctx, "run", "v2", echo, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("Run with another revision: %v", err)
	}
	// Delete the completed run; Append to a cancelled one is closed.
	if err := e.Delete("run", "v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Inspect("run"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("after Delete: %v", err)
	}
	if _, err := e.Run(ctx, "gone", "v1", func() *agent.Agent { return approvalAgent(new(atomic.Int32)) }, input()); !errors.Is(err, types.ErrSuspended) {
		t.Fatal(err)
	}
	if err := e.Cancel("gone", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := e.Append("gone", "v1", "k", more); !errors.Is(err, ErrClosed) {
		t.Fatalf("Append to a cancelled run: %v", err)
	}
}

// A factory that cannot build an agent fails the run, and the state says
// why.
func TestASetupFailureIsRecorded(t *testing.T) {
	e := New(t.TempDir())
	if _, err := e.Run(context.Background(), "run", "v1", func() *agent.Agent { return nil }, input()); err == nil {
		t.Fatal("setup failure hidden")
	}
	s, err := e.Inspect("run")
	if err != nil || s.Status != statusFailed || !strings.Contains(s.Error, "nil agent") {
		t.Fatalf("status %q error %q, %v", s.Status, s.Error, err)
	}
	if _, err := e.Leased("missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unknown run: %v", err)
	}
}
