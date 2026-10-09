package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"

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
// different model, or to dials that change the branch's, is recorded on
// branch as ConfigContent, so later turns use it, and reported to emit as a
// RouteDelta. It returns the switch, or nil when nothing changes.
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
	if sw == nil {
		return nil, nil
	}
	model := sw.Model
	if model == o.Model {
		model = ""
	}
	var dials *types.Dials
	if sw.Dials != nil && !sw.Dials.IsZero() {
		messages, err := tr.FlattenBranch(branch)
		if err != nil {
			return nil, err
		}
		current, _ := a.prepareMessages(messages)
		if next := current.dials.Merge(*sw.Dials); !reflect.DeepEqual(next, current.dials) {
			d := sw.Dials.Clone()
			dials = &d
		}
	}
	// A switch that changes neither the model nor the dials is ignored, so
	// a ladder that has reached its top ends the escalation.
	if model == "" && dials == nil {
		return nil, nil
	}
	reason := sw.Reason
	if reason == "" {
		reason = string(o.Kind)
	}
	cfg := types.SystemMessage{Content: []types.SystemContent{types.ConfigContent{Model: model, Dials: dials, Reason: reason}}}
	if err := a.appendToBranch(ctx, tr, branch, cfg); err != nil {
		return nil, err
	}
	a.cfg.Logger.Info("outcome policy switched configuration",
		"agent", a.cfg.Name, "outcome", o.Kind, "from", o.Model, "to", cmp.Or(model, o.Model), "dials", dials != nil, "reason", reason)
	if emit != nil {
		emit(types.RouteDelta{Provider: o.Provider, Model: cmp.Or(model, o.Model), Reason: reason})
	}
	return &types.Switch{Model: model, Dials: dials, Reason: reason}, nil
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
