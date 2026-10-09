package duraturo

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/urmzd/duraturo/pkg/replay"
	"github.com/urmzd/duraturo/pkg/run"

	"github.com/urmzd/saige/agent/types"
)

// attempt opens one workflow attempt of run id by hand, the way a worker
// does after a claim: a replay frame over the run's records.
type attempt struct {
	t   *testing.T
	e   *Engine
	id  string
	n   int
	ctx context.Context
	r   *runner
}

func acceptRun(t *testing.T, e *Engine, id string) {
	t.Helper()
	if err := e.lgr.Accept(context.Background(), run.Run{ID: id, Name: DefaultWorkflow, Status: run.StatusPending, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func (a *attempt) next() *attempt {
	a.t.Helper()
	_, records, err := a.e.lgr.Load(context.Background(), a.id)
	if err != nil {
		a.t.Fatal(err)
	}
	a.n++
	frame := replay.NewFrame(a.id, a.n, a.e.lgr, run.JSONCodec{}, records, nil)
	a.ctx = replay.WithFrame(context.Background(), frame)
	a.r = &runner{e: a.e, runID: a.id, posted: map[string]posted{}}
	return a
}

func open(t *testing.T, e *Engine, id string) *attempt {
	t.Helper()
	acceptRun(t, e, id)
	return (&attempt{t: t, e: e, id: id}).next()
}

func TestUncertainAttemptNeedsReconciliation(t *testing.T) {
	e := newEngine()
	a := open(t, e, "run")
	_, err := a.r.RunStep(a.ctx, "write", func(context.Context) (types.StepResult, error) { panic("after external write") })
	if !errors.Is(err, ErrIndeterminate) {
		t.Fatalf("panic: %v", err)
	}
	a.next()
	_, err = a.r.RunStep(a.ctx, "write", func(context.Context) (types.StepResult, error) {
		t.Fatal("repeated uncertain write")
		return types.StepResult{}, nil
	})
	if !errors.Is(err, ErrIndeterminate) || !errors.Is(err, run.ErrParked) {
		t.Fatalf("replay: %v", err)
	}
	state, err := e.Inspect(context.Background(), "run")
	if err != nil || len(state.Uncertain) != 1 || state.Uncertain[0].Error != "step panic: after external write" {
		t.Fatalf("state = %+v, %v", state, err)
	}
	result := types.StepResult{Kind: types.StepKindTool, ToolResult: "verified written", ToolBlocks: []types.ToolResultBlock{{Data: []byte{1, 2}}}}
	if err := e.Reconcile(context.Background(), "run", "write", &result); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		a.next()
		restored, err := a.r.RunStep(a.ctx, "write", func(context.Context) (types.StepResult, error) {
			t.Fatal("reconciled step ran")
			return types.StepResult{}, nil
		})
		if err != nil || len(restored.ToolBlocks) != 1 || restored.ToolBlocks[0].Data[1] != 2 {
			t.Fatalf("replay %d: %+v, %v", i, restored, err)
		}
	}
	if state, _ := e.Inspect(context.Background(), "run"); len(state.Uncertain) != 0 {
		t.Fatalf("reconciled step still uncertain: %+v", state.Uncertain)
	}
}

func TestCrashedStepKeepsReservationAndRetriesOnlyWhenPermitted(t *testing.T) {
	e := newEngine()
	a := open(t, e, "crash")
	receipt := types.BudgetReceipt{ID: "crashed-attempt", Cost: types.USD(.25), Usage: types.TokenUsage{Requests: 1, InputTokens: 100}, Uncertain: true}
	// The step's goroutine ends after the reservation is saved, without
	// returning: nothing after that point runs, as in a process crash.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = a.r.RunStep(a.ctx, "side-effect", func(ctx context.Context) (types.StepResult, error) {
			if err := a.r.RecordReservation(ctx, "side-effect", receipt); err != nil {
				t.Error(err)
			}
			runtime.Goexit()
			return types.StepResult{}, nil
		})
	}()
	<-done

	a.next()
	_, err := a.r.RunStep(a.ctx, "side-effect", func(context.Context) (types.StepResult, error) {
		t.Fatal("repeated external effect")
		return types.StepResult{}, nil
	})
	if !errors.Is(err, ErrIndeterminate) {
		t.Fatal(err)
	}
	state, err := e.Inspect(context.Background(), "crash")
	if err != nil || len(state.Uncertain) != 1 {
		t.Fatalf("state = %+v, %v", state, err)
	}
	if got := state.Uncertain[0].Receipt; got == nil || got.Cost != types.USD(.25) || !got.Uncertain {
		t.Fatalf("lost crash accounting: %+v", got)
	}
	if err := e.Reconcile(context.Background(), "crash", "side-effect", nil); err != nil {
		t.Fatal(err)
	}

	// The permitted retry runs the step once and keeps the uncertain charge.
	for i := 0; i < 2; i++ {
		a.next()
		a.r.budget = types.NewBudget(types.BudgetPolicy{MaxRequests: 5})
		runs := 0
		got, err := a.r.RunStep(a.ctx, "side-effect", func(context.Context) (types.StepResult, error) {
			runs++
			return types.StepResult{Kind: types.StepKindTool, ToolResult: "retried"}, nil
		})
		if err != nil || got.ToolResult != "retried" {
			t.Fatalf("attempt %d: %+v, %v", i, got, err)
		}
		if want := 1 - i; runs != want {
			t.Fatalf("attempt %d ran the step %d times, want %d", i, runs, want)
		}
		if a.r.budget.Uncertain() != 1 || a.r.budget.Usage().Requests != 1 {
			t.Fatalf("attempt %d budget: uncertain=%d usage=%+v", i, a.r.budget.Uncertain(), a.r.budget.Usage())
		}
	}
}

