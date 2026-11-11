package definition_test

import (
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/definition"
)

func TestPolicyZeroGrantsNothing(t *testing.T) {
	var p definition.Policy
	if p.MayWrite("github") {
		t.Error("the zero policy grants MCP writes")
	}
	if p.MayUseArea("notes/a") || p.MayUseArea("") {
		t.Error("the zero policy grants a workspace area")
	}
	if len(p.Subscriptions) != 0 || len(p.Approvers) != 0 || p.SelfApproval || len(p.Knowledge) != 0 {
		t.Errorf("the zero policy holds grants: %+v", p)
	}
	content := "---\napiVersion: saige/v1\nname: docs-bot\nversion: 1.0.0\nmodel: offline/echo\ntools:\n  mcp:\n    - server: github\n---\nAnswer.\n"
	a, err := definition.Parse(content, p, definition.Checks{})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Spec.Access.WorkspaceAreas) != 0 || len(a.Spec.Access.Spawn) != 0 {
		t.Errorf("a zero policy compiled to access %+v", a.Spec.Access)
	}
	if strings.Join(a.Spec.Tools, ",") != "mcp:github" {
		t.Errorf("a zero policy compiled tools %v, want the read-only grant", a.Spec.Tools)
	}
	if a.Spec.SelfApproval || len(a.Spec.Approvers) > 0 || len(a.Spec.Answerers) > 0 || a.Spec.TaggedOnly {
		t.Errorf("a zero policy compiled answer rules %+v", a.Spec)
	}
}

func TestPolicyHashDetectsAnEdit(t *testing.T) {
	base := definition.Policy{WorkspaceAreas: []string{"b/", "a/"}, Approvers: []string{"ada"}}
	reordered := definition.Policy{WorkspaceAreas: []string{"a/", "b/"}, Approvers: []string{"ada"}}
	if base.Hash() != reordered.Hash() {
		t.Error("order changed the hash")
	}
	if !strings.HasPrefix(base.Hash(), "sha256:") {
		t.Errorf("hash %q has no algorithm", base.Hash())
	}
	for name, edit := range map[string]definition.Policy{
		"area":          {WorkspaceAreas: []string{"a/", "c/"}, Approvers: []string{"ada"}},
		"approver":      {WorkspaceAreas: []string{"a/", "b/"}, Approvers: []string{"eve"}},
		"self approval": {WorkspaceAreas: []string{"a/", "b/"}, Approvers: []string{"ada"}, SelfApproval: true},
		"mcp write":     {WorkspaceAreas: []string{"a/", "b/"}, Approvers: []string{"ada"}, MCPWrite: []string{"github"}},
	} {
		if edit.Hash() == base.Hash() {
			t.Errorf("an edit to the %s kept the hash", name)
		}
	}
}

func TestPolicyValidateRefusesBadEntries(t *testing.T) {
	for name, p := range map[string]definition.Policy{
		"empty area":         {WorkspaceAreas: []string{""}},
		"duplicate approver": {Approvers: []string{"a", "a"}},
		"unknown fallback":   {FallbackOn: []string{"sometimes"}},
		"any with others":    {FallbackOn: []string{"any", "auth"}},
		"no channel":         {Subscriptions: []definition.Subscription{{Types: []string{"message."}}}},
		"no types":           {Subscriptions: []definition.Subscription{{Channel: "slack://T01"}}},
		"corpus without uri": {Knowledge: []definition.Corpus{{Name: "docs"}}},
	} {
		if err := p.Validate(); err == nil {
			t.Errorf("%s: validated", name)
		}
	}
	if err := (definition.Policy{}).Validate(); err != nil {
		t.Errorf("zero policy: %v", err)
	}
}

// Choices offer nothing by default, normalize to a set, and refuse entries
// a host could not apply.
func TestPolicyChoices(t *testing.T) {
	var zero definition.Policy
	if zero.Choices.AllowsModel("anthropic/claude") || zero.Choices.AllowsEffort("high") {
		t.Error("the zero policy offers a choice")
	}
	empty := definition.Policy{Choices: &definition.Choices{}}
	if empty.Normalize().Choices != nil || empty.Hash() != zero.Hash() {
		t.Error("choices that offer nothing differ from none")
	}
	p := definition.Policy{Choices: &definition.Choices{
		Models:  []string{" openai/gpt ", "anthropic/claude"},
		Efforts: []string{"High", "low"},
	}}.Normalize()
	if !p.Choices.AllowsModel("openai/gpt") || !p.Choices.AllowsEffort("high") || p.Choices.AllowsEffort("medium") {
		t.Errorf("normalized choices %+v", p.Choices)
	}
	if p.Hash() == zero.Hash() {
		t.Error("choices kept the hash")
	}
	for name, c := range map[string]definition.Choices{
		"bare model":     {Models: []string{"claude"}},
		"repeated model": {Models: []string{"a/b", "a/b"}},
		"odd effort":     {Efforts: []string{"very-high"}},
	} {
		if err := (definition.Policy{Choices: &c}).Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, c)
		}
	}
	content := "---\napiVersion: saige/v1\nname: docs-bot\nversion: 1.0.0\nmodel: offline/echo\n---\nAnswer.\n"
	if _, err := definition.Parse(content, definition.Policy{Choices: &definition.Choices{Models: []string{"nowhere/x"}}}, definition.Checks{}); err == nil {
		t.Error("Parse accepted a model choice behind an unknown provider")
	}
	if _, err := definition.Parse(content, p, definition.Checks{Providers: []string{"anthropic", "openai", "offline"}}); err != nil {
		t.Errorf("Parse refused valid choices: %v", err)
	}
}

// Accept admits a provider the deployment adds while it runs; without it, only
// the provider table counts.
func TestChecksAcceptAdmitsAProviderAddedAtRunTime(t *testing.T) {
	content := "---\napiVersion: saige/v1\nname: docs-bot\nversion: 1.0.0\nmodel: team-anthropic/claude-sonnet-5-5\n---\nAnswer.\n"
	if _, err := definition.Parse(content, definition.Policy{}, definition.Checks{}); err == nil {
		t.Fatal("Parse accepted a provider outside the table with no Accept")
	}
	accept := func(name string) bool { return name == "team-anthropic" }
	if _, err := definition.Parse(content, definition.Policy{}, definition.Checks{Accept: accept}); err != nil {
		t.Fatalf("Parse refused a provider Accept admits: %v", err)
	}
	refused := strings.Replace(content, "team-anthropic/", "elsewhere/", 1)
	if _, err := definition.Parse(refused, definition.Policy{}, definition.Checks{Accept: accept}); err == nil {
		t.Fatal("Parse accepted a provider Accept refuses")
	}
}
