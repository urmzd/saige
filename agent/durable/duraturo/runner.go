package duraturo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	dt "github.com/urmzd/duraturo"

	"github.com/urmzd/saige/agent/types"
)

// Record names. A step's records sit under keys duraturo derives from these
// names and their occurrence in the run, so they must never change.
const (
	stepPrefix      = "step:"      // a step's recorded result
	uncertainPrefix = "uncertain:" // a step left without a known outcome
	reconcilePrefix = "reconcile:" // the host's resolution of an uncertain step
	dispatchName    = "dispatch"   // nested: one dispatch round started
	releasedName    = "released"   // nested: the round ended before dispatch
	reserveName     = "reserve"    // nested: the budget reservation of the round
)

// errUnrecorded makes a probe step report absence without recording it.
var errUnrecorded = errors.New("no recorded outcome")

// runner is the types.StepRunner one workflow attempt hands to the agent. It
// lives for one attempt; everything durable is a duraturo record.
type runner struct {
	e      *Engine
	runID  string
	budget *types.Budget

	mu     sync.Mutex
	posted map[string]posted // interrupts posted in this attempt, by ID
}

var (
	_ types.StepRunner              = (*runner)(nil)
	_ types.ApprovalRunner          = (*runner)(nil)
	_ types.InterruptRouter         = (*runner)(nil)
	_ types.ConcurrentStepRunner    = (*runner)(nil)
	_ types.SharedBudgetRunner      = (*runner)(nil)
	_ types.BudgetReservationRunner = (*runner)(nil)
)

// ConcurrentSteps is false: record keys follow call order, so steps must run
// one at a time.
func (r *runner) ConcurrentSteps() bool { return false }

// SharedBudgetOnly is true: recovery receipts belong to the run's budget.
func (r *runner) SharedBudgetOnly() bool { return true }

// RunStep executes or replays a named step. A recorded result returns without
// calling fn. A step that started on an earlier attempt and left no result is
// uncertain: the run parks with ErrIndeterminate until Reconcile resolves it,
// and fn does not run again unless the host permits a retry.
//
// While fn runs, the tool context carries the step's duraturo idempotency key
// under types.ToolContextIdempotencyKey.
func (r *runner) RunStep(ctx context.Context, name string, fn func(context.Context) (types.StepResult, error)) (types.StepResult, error) {
	if !dt.InRun(ctx) {
		return types.StepResult{}, errors.New("durable step outside a run")
	}
	for {
		var (
			uncertain bool
			released  bool
			liveErr   error // error of a call made on this attempt
		)
		raw, err := dt.Step(ctx, stepPrefix+name, func(ctx context.Context) ([]byte, error) {
			for {
				// A round is one try at dispatch. A round that ended before
				// dispatch (a suspension or a refused admission) records
				// that, so a later attempt starts a new round instead of
				// treating the call as uncertain.
				fresh := false
				if _, err := dt.Step(ctx, dispatchName, func(ctx context.Context) (int, error) {
					fresh = true
					info, _ := dt.FromContext(ctx)
					return info.Attempt, nil
				}); err != nil {
					return nil, err
				}
				if !fresh {
					_, err := dt.Step(ctx, releasedName, func(context.Context) (bool, error) { return false, errUnrecorded })
					if err == nil {
						continue
					}
					if errors.Is(err, errUnrecorded) {
						uncertain = true
					}
					return nil, err
				}
				result, err := safeStep(withIdempotencyKey(ctx), fn)
				if errors.Is(err, types.ErrSuspended) || errors.Is(err, types.ErrBudgetAdmission) {
					released, liveErr = true, err
					if _, rerr := dt.Step(ctx, releasedName, func(context.Context) (bool, error) { return true, nil }); rerr != nil {
						return nil, errors.Join(err, rerr)
					}
					return nil, err
				}
				// A stopped provider call that returns its committed partial
				// turn has a known outcome: record the partial turn and still
				// report the stop to this caller.
				if err != nil && (!errors.Is(err, context.Canceled) || !truncatedLLM(result)) {
					uncertain, liveErr = true, err
					return nil, err
				}
				liveErr = err
				encoded, encErr := encode(result)
				if encErr != nil {
					uncertain, liveErr = true, encErr
					return nil, encErr
				}
				return encoded, nil
			}
		})
		switch {
		case err == nil:
			var result types.StepResult
			if derr := decode(raw, &result); derr != nil {
				return types.StepResult{}, derr
			}
			return result, liveErr
		case released:
			return types.StepResult{}, err
		case !uncertain:
			return types.StepResult{}, err
		}
		retry, err := r.uncertain(ctx, name, liveErr)
		if err != nil || !retry.permit {
			return retry.result, err
		}
		if retry.receipt != nil && r.budget != nil {
			if err := r.budget.Restore(*retry.receipt); err != nil {
				return types.StepResult{}, fmt.Errorf("restore budget receipt: %w", err)
			}
		}
	}
}

// resolution is the host's answer to an uncertain step.
type resolution struct {
	result  types.StepResult
	permit  bool                 // retry the step in a new round
	receipt *types.BudgetReceipt // charge of the uncertain attempt, kept on retry
}