func TestSuspendedStepIsNotUncertain(t *testing.T) {
	e := newEngine()
	a := open(t, e, "run")
	_, err := a.r.RunStep(a.ctx, "llm-0", func(context.Context) (types.StepResult, error) {
		return types.StepResult{}, types.ErrSuspended
	})
	if !errors.Is(err, types.ErrSuspended) {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		a.next()
		runs := 0
		_, err := a.r.RunStep(a.ctx, "llm-0", func(context.Context) (types.StepResult, error) {
			runs++
			if i == 0 {
				return types.StepResult{}, types.ErrSuspended
			}
			return types.StepResult{Kind: types.StepKindLLM, Message: &types.AssistantMessage{}}, nil
		})
		if runs != 1 || (i == 0) != errors.Is(err, types.ErrSuspended) {
			t.Fatalf("round %d: runs=%d err=%v", i, runs, err)
		}
	}
	a.next()
	if _, err := a.r.RunStep(a.ctx, "llm-0", func(context.Context) (types.StepResult, error) {
		t.Fatal("recorded step ran")
		return types.StepResult{}, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTruncatedTurnIsRecorded(t *testing.T) {
	e := newEngine()
	a := open(t, e, "run")
	partial := types.StepResult{Kind: types.StepKindLLM, Message: &types.AssistantMessage{Content: []types.AssistantContent{types.TextContent{Text: "part"}, types.TruncationContent{Reason: "interrupted"}}}}
	got, err := a.r.RunStep(a.ctx, "llm-0", func(context.Context) (types.StepResult, error) { return partial, context.Canceled })
	if !errors.Is(err, context.Canceled) || got.Message == nil {
		t.Fatalf("%+v, %v", got, err)
	}
	a.next()
	got, err = a.r.RunStep(a.ctx, "llm-0", func(context.Context) (types.StepResult, error) {
		t.Fatal("truncated turn ran again")
		return types.StepResult{}, nil
	})
	if err != nil || len(got.Message.Content) != 2 {
		t.Fatalf("%+v, %v", got, err)
	}
}

func TestApprovalExpiryAndConflict(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	e.ApprovalTTL = time.Hour
	a := open(t, e, "run")
	req := types.ApprovalRequest{ID: "approval", ToolCall: types.ToolUseContent{ID: "call", Arguments: map[string]any{"integer": 9007199254740993}}}
	if _, err := a.r.ResolveApproval(a.ctx, req); !errors.Is(err, types.ErrSuspended) || !errors.Is(err, run.ErrParked) {
		t.Fatal(err)
	}
	a.next()
	// Large integers survive the record, so replay does not see a change.
	if _, err := a.r.ResolveApproval(a.ctx, req); !errors.Is(err, types.ErrSuspended) {
		t.Fatalf("replay: %v", err)
	}
	a.next()
	changed := req
	changed.ToolCall.Arguments = map[string]any{"integer": 1}
	if _, err := a.r.ResolveApproval(a.ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed request: %v", err)
	}

	e.ApprovalTTL = time.Millisecond
	expired := open(t, e, "expired")
	if _, err := expired.r.ResolveApproval(expired.ctx, req); !errors.Is(err, types.ErrSuspended) {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := e.Decide(ctx, "expired", req.ID, "key", types.ApprovalDecision{Approved: true}); !errors.Is(err, ErrClosed) || !errors.Is(err, types.ErrInterruptExpired) {
		t.Fatalf("late decision: %v", err)
	}
	expired.next()
	if _, err := expired.r.ResolveApproval(expired.ctx, req); !errors.Is(err, ErrClosed) {
		t.Fatalf("expired replay: %v", err)
	}
}

func TestPostFromInsideAStepUsesTheStepScope(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	a := open(t, e, "run")
	in := question("run")
	_, err := a.r.RunStep(a.ctx, "llm-0", func(stepCtx context.Context) (types.StepResult, error) {
		_, err := a.r.Post(stepCtx, in)
		return types.StepResult{}, err
	})
	if !errors.Is(err, types.ErrSuspended) {
		t.Fatal(err)
	}
	if err := e.Router().Reply(ctx, types.InterruptReply{ID: in.ID, IdempotencyKey: "k", Answer: json.RawMessage(`"yes"`)}); err != nil {
		t.Fatal(err)
	}
	a.next()
	got, err := a.r.RunStep(a.ctx, "llm-0", func(stepCtx context.Context) (types.StepResult, error) {
		ch, err := a.r.Post(stepCtx, in)
		if err != nil {
			return types.StepResult{}, err
		}
		reply := <-ch
		return types.StepResult{Kind: types.StepKindTool, ToolResult: string(reply.Answer)}, nil
	})
	if err != nil || got.ToolResult != `"yes"` {
		t.Fatalf("%+v, %v", got, err)
	}
}
