package runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/runtime"
	"github.com/urmzd/saige/flow/turn/native"
)

// guardedCalls is a delegation tool that asks, per call, for the approval its
// callee's policy names: only a call to "guarded" asks, of carol or an
// oncall, and self-approval follows self.
type guardedCalls struct {
	*countingTool
	approvers []string
	self      bool
}

func (g guardedCalls) Approval(call saigetypes.ToolCallPart) (native.ApprovalRequest, bool) {
	if call.Arguments["agent"] != "guarded" {
		return native.ApprovalRequest{}, false
	}
	return native.ApprovalRequest{Prompt: "Allow ops to call guarded?", Approvers: g.approvers, SelfApproval: g.self}, true
}

// newGuarded is a parking world whose agent delegates through guardedCalls.
// A definition names approvers only alongside approval.ask, so this one names
// none, and only the call's approvers may answer.
func newGuarded(t *testing.T, approvers []string, self bool) (*parking, guardedCalls) {
	t.Helper()
	w := newWorld(t)
	tool := guardedCalls{countingTool: &countingTool{def: saigetypes.ToolDef{Name: "call_agent", Parameters: saigetypes.ParameterSchema{
		Type: "object", Required: []string{"agent"}, Properties: map[string]saigetypes.PropertyDef{"agent": {Type: "string"}},
	}}}, approvers: approvers, self: self}
	p := &parking{world: w, deploy: tool.countingTool, model: &agenttest.ScriptedProvider{Responses: [][]saigetypes.Delta{
		call("c1", "call_agent", map[string]any{"agent": "guarded"}), agenttest.TextResponse("Done."),
	}}}
	w.specs.set(native.Spec{
		Name: "ops", Provider: native.ProviderOffline, Model: "scripted", SystemPrompt: "You delegate.",
		Tools: []string{"call_agent"},
	})
	p.exec = sandboxOf(t, w.specs, map[string]saigetypes.Provider{"ops": p.model, "docs-bot": bystander()},
		runtime.WithCatalog(func(runtime.Request, func(event.Action) error) (native.Tools, error) {
			return native.StaticTools(tool), nil
		}))
	return p, tool
}

// A per-call approval is answered only by the people the call names, by
// subject or by role, in place of the spec's approvers; the requester is
// refused unless the call allows self-approval. The approved call runs.
func TestAPerCallApprovalIsAnsweredOnlyByItsOwnApprovers(t *testing.T) {
	roles := runtime.WithRoles(func(_ context.Context, tenant, who string) ([]string, error) {
		if tenant == "acme" && who == "dave" {
			return []string{"oncall"}, nil
		}
		return nil, nil
	})
	for _, tc := range []struct {
		name      string
		approvers []string
		self      bool
		by        string
		allowed   bool
	}{
		{"a named subject", []string{"carol", "role:oncall"}, false, "carol", true},
		{"a holder of a named role", []string{"carol", "role:oncall"}, false, "dave", true},
		{"someone not named", []string{"carol", "role:oncall"}, false, "bob", false},
		{"the spec's approver", []string{"carol", "role:oncall"}, false, "spec-approver", false},
		{"the requester, though named", []string{"ana", "carol"}, false, "ana", false},
		{"the requester, named, with self-approval", []string{"ana", "carol"}, true, "ana", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, tool := newGuarded(t, tc.approvers, tc.self)
			rt := p.rt(t, roles)
			p.workers(t, rt, 2, 0)
			runID := p.accept(t, rt, requested("evt-1"))
			q := p.parkedOn(t, rt, runID)
			if q.Kind != event.QuestionApproval || len(q.Approvers) != len(tc.approvers) || q.SelfApproval != tc.self {
				t.Fatalf("parked on %+v, want an approval carrying the call's approvers", q)
			}
			err := p.answerAs(rt, runID, q.ID, tc.by, tc.by, event.Answer{Decision: event.DecisionApprove})
			if tc.allowed != (err == nil) || (err != nil && !errors.Is(err, runtime.ErrNotAllowed)) {
				t.Fatalf("err = %v, want allowed=%v", err, tc.allowed)
			}
			if !tc.allowed {
				return
			}
			p.result(t, rt, runID)
			if n := len(tool.calls()); n != 1 {
				t.Errorf("call_agent ran %d times after approval, want 1", n)
			}
		})
	}
}
