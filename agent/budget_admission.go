package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/urmzd/saige/agent/types"
)

// reserveProviderCall admits capacity and records the recovery charge before
// any provider operation. No reservation is held while a decision is pending.
// A turn interrupted before or during admission returns errTurnInterrupted,
// so the loop replaces the turn instead of failing the run.
func (a *Agent) reserveProviderCall(stepCtx context.Context, stream *EventStream, provider types.Provider, stepName string) (types.BudgetReservation, types.Pricing, error) {
	caps, _ := types.ProviderCapabilities(provider)
	if interruptRequested(stepCtx) {
		return types.BudgetReservation{}, caps.Pricing, errTurnInterrupted
	}
	// A spawned child's first call uses the reservation taken when it was
	// spawned, so the capacity admitted then is the capacity it spends.
	if reservation, ok := a.admission.take(); ok {
		return a.recordReservation(stepCtx, provider, caps.Pricing, reservation, stepName)
	}
	reservation, err := a.cfg.Budget.Reserve(types.NewID(), caps.Pricing)
	if errors.Is(err, types.ErrBudgetExceeded) && a.cfg.Budget.Policy().OnExceed == types.BudgetRequireApproval {
		call := types.ToolUseContent{ID: stepName, Name: budgetToolName}
		_, approved := a.awaitApprovalPhase(stepCtx, stream, call, []types.Marker{a.cfg.Budget.ApprovalMarker()}, "budget-admission")
		if interruptRequested(stepCtx) {
			// SubmitInterruptReplace cancelled the wait: the turn is being
			// replaced, not refused. Nothing is reserved yet.
			return types.BudgetReservation{}, caps.Pricing, errTurnInterrupted
		}
		if stopped := stream.runError(); stopped != nil {
			return types.BudgetReservation{}, caps.Pricing, stopped
		}
		if approved {
			a.cfg.Budget.Grant(0)
			reservation, err = a.cfg.Budget.Reserve(types.NewID(), caps.Pricing)
		}
	}
	if err != nil {
		return types.BudgetReservation{}, caps.Pricing, fmt.Errorf("%w: %w", types.ErrBudgetAdmission, err)
	}
	return a.recordReservation(stepCtx, provider, caps.Pricing, reservation, stepName)
}

// recordReservation saves an admitted reservation with a durable runner
// before dispatch, and releases it when that save fails.
func (a *Agent) recordReservation(stepCtx context.Context, provider types.Provider, pricing types.Pricing, reservation types.BudgetReservation, stepName string) (types.BudgetReservation, types.Pricing, error) {
	if recorder, ok := a.cfg.StepRunner.(types.BudgetReservationRunner); ok {
		receipt := a.cfg.Budget.ReservationReceipt(reservation.ID, types.ProviderModel(provider), pricing)
		if err := recorder.RecordReservation(stepCtx, stepName, receipt); err != nil {
			_ = a.cfg.Budget.Settle(reservation.ID, receipt.Model, pricing, types.TokenUsage{}, false)
			return types.BudgetReservation{}, pricing, fmt.Errorf("%w: persist reservation: %w", types.ErrBudgetAdmission, err)
		}
	}
	return reservation, pricing, nil
}

func (a *Agent) settleProviderCall(reservation types.BudgetReservation, pricing types.Pricing, provider types.Provider, usage *types.UsageDelta, failed bool) (types.BudgetReceipt, *types.UsageDelta, error) {
	unknown := usage == nil || failed
	if usage == nil {
		usage = &types.UsageDelta{}
	}
	usage.AccountingID = reservation.ID
	model := types.ProviderModel(provider)
	if usage.ResponseModel != "" {
		model = usage.ResponseModel
	}
	err := a.cfg.Budget.Settle(reservation.ID, model, pricing, types.UsageFromDelta(*usage), unknown)
	return a.cfg.Budget.Receipt(reservation.ID), usage, err
}

func (a *Agent) validateDurableConfiguration() error {
	if runner, ok := asPrefixRunner(a.cfg.StepRunner); ok && runner.SharedBudgetOnly() && a.cfg.Budget != runner.parentBudget {
		return errors.New("durable child budgets require separate receipt ownership; use the shared run budget")
	}
	if _, ok := a.cfg.StepRunner.(types.ApprovalRunner); ok && a.cfg.CompactCfg != nil && a.cfg.CompactCfg.ToCompactor() != nil {
		return errors.New("durable approval replay requires compaction checkpoints; automatic compaction is unsupported")
	}
	return nil
}
