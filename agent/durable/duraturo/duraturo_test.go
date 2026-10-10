package duraturo

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urmzd/duraturo/pkg/ledger"
	"github.com/urmzd/duraturo/pkg/queue"
	"github.com/urmzd/duraturo/pkg/worker"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/agent/types"
)

// newEngine returns an engine on an in-memory ledger and queue.
func newEngine() *Engine { return New(ledger.NewMemory(), queue.NewMemory()) }

// startWorker runs a worker for e until the test ends. The short lease lets
// the janitor pick up a reply that lands while its run is still parking.
func startWorker(t *testing.T, e *Engine) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := e.Worker(
		worker.WithLeaseTTL(300*time.Millisecond),
		worker.WithJanitorEvery(20*time.Millisecond),
		worker.WithConcurrency(2),
		worker.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

type provider struct{ calls *atomic.Int32 }

func (p provider) Stream(_ context.Context, req types.Request) (<-chan types.Delta, error) {
	m := req.Messages
	p.calls.Add(1)
	out := make(chan types.Delta, 20)
	re := agenttest.Reindexer()
	if len(m) <= 2 {
		for _, d := range agenttest.ToolCallResponse("approval-call", "write", map[string]any{"value": "original"}) {
			out <- re(d)
		}
		for _, d := range agenttest.ToolCallResponse("independent-call", "read", nil) {
			out <- re(d)
		}
	} else {
		for _, d := range agenttest.TextResponse("done") {
			out <- re(d)
		}
	}
	close(out)
	return out, nil
}

func TestSuspendDecideResumeAndReplay(t *testing.T) {
	ctx := testContext(t)
	e := newEngine()
	var calls, writes, reads atomic.Int32
	factory := func(string) *agent.Agent {
		write := &types.ToolFunc{Def: types.ToolDef{Name: "write"}, Fn: func(_ context.Context, args map[string]any) (string, error) {
			writes.Add(1)
			if args["value"] != "approved" {
				t.Errorf("wrong args %v", args)
			}
			return "written", nil
		}}
		read := &types.ToolFunc{Def: types.ToolDef{Name: "read"}, Fn: func(context.Context, map[string]any) (string, error) { reads.Add(1); return "read", nil }}
		return agent.NewAgent(agent.AgentConfig{Provider: provider{&calls}, SystemPrompt: "rules", Tools: types.NewToolRegistry(types.WithMarkers(write, types.Marker{Kind: "approval"}), read), MaxParallelTools: 1})
	}
	wf := e.Register("", factory)
	startWorker(t, e)
	input := []types.Message{types.UserMsg(types.Text("go"))}

	if _, err := e.Run(ctx, wf, "run", input); !errors.Is(err, types.ErrSuspended) {
		t.Fatal(err)
	}
	if writes.Load() != 0 || calls.Load() != 1 {
		t.Fatalf("calls=%d writes=%d", calls.Load(), writes.Load())
	}
	readsBefore := reads.Load()
	state, err := e.Inspect(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 1 || state.Pending[0].ID != "marker/approval-call" || state.Pending[0].Kind != types.InterruptApproval {
		t.Fatalf("pending = %+v", state.Pending)
	}

	decision := types.ApprovalDecision{Approved: true, ModifiedArgs: map[string]any{"value": "approved"}}
	if err := e.Decide(ctx, "run", "marker/approval-call", "key", decision); err != nil {
		t.Fatal(err)
	}
	if err := e.Decide(ctx, "run", "marker/approval-call", "key", decision); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	if err := e.Decide(ctx, "run", "marker/approval-call", "other", types.ApprovalDecision{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed reply: %v", err)
	}
	if err := e.Decide(ctx, "run", "missing", "key", decision); !errors.Is(err, types.ErrInterruptNotFound) {
		t.Fatalf("unknown interrupt: %v", err)
	}

	result, err := e.Wait(ctx, "run")
	if err != nil || result == nil {
		t.Fatalf("%v %v", result, err)
	}
	// The first provider turn replays from its record; the read completed
	// before the suspension is not repeated.
	if writes.Load() != 1 || calls.Load() != 2 || reads.Load() != max(readsBefore, 1) {
		t.Fatalf("calls=%d reads=%d writes=%d", calls.Load(), reads.Load(), writes.Load())
	}
	// Starting the same run again returns the recorded result.
	if _, err := e.Run(ctx, wf, "run", input); err != nil {
		t.Fatal(err)
	}
	if writes.Load() != 1 || calls.Load() != 2 {
		t.Fatal("replayed effects")
	}
	if err := e.Decide(ctx, "run", "marker/approval-call", "key", decision); !errors.Is(err, ErrClosed) {
		t.Fatalf("reply to a finished run: %v", err)
	}
	if err := e.Start(ctx, wf, "run", []types.Message{types.UserMsg(types.Text("other"))}); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed input: %v", err)
	}
	other := e.Register("other", factory)
	if err := e.Start(ctx, other, "run", input); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed workflow: %v", err)
	}
}

func TestToolsReceiveStableIdempotencyKey(t *testing.T) {
	ctx := testContext(t)
	e := newEngine()
	var keys []string
	factory := func(string) *agent.Agent {
		write := &types.ToolFunc{Def: types.ToolDef{Name: "write"}, Fn: func(ctx context.Context, _ map[string]any) (string, error) {
			keys = append(keys, types.ToolContextFrom(ctx).String(types.ToolContextIdempotencyKey, ""))
			return "ok", nil
		}}
		p := &agenttest.ScriptedProvider{Responses: [][]types.Delta{agenttest.ToolCallResponse("call-1", "write", nil), agenttest.TextResponse("done")}}
		return agent.NewAgent(agent.AgentConfig{Provider: p, Tools: types.NewToolRegistry(write), ToolContext: types.NewToolContext(map[string]any{"limit": 3})})
	}
	wf := e.Register("", factory)
	startWorker(t, e)
	if _, err := e.Run(ctx, wf, "keyed", []types.Message{types.UserMsg(types.Text("go"))}); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0] != "keyed:step:tool-call-1#0" {
		t.Fatalf("keys = %q", keys)
	}
}

