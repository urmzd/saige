package types

import "context"

// CapabilityPolicy maps a tool's declared capability class to a gate
// outcome. A class missing from the map requires approval, so a policy that
// forgets a class fails closed rather than open.
type CapabilityPolicy map[ToolCapability]GateOutcome

// DefaultCapabilityPolicy is the policy CapabilityGate applies when given
// nil: reads run, writes and undeclared tools wait for a human, and
// destructive tools are denied. A deployment that wants destructive tools
// behind approval instead says so explicitly.
func DefaultCapabilityPolicy() CapabilityPolicy {
	return CapabilityPolicy{
		ToolCapabilityRead:        GateAllow,
		ToolCapabilityWrite:       GateRequireApproval,
		ToolCapabilityDestructive: GateDeny,
		ToolCapabilityUnknown:     GateRequireApproval,
	}
}

// Outcome returns the outcome for a capability. The zero value and any
// unrecognized class are judged as ToolCapabilityUnknown.
func (p CapabilityPolicy) Outcome(c ToolCapability) GateOutcome {
	if outcome, ok := p[c.Effective()]; ok {
		return outcome
	}
	return GateRequireApproval
}

// CapabilityGate decides each call from the tool's declared capability
// class (ToolDef.Capability). A nil policy uses DefaultCapabilityPolicy.
//
// It judges what a tool says about itself, so it is only as good as the
// declarations: a tool that declares read and writes anyway is not caught.
// Tools from sources you do not control (MCP servers, generated tools)
// usually declare nothing and land in the unknown class, which is why that
// class defaults to approval. Compose it with types.Gates and a name- or
// argument-based gate for finer rules; the most restrictive verdict wins.
func CapabilityGate(policy CapabilityPolicy) ToolGate {
	if policy == nil {
		policy = DefaultCapabilityPolicy()
	}
	// Copy, so a caller changing its map after construction cannot change
	// a gate that may be running concurrently.
	fixed := make(CapabilityPolicy, len(policy))
	for c, outcome := range policy {
		fixed[c.Effective()] = outcome
	}
	return GateFunc(func(_ context.Context, def ToolDef, _ map[string]any) GateDecision {
		class := def.Capability.Effective()
		switch fixed.Outcome(class) {
		case GateAllow:
			return Allow()
		case GateDeny:
			return Deny("tool " + def.Name + " is " + string(class) + " and denied by capability policy")
		default:
			return RequireApproval("tool " + def.Name + " is " + string(class) + " and requires approval")
		}
	})
}
