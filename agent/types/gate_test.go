package types

import (
	"context"
	"testing"
)

func def(name string) ToolDef { return ToolDef{Name: name} }

func TestAllowAllGateLetsEverythingThrough(t *testing.T) {
	var g ToolGate = AllowAllGate{}
	if g.Check(context.Background(), def("anything"), nil).Outcome != GateAllow {
		t.Error("the default gate must allow")
	}
}

// The capability MarkedTool lacks: a per-argument decision on one tool, rather
// than an all-or-nothing marker attached at registration.
func TestAGateDecidesPerArgumentsNotPerTool(t *testing.T) {
	gate := GateFunc(func(_ context.Context, d ToolDef, args map[string]any) GateDecision {
		if d.Name != "file" {
			return Allow()
		}
		if mode, _ := args["mode"].(string); mode == "write" {
			return RequireApproval("writing needs a human")
		}
		return Allow()
	})

	read := gate.Check(context.Background(), def("file"), map[string]any{"mode": "read"})
	write := gate.Check(context.Background(), def("file"), map[string]any{"mode": "write"})

	if read.Outcome != GateAllow {
		t.Error("a read on the gated tool must pass")
	}
	if write.Outcome != GateRequireApproval {
		t.Error("a write on the same tool must be gated")
	}
}

func TestAllowListDeniesEverythingElse(t *testing.T) {
	g := AllowListGate("search", "read")
	if g.Check(context.Background(), def("search"), nil).Outcome != GateAllow {
		t.Error("a listed tool must pass")
	}
	d := g.Check(context.Background(), def("delete_everything"), nil)
	if d.Outcome != GateDeny {
		t.Error("an unlisted tool must be denied")
	}
	if d.Reason == "" {
		t.Error("a denial must carry a reason: without one the model just retries the same call")
	}
}

func TestDenyListBlocksOnlyTheNamed(t *testing.T) {
	g := DenyListGate("rm")
	if g.Check(context.Background(), def("rm"), nil).Outcome != GateDeny {
		t.Error("a denied tool must be blocked")
	}
	if g.Check(context.Background(), def("ls"), nil).Outcome != GateAllow {
		t.Error("an unlisted tool must pass a deny list")
	}
}

func TestPrefixApprovalGatesByNameShape(t *testing.T) {
	g := PrefixApprovalGate("confirm writes", "write_", "delete_")
	if g.Check(context.Background(), def("write_file"), nil).Outcome != GateRequireApproval {
		t.Error("a matching prefix must require approval")
	}
	if g.Check(context.Background(), def("read_file"), nil).Outcome != GateAllow {
		t.Error("a non-matching tool must pass")
	}
}

// The most restrictive verdict wins regardless of ordering, so composing gates
// cannot accidentally widen access.
func TestGatesReturnTheFirstNonAllowVerdict(t *testing.T) {
	permissive := GateFunc(func(context.Context, ToolDef, map[string]any) GateDecision { return Allow() })
	strict := DenyListGate("rm")

	if got := Gates(permissive, strict).Check(context.Background(), def("rm"), nil); got.Outcome != GateDeny {
		t.Error("a later denial must win over an earlier allow")
	}
	if got := Gates(strict, permissive).Check(context.Background(), def("rm"), nil); got.Outcome != GateDeny {
		t.Error("ordering must not change the verdict")
	}
	if got := Gates().Check(context.Background(), def("anything"), nil); got.Outcome != GateAllow {
		t.Error("an empty chain must allow")
	}
}

// A gate that narrows a call rather than refusing it must have its rewrite seen
// by the gates after it, or a later policy judges arguments that will not run.
func TestModifiedArgumentsFlowThroughTheChain(t *testing.T) {
	clamp := GateFunc(func(_ context.Context, _ ToolDef, args map[string]any) GateDecision {
		out := map[string]any{}
		for k, v := range args {
			out[k] = v
		}
		if n, ok := out["limit"].(int); ok && n > 10 {
			out["limit"] = 10
		}
		return GateDecision{Outcome: GateAllow, ModifiedArgs: out}
	})
	inspect := GateFunc(func(_ context.Context, _ ToolDef, args map[string]any) GateDecision {
		if n, _ := args["limit"].(int); n > 10 {
			return Deny("limit still too large")
		}
		return Allow()
	})

	got := Gates(clamp, inspect).Check(context.Background(), def("search"), map[string]any{"limit": 1000})
	if got.Outcome != GateAllow {
		t.Fatalf("outcome = %v, want allow: the second gate must judge the clamped value", got.Outcome)
	}
	if n, _ := got.ModifiedArgs["limit"].(int); n != 10 {
		t.Errorf("limit = %v, want the clamped 10 to reach the caller", got.ModifiedArgs["limit"])
	}
}
