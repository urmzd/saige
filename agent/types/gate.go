package types

import (
	"context"
	"strings"
)

// GateOutcome is what a ToolGate decided about one pending tool call.
type GateOutcome int

const (
	// GateAllow: run the call as-is.
	GateAllow GateOutcome = iota
	// GateRequireApproval: pause and ask a human before running it. The agent
	// loop emits a MarkerDelta and waits, exactly as it does for a MarkedTool.
	GateRequireApproval
	// GateDeny: do not run it. The model is told the call was refused, which
	// keeps the transcript coherent and lets it try something else.
	GateDeny
)

// GateDecision is a gate's verdict on one pending call.
type GateDecision struct {
	Outcome GateOutcome
	// Reason is surfaced to the model on a denial and to the human on an
	// approval prompt. A denial with no reason teaches the model nothing and it
	// will usually retry the same call.
	Reason string
	// Marker overrides the approval prompt's presentation when the outcome is
	// GateRequireApproval. nil uses a default marker built from Reason.
	Marker *Marker
	// ModifiedArgs replaces the call's arguments when non-nil. This is how a
	// gate narrows a call rather than refusing it outright, e.g. clamping a
	// limit or rewriting a path to stay inside a sandbox.
	ModifiedArgs map[string]any
}

// Allow, Deny and RequireApproval build the three decisions.
func Allow() GateDecision { return GateDecision{Outcome: GateAllow} }

func Deny(reason string) GateDecision {
	return GateDecision{Outcome: GateDeny, Reason: reason}
}

func RequireApproval(reason string) GateDecision {
	return GateDecision{Outcome: GateRequireApproval, Reason: reason}
}

// ToolGate decides whether a tool call may proceed, before it runs.
//
// This is pre-gating in the literal sense: the gate sees the resolved tool
// definition and the model's actual arguments, and decides on that basis. The
// existing MarkedTool mechanism cannot do this. A marker is attached to a tool
// at registration time, so it is all-or-nothing per tool: either every call to
// write_file needs a human, or none does. A gate can allow reads and stop
// writes, allow paths under a root and refuse the rest, or wave through the
// nine safe tools an MCP server exposes and gate the tenth.
//
// Gates run for every tool call, including those to sub-agents, handoffs, and
// tools imported from MCP servers this process connects to. They cannot see
// calls a provider makes to its own server-side tools (see ServerTool): those
// never reach this process.
//
// Implementations must be safe for concurrent use: tool calls are fanned out.
type ToolGate interface {
	Check(ctx context.Context, def ToolDef, args map[string]any) GateDecision
}

// GateFunc adapts a plain function to the ToolGate interface.
type GateFunc func(ctx context.Context, def ToolDef, args map[string]any) GateDecision

func (f GateFunc) Check(ctx context.Context, def ToolDef, args map[string]any) GateDecision {
	return f(ctx, def, args)
}

// AllowAllGate is the default: every call proceeds. It exists so the agent loop
// never has to nil-check a gate.
type AllowAllGate struct{}

func (AllowAllGate) Check(context.Context, ToolDef, map[string]any) GateDecision {
	return Allow()
}

// Gates runs gates in order and returns the first non-allow decision, so the
// most restrictive verdict wins regardless of ordering. An empty chain allows.
//
// Composing this way keeps each gate single-purpose: an allow-list, an
// approval rule, and an argument clamp are three gates, not one function with
// three branches.
func Gates(gates ...ToolGate) ToolGate {
	return GateFunc(func(ctx context.Context, def ToolDef, args map[string]any) GateDecision {
		merged := args
		for _, g := range gates {
			d := g.Check(ctx, def, merged)
			if d.ModifiedArgs != nil {
				// Later gates judge the rewritten arguments, or a clamp applied
				// by an early gate could be evaluated only against the original.
				merged = d.ModifiedArgs
			}
			if d.Outcome != GateAllow {
				if d.ModifiedArgs == nil && merged != nil {
					d.ModifiedArgs = merged
				}
				return d
			}
		}
		if merged != nil {
			return GateDecision{Outcome: GateAllow, ModifiedArgs: merged}
		}
		return Allow()
	})
}

// AllowListGate permits only the named tools and denies everything else. Use it
// when tools arrive from somewhere you do not control, such as an MCP server
// whose tool list can change under you between connections.
func AllowListGate(names ...string) ToolGate {
	allowed := make(map[string]bool, len(names))
	for _, n := range names {
		allowed[n] = true
	}
	return GateFunc(func(_ context.Context, def ToolDef, _ map[string]any) GateDecision {
		if allowed[def.Name] {
			return Allow()
		}
		return Deny("tool " + def.Name + " is not on the allow list")
	})
}

// DenyListGate refuses the named tools and permits the rest.
func DenyListGate(names ...string) ToolGate {
	denied := make(map[string]bool, len(names))
	for _, n := range names {
		denied[n] = true
	}
	return GateFunc(func(_ context.Context, def ToolDef, _ map[string]any) GateDecision {
		if denied[def.Name] {
			return Deny("tool " + def.Name + " is denied by policy")
		}
		return Allow()
	})
}

// PrefixApprovalGate requires human approval for any tool whose name starts
// with one of the given prefixes. It is the cheap version of the policy most
// deployments want: read freely, confirm before writing.
func PrefixApprovalGate(reason string, prefixes ...string) ToolGate {
	return GateFunc(func(_ context.Context, def ToolDef, _ map[string]any) GateDecision {
		for _, p := range prefixes {
			if strings.HasPrefix(def.Name, p) {
				if reason == "" {
					reason = "tool " + def.Name + " requires approval"
				}
				return RequireApproval(reason)
			}
		}
		return Allow()
	})
}
