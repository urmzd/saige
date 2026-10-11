package bind

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/definition"
	"github.com/urmzd/saige/agent/types"
)

func TestApprovalGateArgumentRules(t *testing.T) {
	gate, err := newApprovalGate(&definition.ApprovalSpec{
		Allow: []string{"open_pull_request(branch:fix/*)"},
		Ask:   []string{"open_pull_request(draft:false)"},
		Deny:  []string{"open_pull_request(base:release)"},
	})
	if err != nil {
		t.Fatal(err)
	}
	def := types.ToolDef{Name: "open_pull_request", Capability: types.ToolCapabilityWrite}
	cases := []struct {
		name string
		args map[string]any
		want types.GateOutcome
	}{
		{"allow rule matches", map[string]any{"branch": "fix/typo", "base": "main"}, types.GateAllow},
		{"no rule matches, write asks", map[string]any{"branch": "feat/x", "base": "main"}, types.GateRequireApproval},
		{"missing argument never allows", map[string]any{"base": "main"}, types.GateRequireApproval},
		{"ask beats allow", map[string]any{"branch": "fix/typo", "draft": false}, types.GateRequireApproval},
		{"deny beats allow", map[string]any{"branch": "fix/typo", "base": "release"}, types.GateDeny},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gate.Check(context.Background(), def, tc.args).Outcome; got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
