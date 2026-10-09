package types

import (
	"context"
	"fmt"
	"strings"
	"sync"
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
func TestGatesMostRestrictiveVerdictWins(t *testing.T) {
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

// An approval verdict must not shadow a later denial: a human click would
// otherwise run a call that policy refuses.
func TestGatesDenyBeatsApprovalInEitherOrder(t *testing.T) {
	approve := PrefixApprovalGate("x", "write_")
	deny := DenyListGate("write_secrets")
	tests := []struct {
		name string
		gate ToolGate
		tool string
		want GateOutcome
	}{
		{"approval then deny", Gates(approve, deny), "write_secrets", GateDeny},
		{"deny then approval", Gates(deny, approve), "write_secrets", GateDeny},
		{"approval only matches", Gates(approve, deny), "write_notes", GateRequireApproval},
		{"neither matches", Gates(approve, deny), "read_notes", GateAllow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.gate.Check(context.Background(), def(tt.tool), nil); got.Outcome != tt.want {
				t.Errorf("outcome = %v, want %v", got.Outcome, tt.want)
			}
		})
	}
}

func TestGatesMergeApprovalReasonsAndKeepFirstMarker(t *testing.T) {
	custom := &Marker{Kind: "custom"}
	first := GateFunc(func(context.Context, ToolDef, map[string]any) GateDecision {
		d := RequireApproval("writes need review")
		d.Marker = custom
		return d
	})
	second := GateFunc(func(context.Context, ToolDef, map[string]any) GateDecision {
		return RequireApproval("cost above limit")
	})
	got := Gates(first, second).Check(context.Background(), def("write"), nil)
	if got.Outcome != GateRequireApproval {
		t.Fatalf("outcome = %v, want approval", got.Outcome)
	}
	if got.Reason != "writes need review; cost above limit" {
		t.Errorf("reason = %q, want both reasons", got.Reason)
	}
	if got.Marker != custom {
		t.Errorf("marker = %v, want the first custom marker", got.Marker)
	}
}

func TestGatesApprovalCarriesRewrittenArguments(t *testing.T) {
	clamp := GateFunc(func(context.Context, ToolDef, map[string]any) GateDecision {
		return GateDecision{Outcome: GateAllow, ModifiedArgs: map[string]any{"limit": 10}}
	})
	got := Gates(PrefixApprovalGate("", "s"), clamp).Check(context.Background(), def("search"), map[string]any{"limit": 99})
	if got.Outcome != GateRequireApproval {
		t.Fatalf("outcome = %v, want approval", got.Outcome)
	}
	if got.ModifiedArgs["limit"] != 10 {
		t.Errorf("args = %v, want the clamped arguments", got.ModifiedArgs)
	}
}

// The default reason must name the tool under check, and concurrent checks
// must not share state.
func TestPrefixApprovalGateDefaultReasonNamesEachTool(t *testing.T) {
	g := PrefixApprovalGate("", "write_", "delete_")
	if got := g.Check(context.Background(), def("write_a"), nil).Reason; !strings.Contains(got, "write_a") {
		t.Errorf("reason = %q, want it to name write_a", got)
	}
	if got := g.Check(context.Background(), def("delete_b"), nil).Reason; !strings.Contains(got, "delete_b") {
		t.Errorf("reason = %q, want it to name delete_b", got)
	}

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("write_%d", i)
			if got := g.Check(context.Background(), def(name), nil).Reason; !strings.Contains(got, name) {
				t.Errorf("reason = %q, want it to name %s", got, name)
			}
		}(i)
	}
	wg.Wait()
}