// reconcilePayload is the event record Reconcile writes. Result and Receipt
// are gob. An empty Result permits a retry.
type reconcilePayload struct {
	Result  []byte `json:"result,omitempty"`
	Receipt []byte `json:"receipt,omitempty"`
}

// uncertain records that step name has no known outcome, then waits for the
// host's reconciliation. liveErr is the failure seen on this attempt, if any.
func (r *runner) uncertain(ctx context.Context, name string, liveErr error) (resolution, error) {
	msg := "an earlier attempt stopped during the step"
	if liveErr != nil {
		msg = liveErr.Error()
	}
	if _, err := dt.Step(ctx, uncertainPrefix+name, func(context.Context) (string, error) { return msg, nil }); err != nil {
		return resolution{}, err
	}
	if liveErr != nil {
		return resolution{}, fmt.Errorf("%w: %s: %w", ErrIndeterminate, name, liveErr)
	}
	p, err := dt.Event[reconcilePayload](ctx, reconcilePrefix+name)
	if err != nil {
		return resolution{}, fmt.Errorf("%w: %s: %w", ErrIndeterminate, name, err)
	}
	if len(p.Result) > 0 {
		var result types.StepResult
		if err := decode(p.Result, &result); err != nil {
			return resolution{}, err
		}
		return resolution{result: result}, nil
	}
	res := resolution{permit: true}
	if len(p.Receipt) > 0 {
		var receipt types.BudgetReceipt
		if err := decode(p.Receipt, &receipt); err != nil {
			return resolution{}, err
		}
		res.receipt = &receipt
	}
	return res, nil
}

// RecordReservation saves the conservative charge of the step being
// dispatched before the provider request, so a crash leaves it for
// reconciliation.
func (r *runner) RecordReservation(ctx context.Context, _ string, receipt types.BudgetReceipt) error {
	if dt.IdempotencyKey(ctx) == "" {
		return fmt.Errorf("%w: reservation outside a running step", ErrConflict)
	}
	_, err := dt.Step(ctx, reserveName, func(context.Context) ([]byte, error) { return encode(receipt) })
	return err
}

// withIdempotencyKey adds the step's duraturo idempotency key to the tool
// context.
func withIdempotencyKey(ctx context.Context) context.Context {
	key := dt.IdempotencyKey(ctx)
	if key == "" {
		return ctx
	}
	return types.WithToolContext(ctx, types.ToolContextFrom(ctx).With(types.ToolContextIdempotencyKey, key))
}

func safeStep(ctx context.Context, fn func(context.Context) (types.StepResult, error)) (result types.StepResult, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("step panic: %v", p)
		}
	}()
	return fn(ctx)
}

// truncatedLLM reports whether result is a provider turn committed before it
// finished, marked with TruncationContent. A turn that still holds tool calls
// does not qualify: their arguments may be incomplete.
func truncatedLLM(result types.StepResult) bool {
	if result.Kind != types.StepKindLLM || result.Message == nil {
		return false
	}
	marked := false
	for _, c := range result.Message.Content {
		switch c.(type) {
		case types.TruncationContent:
			marked = true
		case types.ToolUseContent:
			return false
		}
	}
	return marked
}

// ResolveApproval posts the request as an approval interrupt. It returns the
// recorded decision, or types.ErrSuspended (with the run parked) while none
// exists. An expired request returns ErrClosed. Replay must ask the same
// request; a changed one returns ErrConflict.
func (r *runner) ResolveApproval(ctx context.Context, req types.ApprovalRequest) (types.ApprovalDecision, error) {
	payload, err := json.Marshal(req.ToolCall)
	if err != nil {
		return types.ApprovalDecision{}, err
	}
	in := types.Interrupt{ID: req.ID, RunID: r.runID, Kind: types.InterruptApproval, Payload: payload, Markers: req.Markers}
	reply, err := r.post(ctx, in, &req)
	if err != nil {
		return types.ApprovalDecision{}, err
	}
	return reply.Decision, nil
}

// Post records an interrupt raised inside the run. A new or unanswered
// interrupt returns types.ErrSuspended and parks the run; after Reply, the
// replayed Post delivers the recorded reply on the returned channel. An
// interrupt posted without ExpiresAt expires after the engine's ApprovalTTL.
// Replay must post the same interrupt; a changed one returns ErrConflict.
func (r *runner) Post(ctx context.Context, in types.Interrupt) (<-chan types.InterruptReply, error) {
	if in.RunID == "" {
		in.RunID = r.runID
	}
	reply, err := r.post(ctx, in, nil)
	if err != nil {
		return nil, err
	}
	return delivered(reply), nil
}

// Reply records a reply for an interrupt of this run.
func (r *runner) Reply(ctx context.Context, reply types.InterruptReply) error {
	return r.e.reply(ctx, r.runID, reply)
}

// Pending lists this run's unanswered interrupts.
func (r *runner) Pending(ctx context.Context, _ string) ([]types.Interrupt, error) {
	return r.e.Router().Pending(ctx, r.runID)
}
