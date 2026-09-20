package local

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

type provider struct{ calls *atomic.Int32 }

func (p provider) ChatStream(_ context.Context, m []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	p.calls.Add(1)
	out := make(chan types.Delta, 20)
	if len(m) <= 2 {
		for _, d := range agenttest.ToolCallResponse("approval-call", "write", map[string]any{"value": "original"}) {
			out <- d
		}
		for _, d := range agenttest.ToolCallResponse("independent-call", "read", nil) {
			out <- d
		}
	} else {
		for _, d := range agenttest.TextResponse("done") {
			out <- d
		}
	}
	close(out)
	return out, nil
}
func TestSuspendReleaseResumeAndReplay(t *testing.T) {
	ctx := context.Background()
	engine := New(t.TempDir())
	var calls, writes, reads atomic.Int32
	write := &types.ToolFunc{Def: types.ToolDef{Name: "write"}, Fn: func(_ context.Context, args map[string]any) (string, error) {
		writes.Add(1)
		if args["value"] != "approved" {
			t.Errorf("wrong args %v", args)
		}
		args["value"] = "mutated"
		return "written", nil
	}}
	read := &types.ToolFunc{Def: types.ToolDef{Name: "read"}, Fn: func(context.Context, map[string]any) (string, error) { reads.Add(1); return "read", nil }}
	factory := func() *agent.Agent {
		return agent.NewAgent(agent.AgentConfig{Provider: provider{&calls}, SystemPrompt: "rules", Tools: types.NewToolRegistry(types.WithMarkers(write, types.Marker{Kind: "approval"}), read), MaxParallelTools: 1})
	}
	input := []types.Message{types.NewUserMessage("go")}
	_, err := engine.Run(ctx, "run", "v1", factory, input)
	if !errors.Is(err, types.ErrSuspended) {
		t.Fatal(err)
	}
	if writes.Load() != 0 || reads.Load() != 1 || calls.Load() != 1 {
		t.Fatalf("calls=%d reads=%d writes=%d", calls.Load(), reads.Load(), writes.Load())
	}
	// Reopen the engine to prove decisions and completed siblings survive restart.
	engine = New(engine.Directory)
	state, err := engine.Inspect("run")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "suspended" || len(state.Interrupts) != 1 {
		t.Fatalf("%+v", state)
	}
	decision := types.ApprovalDecision{Approved: true, ModifiedArgs: map[string]any{"value": "approved"}}
	if err := engine.Decide("run", "wrong", "marker/approval-call", "key", decision); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err := engine.Decide("run", "v1", "marker/approval-call", "key", decision); err != nil {
		t.Fatal(err)
	}
	if err := engine.Decide("run", "v1", "marker/approval-call", "key", decision); err != nil {
		t.Fatal(err)
	}
	if err := engine.Decide("run", "v1", "marker/approval-call", "other", types.ApprovalDecision{}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	result, err := engine.Run(ctx, "run", "v1", factory, input)
	if err != nil || result == nil {
		t.Fatalf("%v %v", result, err)
	}
	if writes.Load() != 1 || reads.Load() != 1 || calls.Load() != 2 {
		t.Fatalf("calls=%d reads=%d writes=%d", calls.Load(), reads.Load(), writes.Load())
	}
	if _, err = engine.Run(ctx, "run", "v1", factory, input); err != nil {
		t.Fatal(err)
	}
	if writes.Load() != 1 || reads.Load() != 1 || calls.Load() != 2 {
		t.Fatal("replayed effects")
	}
}
func TestUncertainAttemptNeedsReconciliation(t *testing.T) {
	e := New(t.TempDir())
	path, release, err := e.acquire("run")
	if err != nil {
		t.Fatal(err)
	}
	r := &runner{path: path, state: State{Version: 1, RunID: "run", Revision: "v1", Steps: map[string]Step{}, Interrupts: map[string]Interrupt{}}}
	_, err = r.RunStep(context.Background(), "write", func(context.Context) (types.StepResult, error) { panic("after external write") })
	if err == nil {
		t.Fatal("panic hidden")
	}
	_, err = r.RunStep(context.Background(), "write", func(context.Context) (types.StepResult, error) {
		t.Fatal("repeated uncertain write")
		return types.StepResult{}, nil
	})
	if !errors.Is(err, ErrIndeterminate) {
		t.Fatal(err)
	}
	release()
	result := types.StepResult{Kind: types.StepKindTool, ToolResult: "verified written", ToolBlocks: []types.ToolResultBlock{{Data: []byte{1, 2}}}}
	if err := e.Reconcile("run", "v1", "write", &result); err != nil {
		t.Fatal(err)
	}
	saved, err := e.Inspect("run")
	if err != nil {
		t.Fatal(err)
	}
	var restored types.StepResult
	if err := decode(saved.Steps["write"].Result, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.ToolBlocks[0].Data[1] != 2 {
		t.Fatal("lost bytes")
	}
}
func TestWorkerLockAcrossProcesses(t *testing.T) {
	if path := os.Getenv("SAIGE_LOCK_PROBE"); path != "" {
		_, err := lock(path)
		if !errors.Is(err, ErrBusy) {
			os.Exit(2)
		}
		return
	}
	path := t.TempDir() + "/worker.lock"
	release, err := lock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkerLockAcrossProcesses$")
	cmd.Env = append(os.Environ(), "SAIGE_LOCK_PROBE="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v", output, err)
	}
}
func TestApprovalExpiryAndCancellation(t *testing.T) {
	e := New(t.TempDir())
	path, release, err := e.acquire("run")
	if err != nil {
		t.Fatal(err)
	}
	r := &runner{path: path, ttl: time.Hour, state: State{Version: 1, RunID: "run", Revision: "v1", Steps: map[string]Step{}, Interrupts: map[string]Interrupt{}}}
	req := types.ApprovalRequest{ID: "approval", ToolCall: types.ToolUseContent{ID: "call", Arguments: map[string]any{"integer": 9007199254740993}}}
	if _, err := r.ResolveApproval(context.Background(), req); !errors.Is(err, types.ErrSuspended) {
		t.Fatal(err)
	}
	p := r.state.Interrupts[req.ID]
	p.ExpiresAt = time.Now().Add(-time.Second)
	r.state.Interrupts[req.ID] = p
	if err := r.save(); err != nil {
		t.Fatal(err)
	}
	release()
	if err := e.Decide("run", "v1", req.ID, "key", types.ApprovalDecision{Approved: true}); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err := e.Cancel("run", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := e.Decide("run", "v1", req.ID, "key", types.ApprovalDecision{Approved: true}); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestProcessCrashDoesNotRepeatUncertainStep(t *testing.T) {
	if dir := os.Getenv("SAIGE_CRASH_PROBE"); dir != "" {
		e := New(dir)
		path, release, err := e.acquire("crash")
		if err != nil {
			os.Exit(2)
		}
		defer release()
		r := &runner{path: path, state: State{Version: 1, RunID: "crash", Revision: "v1", Steps: map[string]Step{}, Interrupts: map[string]Interrupt{}}}
		_, _ = r.RunStep(context.Background(), "side-effect", func(ctx context.Context) (types.StepResult, error) {
			receipt := types.BudgetReceipt{ID: "crashed-attempt", Cost: types.USD(.25), Usage: types.TokenUsage{Requests: 1, InputTokens: 100}, Uncertain: true}
			if err := r.RecordReservation(ctx, "side-effect", receipt); err != nil {
				os.Exit(4)
			}
			os.Exit(17)
			return types.StepResult{}, nil
		})
		os.Exit(3)
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessCrashDoesNotRepeatUncertainStep$")
	cmd.Env = append(os.Environ(), "SAIGE_CRASH_PROBE="+dir)
	if err := cmd.Run(); err == nil {
		t.Fatal("helper did not crash")
	}
	e := New(dir)
	path, release, err := e.acquire("crash")
	if err != nil {
		t.Fatal("crash did not release lease", err)
	}
	defer release()
	state, err := e.Inspect("crash")
	if err != nil {
		t.Fatal(err)
	}
	var pending types.StepResult
	if err := decode(state.Steps["side-effect"].Result, &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Receipt == nil || pending.Receipt.Cost != types.USD(.25) || !pending.Receipt.Uncertain {
		t.Fatalf("lost crash accounting: %+v", pending.Receipt)
	}
	r := &runner{path: path, state: state}
	_, err = r.RunStep(context.Background(), "side-effect", func(context.Context) (types.StepResult, error) {
		t.Fatal("repeated external effect")
		return types.StepResult{}, nil
	})
	if !errors.Is(err, ErrIndeterminate) {
		t.Fatal(err)
	}
}

func TestBudgetReplayRestoresUnknownSettlement(t *testing.T) {
	e := New(t.TempDir())
	var calls atomic.Int32
	var current *types.Budget
	factory := func() *agent.Agent {
		current = types.NewBudget(types.BudgetPolicy{MaxRequests: 5, PerCallTokens: 100})
		p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.TextResponse("done")}}
		calls.Add(1)
		return agent.NewAgent(agent.AgentConfig{Provider: p, Budget: current})
	}
	input := []types.Message{types.NewUserMessage("go")}
	if _, err := e.Run(context.Background(), "budget", "v1", factory, input); err != nil {
		t.Fatal(err)
	}
	if current.Uncertain() != 1 || current.Usage().Total() != 100 || current.Usage().Requests != 1 {
		t.Fatalf("%+v", current.Usage())
	}
	if _, err := e.Run(context.Background(), "budget", "v1", factory, input); err != nil {
		t.Fatal(err)
	}
	if current.Uncertain() != 1 || current.Usage().Total() != 100 || current.Usage().Requests != 1 {
		t.Fatalf("replay %+v", current.Usage())
	}
}

type stagedProvider struct {
	calls *atomic.Int32
	first []types.Delta
}

func (p stagedProvider) ChatStream(_ context.Context, messages []types.Message, _ []types.ToolDef) (<-chan types.Delta, error) {
	p.calls.Add(1)
	deltas := agenttest.TextResponse("done")
	if len(messages) <= 2 {
		deltas = p.first
	}
	out := make(chan types.Delta, len(deltas)+1)
	for _, d := range deltas {
		out <- d
	}
	out <- types.UsageDelta{PromptTokens: 10, CompletionTokens: 2}
	close(out)
	return out, nil
}
func (p stagedProvider) Capabilities() types.ModelCapabilities {
	return types.ModelCapabilities{Pricing: types.Pricing{Free: true}}
}

func TestChildApprovalReplaysParentAndChildWithoutRepeatingEffects(t *testing.T) {
	var parentCalls, childCalls, writes atomic.Int32
	e := New(t.TempDir())
	factory := func() *agent.Agent {
		write := &types.ToolFunc{Def: types.ToolDef{Name: "write"}, Fn: func(context.Context, map[string]any) (string, error) { writes.Add(1); return "written", nil }}
		return agent.NewAgent(agent.AgentConfig{SystemPrompt: "parent", Provider: stagedProvider{&parentCalls, agenttest.ToolCallResponse("child-call", "delegate_to_child", map[string]any{"task": "write"})}, Budget: types.NewBudget(types.BudgetPolicy{MaxRequests: 10}), SubAgents: []agent.SubAgentDef{{Name: "child", SystemPrompt: "child", Provider: stagedProvider{&childCalls, agenttest.ToolCallResponse("write-call", "write", nil)}, Tools: types.NewToolRegistry(types.WithMarkers(write, types.Marker{Kind: "approval"}))}}})
	}
	input := []types.Message{types.NewUserMessage("go")}
	if _, err := e.Run(context.Background(), "child", "v1", factory, input); !errors.Is(err, types.ErrSuspended) {
		t.Fatal(err)
	}
	state, err := e.Inspect("child")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Interrupts) != 1 || writes.Load() != 0 {
		t.Fatalf("pending=%d writes=%d", len(state.Interrupts), writes.Load())
	}
	for id := range state.Interrupts {
		if err := e.Decide("child", "v1", id, "decision", types.ApprovalDecision{Approved: true}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := e.Run(context.Background(), "child", "v1", factory, input); err != nil {
			t.Fatal(err)
		}
	}
	if writes.Load() != 1 || parentCalls.Load() != 2 || childCalls.Load() != 2 {
		t.Fatalf("writes=%d parent=%d child=%d", writes.Load(), parentCalls.Load(), childCalls.Load())
	}
}

func TestAdmissionApprovalPersistsBeforeProviderAndRestoresGrant(t *testing.T) {
	e := New(t.TempDir())
	var calls atomic.Int32
	var budget *types.Budget
	factory := func() *agent.Agent {
		budget = types.NewBudget(types.BudgetPolicy{Limit: types.USD(.25), PerCallCost: types.USD(.4), OnExceed: types.BudgetRequireApproval})
		return agent.NewAgent(agent.AgentConfig{SystemPrompt: "system", Provider: stagedProvider{&calls, agenttest.TextResponse("done")}, Budget: budget})
	}
	input := []types.Message{types.NewUserMessage("go")}
	if _, err := e.Run(context.Background(), "grant", "v1", factory, input); !errors.Is(err, types.ErrSuspended) {
		t.Fatal(err)
	}
	state, err := e.Inspect("grant")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || len(state.Steps) != 0 || len(state.Interrupts) != 1 {
		t.Fatalf("provider=%d steps=%d interrupts=%d", calls.Load(), len(state.Steps), len(state.Interrupts))
	}
	for id := range state.Interrupts {
		if err := e.Decide("grant", "v1", id, "decision", types.ApprovalDecision{Approved: true}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := e.Run(context.Background(), "grant", "v1", factory, input); err != nil {
			t.Fatal(err)
		}
		if budget.Remaining() != types.USD(.5) || budget.Usage().Requests != 1 {
			t.Fatalf("remaining=%v usage=%+v", budget.Remaining(), budget.Usage())
		}
	}
	if calls.Load() != 1 {
		t.Fatal("repeated provider call")
	}
}

func TestIndependentChildBudgetFailsBeforeChildDispatch(t *testing.T) {
	e := New(t.TempDir())
	var parentCalls, childCalls atomic.Int32
	factory := func() *agent.Agent {
		return agent.NewAgent(agent.AgentConfig{SystemPrompt: "parent", Provider: stagedProvider{&parentCalls, agenttest.ToolCallResponse("child-call", "delegate_to_child", map[string]any{"task": "go"})}, Budget: types.NewBudget(types.BudgetPolicy{MaxRequests: 10}), SubAgents: []agent.SubAgentDef{{Name: "child", Provider: stagedProvider{&childCalls, agenttest.TextResponse("done")}, Options: []agent.AgentOption{agent.WithBudget(types.NewBudget(types.BudgetPolicy{MaxRequests: 5}))}}}})
	}
	_, err := e.Run(context.Background(), "separate", "v1", factory, []types.Message{types.NewUserMessage("go")})
	if err == nil || childCalls.Load() != 0 {
		t.Fatalf("independent child dispatched: %d, err=%v", childCalls.Load(), err)
	}
}
