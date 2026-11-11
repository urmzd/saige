package runtime

import (
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/flow/turn/native"
)

type approvingCall struct{ *agenttest.MockTool }

func (approvingCall) Approval(call saigetypes.ToolCallPart) (native.ApprovalRequest, bool) {
	return native.ApprovalRequest{Approvers: []string{"role:admin"}}, call.Arguments["agent"] == "guarded"
}

// The host's call tool keeps the per-call approval of the tool it wraps, so a
// host that governs calls per callee is still asked about each one.
func TestHostCallToolForwardsApproval(t *testing.T) {
	inner := approvingCall{&agenttest.MockTool{Def: saigetypes.ToolDef{Name: "call_agent"}}}
	var tool saigetypes.Tool = &hostCallTool{Tool: inner}
	a, ok := tool.(native.Approving)
	if !ok {
		t.Fatal("the host call tool hides Approving")
	}
	if req, ok := a.Approval(saigetypes.ToolCallPart{Arguments: map[string]any{"agent": "guarded"}}); !ok || len(req.Approvers) != 1 {
		t.Fatalf("guarded call = %+v, %v", req, ok)
	}
	if _, ok := a.Approval(saigetypes.ToolCallPart{Arguments: map[string]any{"agent": "open"}}); ok {
		t.Fatal("an open call asked for approval")
	}
	plain := &hostCallTool{Tool: &agenttest.MockTool{Def: saigetypes.ToolDef{Name: "call_agent"}}}
	if _, ok := plain.Approval(saigetypes.ToolCallPart{}); ok {
		t.Fatal("a tool that does not approve per call asked")
	}
}