func TestBudgetReceiptsReplayAfterResume(t *testing.T) {
	ctx := testContext(t)
	e := newEngine()
	var current *types.Budget
	var calls atomic.Int32
	factory := func(string) *agent.Agent {
		current = types.NewBudget(types.BudgetPolicy{MaxRequests: 5, PerCallTokens: 100})
		write := &types.ToolFunc{Def: types.ToolDef{Name: "write"}, Fn: func(context.Context, map[string]any) (string, error) { return "ok", nil }}
		return agent.NewAgent(agent.AgentConfig{SystemPrompt: "system", Provider: stagedNoUsage{&calls}, Budget: current, Tools: types.NewToolRegistry(types.WithMarkers(write, types.Marker{Kind: "approval"}))})
	}
	wf := e.Register("", factory)
	startWorker(t, e)
	input := []types.Message{types.UserMsg(types.Text("go"))}
	if _, err := e.Run(ctx, wf, "budget", input); !errors.Is(err, types.ErrSuspended) {
		t.Fatal(err)
	}
	if err := e.Decide(ctx, "budget", "marker/write-call", "k", types.ApprovalDecision{Approved: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(ctx, "budget"); err != nil {
		t.Fatal(err)
	}
	// Two provider calls, each settled at its reservation because neither
	// reported usage. The first was restored from its receipt on replay.
	if calls.Load() != 2 || current.Uncertain() != 2 || current.Usage().Total() != 200 || current.Usage().Requests != 2 {
		t.Fatalf("calls=%d uncertain=%d usage=%+v", calls.Load(), current.Uncertain(), current.Usage())
	}
}

// stagedNoUsage asks for one gated tool call, then answers. It never reports
// usage.
type stagedNoUsage struct{ calls *atomic.Int32 }

func (p stagedNoUsage) Stream(_ context.Context, req types.Request) (<-chan types.Delta, error) {
	m := req.Messages
	p.calls.Add(1)
	deltas := agenttest.TextResponse("done")
	if len(m) <= 2 {
		deltas = agenttest.ToolCallResponse("write-call", "write", nil)
	}
	out := make(chan types.Delta, len(deltas))
	for _, d := range deltas {
		out <- d
	}
	close(out)
	return out, nil
}

type stagedProvider struct {
	calls *atomic.Int32
	first []types.Delta
}

func (p stagedProvider) Stream(_ context.Context, req types.Request) (<-chan types.Delta, error) {
	messages := req.Messages
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
	ctx := testContext(t)
	var parentCalls, childCalls, writes atomic.Int32
	e := newEngine()
	factory := func(string) *agent.Agent {
		write := &types.ToolFunc{Def: types.ToolDef{Name: "write"}, Fn: func(context.Context, map[string]any) (string, error) { writes.Add(1); return "written", nil }}
		return agent.NewAgent(agent.AgentConfig{SystemPrompt: "parent", Provider: stagedProvider{&parentCalls, agenttest.ToolCallResponse("child-call", "delegate_to_child", map[string]any{"task": "write"})}, Budget: types.NewBudget(types.BudgetPolicy{MaxRequests: 10}), SubAgents: []agent.SubAgentDef{{Name: "child", SystemPrompt: "child", Provider: stagedProvider{&childCalls, agenttest.ToolCallResponse("write-call", "write", nil)}, Tools: types.NewToolRegistry(types.WithMarkers(write, types.Marker{Kind: "approval"}))}}})
	}
	wf := e.Register("", factory)
	startWorker(t, e)
	input := []types.Message{types.UserMsg(types.Text("go"))}
	if _, err := e.Run(ctx, wf, "child", input); !errors.Is(err, types.ErrSuspended) {
		t.Fatal(err)
	}
	state, err := e.Inspect(ctx, "child")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Pending) != 1 || writes.Load() != 0 {
		t.Fatalf("pending=%d writes=%d", len(state.Pending), writes.Load())
	}
	if err := e.Router().Reply(ctx, types.InterruptReply{ID: state.Pending[0].ID, IdempotencyKey: "decision", Decision: types.ApprovalDecision{Approved: true}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := e.Run(ctx, wf, "child", input); err != nil {
			t.Fatal(err)
		}
	}
	if writes.Load() != 1 || parentCalls.Load() != 2 || childCalls.Load() != 2 {
		t.Fatalf("writes=%d parent=%d child=%d", writes.Load(), parentCalls.Load(), childCalls.Load())
	}
}

func TestAdmissionApprovalPersistsBeforeProviderAndRestoresGrant(t *testing.T) {
	ctx := testContext(t)
	e := newEngine()
	var calls atomic.Int32
	var budget *types.Budget
	factory := func(string) *agent.Agent {
		budget = types.NewBudget(types.BudgetPolicy{Limit: types.USD(.25), PerCallCost: types.USD(.4), OnExceed: types.BudgetRequireApproval})
		return agent.NewAgent(agent.AgentConfig{SystemPrompt: "system", Provider: stagedProvider{&calls, agenttest.TextResponse("done")}, Budget: budget})
	}
	wf := e.Register("", factory)
	startWorker(t, e)
	input := []types.Message{types.UserMsg(types.Text("go"))}
	if _, err := e.Run(ctx, wf, "grant", input); !errors.Is(err, types.ErrSuspended) {
		t.Fatal(err)
	}
	state, err := e.Inspect(ctx, "grant")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || len(state.Uncertain) != 0 || len(state.Pending) != 1 {
		t.Fatalf("provider=%d uncertain=%d pending=%d", calls.Load(), len(state.Uncertain), len(state.Pending))
	}
	if err := e.Decide(ctx, "grant", state.Pending[0].ID, "decision", types.ApprovalDecision{Approved: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Wait(ctx, "grant"); err != nil {
		t.Fatal(err)
	}
	if budget.Remaining() != types.USD(.5) || budget.Usage().Requests != 1 {
		t.Fatalf("remaining=%v usage=%+v", budget.Remaining(), budget.Usage())
	}
	if calls.Load() != 1 {
		t.Fatal("repeated provider call")
	}
}

func TestIndependentChildBudgetFailsBeforeChildDispatch(t *testing.T) {
	ctx := testContext(t)
	e := newEngine()
	var parentCalls, childCalls atomic.Int32
	factory := func(string) *agent.Agent {
		return agent.NewAgent(agent.AgentConfig{SystemPrompt: "parent", Provider: stagedProvider{&parentCalls, agenttest.ToolCallResponse("child-call", "delegate_to_child", map[string]any{"task": "go"})}, Budget: types.NewBudget(types.BudgetPolicy{MaxRequests: 10}), SubAgents: []agent.SubAgentDef{{Name: "child", Provider: stagedProvider{&childCalls, agenttest.TextResponse("done")}, Options: []agent.AgentOption{agent.WithBudget(types.NewBudget(types.BudgetPolicy{MaxRequests: 5}))}}}})
	}
	wf := e.Register("", factory)
	startWorker(t, e)
	_, err := e.Run(ctx, wf, "separate", []types.Message{types.UserMsg(types.Text("go"))})
	if err == nil || childCalls.Load() != 0 {
		t.Fatalf("independent child dispatched: %d, err=%v", childCalls.Load(), err)
	}
}

func TestSetupFailureFailsRun(t *testing.T) {
	ctx := testContext(t)
	e := newEngine()
	wf := e.Register("", func(string) *agent.Agent { return nil })
	startWorker(t, e)
	_, err := e.Run(ctx, wf, "broken", []types.Message{types.UserMsg(types.Text("go"))})
	if !errors.Is(err, ErrFailed) || !strings.Contains(err.Error(), "nil agent") {
		t.Fatalf("err = %v", err)
	}
	state, err := e.Inspect(ctx, "broken")
	if err != nil || state.Status != "failed" || state.Error == "" {
		t.Fatalf("state = %+v, %v", state, err)
	}
}

// TestToolPanicLeavesUncertainStep: a tool that panics may have changed
// external state, so the run parks with ErrIndeterminate instead of calling
// the tool again, and continues once the host reconciles the step.
func TestToolPanicLeavesUncertainStep(t *testing.T) {
	ctx := testContext(t)
	e := newEngine()
	var writes, calls atomic.Int32
	factory := func(string) *agent.Agent {
		write := &types.ToolFunc{Def: types.ToolDef{Name: "write"}, Fn: func(context.Context, map[string]any) (string, error) {
			writes.Add(1)
			panic("connection lost after the write")
		}}
		return agent.NewAgent(agent.AgentConfig{SystemPrompt: "system", Provider: stagedProvider{&calls, agenttest.ToolCallResponse("write-call", "write", nil)}, Tools: types.NewToolRegistry(write)})
	}
	wf := e.Register("", factory)
	startWorker(t, e)
	input := []types.Message{types.UserMsg(types.Text("go"))}
	if _, err := e.Run(ctx, wf, "uncertain", input); !errors.Is(err, ErrIndeterminate) {
		t.Fatalf("err = %v", err)
	}
	state, err := e.Inspect(ctx, "uncertain")
	if err != nil || len(state.Uncertain) != 1 || state.Uncertain[0].Step != "tool-write-call" {
		t.Fatalf("state = %+v, %v", state, err)
	}
	if err := e.Reconcile(ctx, "uncertain", "tool-other", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("reconcile of an unknown step: %v", err)
	}
	verified := types.StepResult{Kind: types.StepKindTool, ToolCallID: "write-call", ToolResult: "verified written"}
	if err := e.Reconcile(ctx, "uncertain", "tool-write-call", &verified); err != nil {
		t.Fatal(err)
	}
	if err := e.Reconcile(ctx, "uncertain", "tool-write-call", &verified); !errors.Is(err, ErrConflict) {
		t.Fatalf("second reconcile: %v", err)
	}
	if _, err := e.Wait(ctx, "uncertain"); err != nil {
		t.Fatal(err)
	}
	if writes.Load() != 1 || calls.Load() != 2 {
		t.Fatalf("writes=%d calls=%d, want 1 and 2", writes.Load(), calls.Load())
	}
}
