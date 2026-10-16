package online_test

import (
	"testing"

	"github.com/urmzd/saige/agent/agenttest"
	"github.com/urmzd/saige/eval/online"
)

func TestOnlineGeneratorRevisionTracksSystemPrompt(t *testing.T) {
	p := &agenttest.ScriptedProvider{}
	base := &online.BudgetedGenerator{Provider: p}
	changed := &online.BudgetedGenerator{Provider: p, System: "new system prompt"}
	if base.Revision() == changed.Revision() {
		t.Fatal("online generator lost system configuration")
	}
}
