package bind

import (
	"context"
	"fmt"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/definition"
	"github.com/urmzd/saige/agent/types"
)

// defaultCapabilities is the capability policy of an approval block that
// names none: reads run, everything else asks. Destructive tools ask rather
// than fail, because without an approval block they would ask through
// their markers.
func defaultCapabilities() types.CapabilityPolicy {
	return types.CapabilityPolicy{
		types.ToolCapabilityRead:        types.GateAllow,
		types.ToolCapabilityWrite:       types.GateRequireApproval,
		types.ToolCapabilityDestructive: types.GateRequireApproval,
		types.ToolCapabilityUnknown:     types.GateRequireApproval,
	}
}

func outcome(decision string) types.GateOutcome {
	switch decision {
	case definition.DecisionAllow:
		return types.GateAllow
	case definition.DecisionDeny:
		return types.GateDeny
	}
	return types.GateRequireApproval
}

// approvalGate decides each call from a definition's approval block. Deny
// rules win over ask rules, ask rules over allow rules; a call no rule names
// asks when its tool carried an approval marker, and otherwise falls to its
// capability class.
type approvalGate struct {
	allow, ask, deny []definition.Rule
	caps             types.CapabilityPolicy
	// marked holds the approval message of each tool whose marker was
	// removed in favor of this gate.
	marked map[string]string
	// free names the tools that only delegate, such as delegate_to_<name>:
	// the sub-agent's own calls are gated, not the hand-over.
	free map[string]bool
}

func newApprovalGate(spec *definition.ApprovalSpec) (*approvalGate, error) {
	g := &approvalGate{caps: defaultCapabilities(), marked: map[string]string{}, free: map[string]bool{}}
	for class, decision := range spec.Capabilities {
		g.caps[types.ToolCapability(class)] = outcome(decision)
	}
	parse := func(list []string) ([]definition.Rule, error) {
		out := make([]definition.Rule, 0, len(list))
		for _, s := range list {
			r, err := definition.ParseRule(s)
			if err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		return out, nil
	}
	var err error
	if g.allow, err = parse(spec.Allow); err != nil {
		return nil, err
	}
	if g.ask, err = parse(spec.Ask); err != nil {
		return nil, err
	}
	if g.deny, err = parse(spec.Deny); err != nil {
		return nil, err
	}
	return g, nil
}

func (g *approvalGate) Check(_ context.Context, def types.ToolDef, args map[string]any) types.GateDecision {
	for _, r := range g.deny {
		if r.Matches(def.Name, args, false) {
			return types.Deny(fmt.Sprintf("%s is denied by the rule %s", def.Name, r))
		}
	}
	for _, r := range g.ask {
		if r.Matches(def.Name, args, false) {
			return types.RequireApproval(fmt.Sprintf("%s requires approval by the rule %s", def.Name, r))
		}
	}
	for _, r := range g.allow {
		if r.Matches(def.Name, args, true) {
			return types.Allow()
		}
	}
	if g.free[def.Name] {
		return types.Allow()
	}
	if msg, ok := g.marked[def.Name]; ok {
		return types.RequireApproval(msg)
	}
	class := def.Capability.Effective()
	switch g.caps.Outcome(class) {
	case types.GateAllow:
		return types.Allow()
	case types.GateDeny:
		return types.Deny(fmt.Sprintf("%s is %s and denied by the approval policy", def.Name, class))
	}
	return types.RequireApproval(fmt.Sprintf("%s is %s and requires approval", def.Name, class))
}

// unmark returns the tool without its approval marker when the marker is
// the outermost layer, recording the marker's message so the gate asks in
// its place. A marker below a decorator stays and still asks.
func (g *approvalGate) unmark(t types.Tool) types.Tool {
	mt, ok := t.(*types.MarkedTool)
	if !ok || len(mt.Markers) == 0 {
		return t
	}
	name := t.Definition().Name
	msg := mt.Markers[0].Message
	if msg == "" {
		msg = name + " requires approval"
	}
	g.marked[name] = msg
	return mt.Inner
}

// gates composes gates, skipping nil ones. It returns nil for none.
func gates(list ...types.ToolGate) types.ToolGate {
	var kept []types.ToolGate
	for _, g := range list {
		if g != nil {
			kept = append(kept, g)
		}
	}
	switch len(kept) {
	case 0:
		return nil
	case 1:
		return kept[0]
	}
	return types.Gates(kept...)
}

// approvalPolicy maps the block's counters onto agent.ApprovalPolicy.
func approvalPolicy(spec *definition.ApprovalSpec) *agent.ApprovalPolicy {
	return &agent.ApprovalPolicy{DenyAfter: spec.DenyAfter, RampAfter: spec.RampAfter, HideDenied: spec.HideDenied}
}
