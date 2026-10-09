package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/urmzd/saige/agent/tree"
	"github.com/urmzd/saige/agent/types"
)

// maxOutcomeSwitches bounds the model switches one Structured call makes, so
// a policy that keeps proposing models cannot loop forever.
const maxOutcomeSwitches = 3

// WithOutcomePolicy sets the policy that may switch models after an outcome
// such as a failed sub-agent or structured output that never validated.
func WithOutcomePolicy(p types.OutcomePolicy) AgentOption {
	return func(c *AgentConfig) { c.OutcomePolicy = p }
}

// observeOutcome asks the OutcomePolicy about o. An accepted switch to a
// different model is recorded on branch as ConfigContent, so later turns use
// it, and reported to emit as a RouteDelta. It returns the switch, or nil
// when the model stays.
func (a *Agent) observeOutcome(ctx context.Context, emit func(types.Delta), tr *tree.Tree, branch types.BranchID, provider types.Provider, o types.Outcome) (*types.Switch, error) {
	if a.cfg.OutcomePolicy == nil {
		return nil, nil
	}
	o.Agent = a.cfg.Name
	o.Provider = types.ProviderName(provider)
	if o.Model == "" {
		o.Model = types.ProviderModel(provider)
	}
	sw, err := a.cfg.OutcomePolicy.Observe(ctx, o)
	if err != nil {
		return nil, fmt.Errorf("outcome policy: %w", err)
	}
	if sw == nil || sw.Model == "" || sw.Model == o.Model {
		return nil, nil
	}
	reason := sw.Reason
	if reason == "" {
		reason = string(o.Kind)
	}
	cfg := types.SystemMessage{Content: []types.SystemContent{types.ConfigContent{Model: sw.Model, Reason: reason}}}
	if err := a.appendToBranch(ctx, tr, branch, cfg); err != nil {
		return nil, err
	}
	a.cfg.Logger.Info("outcome policy switched model",
		"agent", a.cfg.Name, "outcome", o.Kind, "from", o.Model, "to", sw.Model, "reason", reason)
	if emit != nil {
		emit(types.RouteDelta{Provider: o.Provider, Model: sw.Model, Reason: reason})
	}
	return &types.Switch{Model: sw.Model, Reason: reason}, nil
}

// observeSubAgentFailures reports each failed delegation in one turn to the
// OutcomePolicy and stops at the first switch. A cancelled run reports
// nothing: the child did not fail on its own.
func (a *Agent) observeSubAgentFailures(ctx context.Context, stream *EventStream, tr *tree.Tree, branch types.BranchID, provider types.Provider, results []toolResult) error {
	if a.cfg.OutcomePolicy == nil || ctx.Err() != nil {
		return nil
	}
	for _, r := range results {
		if r.subAgent == "" || r.err == "" || r.refused {
			continue
		}
		o := types.Outcome{Kind: types.OutcomeSubagentFailed, SubAgent: r.subAgent, Attempts: 1, Err: errors.New(r.err)}
		sw, err := a.observeOutcome(ctx, stream.send, tr, branch, provider, o)
		if err != nil {
			return err
		}
		if sw != nil {
			return nil
		}
	}
	return nil
}
