package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/urmzd/saige/agent/types"
)

// reserveProviderCall admits capacity and records the recovery charge before
// any provider operation. No reservation is held while a decision is pending.
func (a *Agent) reserveProviderCall(stepCtx context.Context, stream *EventStream, provider types.Provider, stepName string) (types.BudgetReservation, types.Pricing, error) {
	caps, _ := types.ProviderCapabilities(provider)
	reservation, err := a.cfg.Budget.Reserve(types.NewID(), caps.Pricing)
	if errors.Is(err, types.ErrBudgetExceeded) && a.cfg.Budget.Policy().OnExceed == types.BudgetRequireApproval {
		call := types.ToolUseContent{ID: stepName, Name: "budget"}
		_, _, approved := a.awaitApprovalPhase(stepCtx, stream, call, []types.Marker{a.cfg.Budget.ApprovalMarker()}, "budget-admission")
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
	if recorder, ok := a.cfg.StepRunner.(types.BudgetReservationRunner); ok {
		receipt := a.cfg.Budget.ReservationReceipt(reservation.ID, types.ProviderModel(provider), caps.Pricing)
		if err := recorder.RecordReservation(stepCtx, stepName, receipt); err != nil {
			_ = a.cfg.Budget.Settle(reservation.ID, receipt.Model, caps.Pricing, types.TokenUsage{}, false)
			return types.BudgetReservation{}, caps.Pricing, fmt.Errorf("%w: persist reservation: %w", types.ErrBudgetAdmission, err)
		}
	}
	return reservation, caps.Pricing, nil
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
	if runner, ok := a.cfg.StepRunner.(prefixStepRunner); ok && runner.SharedBudgetOnly() && a.cfg.Budget != runner.parentBudget {
		return errors.New("durable child budgets require separate receipt ownership; use the shared run budget")
	}
	if _, ok := a.cfg.StepRunner.(types.ApprovalRunner); ok && a.cfg.CompactCfg != nil && a.cfg.CompactCfg.ToCompactor() != nil {
		return errors.New("durable approval replay requires compaction checkpoints; automatic compaction is unsupported")
	}
	return nil
}
