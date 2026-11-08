package definition_test

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/urmzd/saige/flow/definition"
)

var update = flag.Bool("update", false, "rewrite golden files")

// everyField sets every field of the old spec.
func everyField() definition.AgentSpec {
	return definition.AgentSpec{
		Name:         " Docs-Bot ",
		Description:  "Answers \"how do I\" questions.",
		Model:        "claude-x",
		Provider:     "Anthropic",
		SystemPrompt: "\nYou answer questions.\n\n---\nNever guess.\n",
		Tools:        []string{"search", "mcp:linear:write", "github_comment", "mcp:notion"},
		Sources:      []definition.Corpus{{Name: "Handbook", URI: "kb://handbook", Options: map[string]string{"k": "3"}}},
		Access:       definition.AgentSpecAccess{Spawn: []string{"writer", "Reviewer"}, WorkspaceAreas: []string{"notes/", "drafts/"}},
		Fallbacks:    []definition.AgentSpecModel{{Provider: "openai", Model: "gpt-x"}},
		FallbackOn:   []string{"rate_limit", "auth"},
		Approvals:    []string{"github_comment", "tag_writer"},
		Approvers:    []string{"role:admin", "ada"},
		SelfApproval: true,
		Answerers:    []string{"eve"},
		TaggedOnly:   true,
	}
}

func TestFromAgentSpecGolden(t *testing.T) {
	rec, err := definition.FromAgentSpec(everyField(), 7)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := json.MarshalIndent(rec.Policy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got := []byte(rec.Content + "\n# policy\n" + string(policy) + "\n")
	path := filepath.Join("testdata", "fromagentspec.golden")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if string(got) != string(want) {
		t.Errorf("FromAgentSpec drifted from %s:\n%s", path, got)
	}
	if rec.Name != "docs-bot" || rec.Version != "7.0.0" {
		t.Errorf("record = %s@%s", rec.Name, rec.Version)
	}

	again, err := definition.FromAgentSpec(everyField(), 7)
	if err != nil || again.Content != rec.Content || again.Policy.Hash() != rec.Policy.Hash() {
		t.Error("FromAgentSpec is not deterministic")
	}

	// The result is a definition flow stores and compiles.
	src := definition.NewMemory()
	rec.Tenant, rec.CreatedBy = "acme", "migration"
	stored, err := src.Put(context.Background(), rec)
	if err != nil {
		t.Fatalf("the converted definition does not store: %v", err)
	}
	a, err := definition.Parse(stored.Content, stored.Policy, definition.Checks{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Spec.SystemPrompt != "You answer questions.\n\n---\nNever guess." {
		t.Errorf("prompt = %q", a.Spec.SystemPrompt)
	}
	if len(a.Spec.Tools) != 4 || a.Spec.Tools[1] != "mcp:linear:write" || a.Spec.Tools[2] != "mcp:notion" {
		t.Errorf("tools = %v", a.Spec.Tools)
	}
}

func TestFromAgentSpecListsNamesThatBreakTheRule(t *testing.T) {
	spec := everyField()
	spec.Name = "docs_bot"
	spec.Access.Spawn = []string{"ok", "9lives", "also_bad"}
	_, err := definition.FromAgentSpec(spec, 1)
	var ne *definition.NameError
	if !errors.As(err, &ne) || !errors.Is(err, definition.ErrInvalid) {
		t.Fatalf("err = %v, want a NameError", err)
	}
	if ne.Agent != "docs_bot" || len(ne.Names) != 3 || ne.Names[0] != "docs_bot" || ne.Names[1] != "9lives" || ne.Names[2] != "also_bad" {
		t.Errorf("names = %v", ne.Names)
	}
	if _, err := definition.FromAgentSpec(everyField(), 0); !errors.Is(err, definition.ErrInvalid) {
		t.Errorf("revision 0 = %v", err)
	}
}
