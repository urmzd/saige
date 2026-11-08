package definition_test

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/definition"
)

func file(front, prompt string) string {
	return "---\napiVersion: saige/v1\nname: docs-bot\nversion: 1.0.0\n" + front + "---\n" + prompt + "\n"
}

func TestParseRefusesWhatFiberCannotHonor(t *testing.T) {
	cases := map[string]struct {
		front  string
		policy definition.Policy
		checks definition.Checks
	}{
		"no model":           {front: ""},
		"preset model":       {front: "model: fast\n"},
		"unknown provider":   {front: "model: acme/m1\n"},
		"too many fallbacks": {front: "model:\n  use: offline/a\n  fallback: [offline/b, offline/c, offline/d, offline/e, offline/f]\n"},
		"repeated fallback":  {front: "model:\n  use: offline/a\n  fallback: [offline/a]\n"},
		"missing fallback credential": {
			front:  "model:\n  use: offline/a\n  fallback: [openai/b]\n",
			checks: definition.Checks{Credential: func(p string) error { return map[string]error{"openai": errors.New("no key")}[p] }},
		},
		"fallback_on without fallbacks": {front: "model: offline/a\n", policy: definition.Policy{FallbackOn: []string{"auth"}}},
		"harness tools":                 {front: "model: offline/a\ntools:\n  harness: [shell]\n"},
		"mcp allow list":                {front: "model: offline/a\ntools:\n  mcp:\n    - server: github\n      allow: [x]\n"},
		"mcp grant in registry":         {front: "model: offline/a\ntools:\n  registry: [\"mcp:github\"]\n"},
		"write to an ungranted server":  {front: "model: offline/a\n", policy: definition.Policy{MCPWrite: []string{"github"}}},
		"approval allow":                {front: "model: offline/a\ntools:\n  registry: [x]\napproval:\n  allow: [x]\n"},
		"approval of an ungranted tool": {front: "model: offline/a\napproval:\n  ask: [x]\n"},
		"approvers without approvals":   {front: "model: offline/a\n", policy: definition.Policy{Approvers: []string{"ada"}}},
		"handoff sub-agent":             {front: "model: offline/a\nsubagents:\n  - ref: writer\n    mode: handoff\n"},
		"unrepresentable budget":        {front: "model: offline/a\nlimits:\n  budget:\n    max_cost: 0.0000001\n"},
		"bad name":                      {front: "model: offline/a\n"},
	}
	for name, c := range cases {
		content := file(c.front, "Answer.")
		if name == "bad name" {
			content = strings.Replace(content, "name: docs-bot", "name: Docs_Bot", 1)
		}
		if _, err := definition.Parse(content, c.policy, c.checks); !errors.Is(err, definition.ErrInvalid) {
			t.Errorf("%s: parse = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := definition.Parse(file("model: offline/a\n", ""), definition.Policy{}, definition.Checks{}); !errors.Is(err, definition.ErrInvalid) {
		t.Errorf("empty prompt: %v", err)
	}
}

func TestParseCompilesTheSpec(t *testing.T) {
	front := "description: Docs.\nmodel:\n  use: anthropic/claude\n  fallback: [openai/gpt]\n" +
		"tools:\n  registry: [search, github_comment]\n  mcp:\n    - server: linear\n" +
		"subagents:\n  - ref: writer\n    budget:\n      max_cost: 0.25\n" +
		"approval:\n  ask: [github_comment, tag_writer]\n" +
		"limits:\n  budget:\n    max_cost: 1.5\n    max_tokens: 1000\n"
	policy := definition.Policy{
		WorkspaceAreas: []string{"notes/"}, MCPWrite: []string{"linear"}, FallbackOn: []string{"auth"},
		Approvers: []string{"ada"}, Answerers: []string{"eve"}, TaggedOnly: true,
		Knowledge: []definition.Corpus{{Name: "docs", URI: "kb://docs"}},
	}
	a, err := definition.Parse(file(front, "Answer."), policy, definition.Checks{})
	if err != nil {
		t.Fatal(err)
	}
	s := a.Spec
	if s.Provider != "anthropic" || s.Model != "claude" || len(s.Fallbacks) != 1 || s.Fallbacks[0].Provider != "openai" {
		t.Errorf("model = %s/%s %v", s.Provider, s.Model, s.Fallbacks)
	}
	if got := strings.Join(s.Tools, ","); got != "github_comment,mcp:linear:write,search" {
		t.Errorf("tools = %s", got)
	}
	if strings.Join(s.Access.Spawn, ",") != "writer" || strings.Join(s.Access.WorkspaceAreas, ",") != "notes/" {
		t.Errorf("access = %+v", s.Access)
	}
	if strings.Join(s.Approvals, ",") != "github_comment,tag_writer" || s.Approvers[0] != "ada" || s.Answerers[0] != "eve" || !s.TaggedOnly {
		t.Errorf("answer rules = %+v", s)
	}
	if s.SystemPrompt != "Answer." || s.Description != "Docs." || len(s.Sources) != 1 || s.FallbackOn[0] != "auth" {
		t.Errorf("spec = %+v", s)
	}
	if a.Budget.MaxCostMicros != 1_500_000 || a.Budget.MaxTokens != 1000 || a.Subagents["writer"].MaxCostMicros != 250_000 {
		t.Errorf("budgets = %+v, %+v", a.Budget, a.Subagents)
	}
}

func TestMicrosIsExact(t *testing.T) {
	for usd, want := range map[float64]int64{0: 0, 1: 1_000_000, 0.1: 100_000, 0.000001: 1, 12.345678: 12_345_678, 1e9: 1e15} {
		got, err := definition.Micros(usd)
		if err != nil || got != want {
			t.Errorf("Micros(%v) = %d, %v; want %d", usd, got, err, want)
		}
	}
	for _, usd := range []float64{-1, math.NaN(), math.Inf(1), 0.0000001, 1e13} {
		if _, err := definition.Micros(usd); !errors.Is(err, definition.ErrInvalid) {
			t.Errorf("Micros(%v) = %v, want ErrInvalid", usd, err)
		}
	}
}
