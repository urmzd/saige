package mcp

import (
	"context"
	"fmt"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/urmzd/saige/agent/types"
)

// CapabilityFromAnnotations classifies a tool from its MCP annotations.
//
// Annotations are claims made by the server, not facts. A claim that raises
// the class (destructiveHint, or a write tool with no destructiveHint, which
// MCP defines as destructive) is always honored. A claim that lowers it
// (readOnlyHint, destructiveHint=false) is honored only when trusted is set;
// otherwise the tool stays ToolCapabilityUnknown, which policies treat as the
// most dangerous class. Missing annotations also give Unknown.
func CapabilityFromAnnotations(a *mcpsdk.ToolAnnotations, trusted bool) types.ToolCapability {
	if a == nil {
		return types.ToolCapabilityUnknown
	}
	if a.ReadOnlyHint {
		if trusted {
			return types.ToolCapabilityRead
		}
		return types.ToolCapabilityUnknown
	}
	if a.DestructiveHint == nil || *a.DestructiveHint {
		return types.ToolCapabilityDestructive
	}
	if trusted {
		return types.ToolCapabilityWrite
	}
	return types.ToolCapabilityUnknown
}

// AnnotationsForCapability is the reverse mapping, for publishing a local
// tool over MCP. Unknown publishes no annotations, which MCP clients read as
// "may be destructive".
func AnnotationsForCapability(c types.ToolCapability) *mcpsdk.ToolAnnotations {
	f := false
	t := true
	switch c.Effective() {
	case types.ToolCapabilityRead:
		return &mcpsdk.ToolAnnotations{ReadOnlyHint: true}
	case types.ToolCapabilityWrite:
		return &mcpsdk.ToolAnnotations{DestructiveHint: &f}
	case types.ToolCapabilityDestructive:
		return &mcpsdk.ToolAnnotations{DestructiveHint: &t}
	default:
		return nil
	}
}

// CapabilityPolicy maps each capability class to a gate outcome.
type CapabilityPolicy struct {
	Read        types.GateOutcome
	Write       types.GateOutcome
	Destructive types.GateOutcome
	Unknown     types.GateOutcome
}

// DefaultCapabilityPolicy allows reads and asks a human for everything else,
// including tools whose class is unknown.
func DefaultCapabilityPolicy() CapabilityPolicy {
	return CapabilityPolicy{
		Read:        types.GateAllow,
		Write:       types.GateRequireApproval,
		Destructive: types.GateRequireApproval,
		Unknown:     types.GateRequireApproval,
	}
}

// CapabilityGate decides by ToolDef.Capability. It is a policy input, never a
// grant: compose it with types.Gates so another gate can still deny, and give
// untrusted servers their own ServerSpec.Gate as well. Start from
// DefaultCapabilityPolicy; the zero CapabilityPolicy allows everything.
func CapabilityGate(policy CapabilityPolicy) types.ToolGate {
	return types.GateFunc(func(_ context.Context, def types.ToolDef, _ map[string]any) types.GateDecision {
		class := def.Capability.Effective()
		var outcome types.GateOutcome
		switch class {
		case types.ToolCapabilityRead:
			outcome = policy.Read
		case types.ToolCapabilityWrite:
			outcome = policy.Write
		case types.ToolCapabilityDestructive:
			outcome = policy.Destructive
		default:
			outcome = policy.Unknown
		}
		reason := fmt.Sprintf("tool %s is classified %s", def.Name, class)
		switch outcome {
		case types.GateDeny:
			return types.Deny(reason)
		case types.GateRequireApproval:
			return types.RequireApproval(reason)
		default:
			return types.Allow()
		}
	})
}
