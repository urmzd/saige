package eval

import (
	"testing"

	"github.com/urmzd/saige/agent/types"
	topeval "github.com/urmzd/saige/eval"
)

func TestGeneratorRevisionTracksJudgeConfiguration(t *testing.T) {
	p := &offeringProvider{offering: textOffering()}
	base := NewGenerator(p)
	if base.Revision() != NewGenerator(p).Revision() {
		t.Fatal("same generator configuration has different revisions")
	}
	altered := NewGenerator(p, WithSystemPrompt("new system prompt"))
	if base.Revision() == altered.Revision() {
		t.Fatal("system prompt missing from revision")
	}
	changedOffering := textOffering()
	changedOffering.ID = "test/other@test"
	other := NewGenerator(&offeringProvider{offering: changedOffering})
	if base.Revision() == other.Revision() {
		t.Fatal("provider offering missing from revision")
	}
	converted := NewGenerator(p, WithConversion(types.ConversionPolicy{Scope: "private-tenant"}))
	if base.Revision() == converted.Revision() {
		t.Fatal("conversion policy missing from revision")
	}
	a := topeval.NewJudgeScorer(base)
	b := topeval.NewJudgeScorer(altered)
	if topeval.ScorerRevision(a) == topeval.ScorerRevision(b) {
		t.Fatal("judge lost generator revision")
	}
}
