package definition

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/types"
)

const fullDefinition = `---
apiVersion: saige/v1
name: reviewer
version: 1.4.0
description: Reviews diffs.
model:
  use: balanced
  fallback: [openai/gpt-6-luna, anthropic/claude-haiku-5-5]
dials:
  creativity: focused
  reasoning:
    depth: low
tools:
  harness: [read, exec]
  mcp:
    - github
    - server: linear
      allow: [search_issues]
  registry: [rag_search]
skills:
  - go-style
  - name: release
    mode: trigger
    triggers: ["(?i)\\brelease\\b"]
  - name: pdf
    mode: search
  - name: house-rules
    mode: pinned
    hash: sha256:0000000000000000000000000000000000000000000000000000000000000000
memory:
  store: team
  recall: inject
  write: [semantic]
  budget: 400
  retention: 720h
  namespace: reviews
subagents:
  - researcher@^1.2
  - ref: summarizer@~2.0
    mode: spawn
    description: Summarize long files.
    budget:
      max_iterations: 4
      timeout: 2m
      max_cost: 0.1
approval:
  capabilities:
    write: ask
    destructive: deny
  allow: ["Bash(git status:*)", read_file]
  ask: [write_file]
  deny: ["Bash(rm:*)"]
  grant: tool
  deny_after: 3
  ramp_after: 2
compaction:
  strategy: chain
  chain:
    - strategy: clear_tool_results
      keep_tool_results: 3
    - strategy: summary
guardrails:
  input:
    - pii
    - name: max_length
      max: 8000
  output:
    - name: regex
      redact: true
      patterns:
        - label: TICKET
          expr: "TICKET-[0-9]+"
limits:
  max_iterations: 12
  llm_timeout: 60s
  tool_timeout: 30s
  budget:
    max_cost: 1.5
    max_tokens: 200000
    on_exceed: ask
metadata:
  team: platform
---

You review diffs.
`

func TestParseFullDefinition(t *testing.T) {
	d, err := Parse([]byte(fullDefinition), "reviewer.agent.md")
	if err != nil {
		t.Fatal(err)
	}
	if d.ID() != "reviewer@1.4.0" || d.Prompt != "You review diffs." || !strings.HasPrefix(d.Digest, "sha256:") {
		t.Fatalf("got %s %q %s", d.ID(), d.Prompt, d.Digest)
	}
	if d.Model.Use != "balanced" || len(d.Model.Fallback) != 2 || !d.Model.IsPreset() {
		t.Fatalf("model %+v", d.Model)
	}
	if *d.Dials.Creativity != types.CreativityFocused || d.Dials.Reasoning.Depth != types.DepthLow {
		t.Fatalf("dials %+v", d.Dials)
	}
	if d.Tools.MCP[0].Server != "github" || d.Tools.MCP[1].Allow[0] != "search_issues" {
		t.Fatalf("mcp %+v", d.Tools.MCP)
	}
	if d.Skills[0].Name != "go-style" || d.Skills[0].EffectiveMode() != SkillLazy || d.Skills[1].Triggers[0] != `(?i)\brelease\b` {
		t.Fatalf("skills %+v", d.Skills)
	}
	if d.Subagents[0].Ref != "researcher@^1.2" || d.Subagents[0].EffectiveMode() != SubagentDelegate ||
		d.Subagents[1].Budget.Timeout != catalog.Duration(2*time.Minute) {
		t.Fatalf("subagents %+v", d.Subagents)
	}
	if d.Memory.Retention != catalog.Duration(720*time.Hour) || d.Approval.Capabilities["destructive"] != "deny" {
		t.Fatalf("memory %+v approval %+v", d.Memory, d.Approval)
	}
	if len(d.Compaction.Chain) != 2 || d.Guardrails.Output[0].Patterns[0].Label != "TICKET" || d.Limits.Budget.MaxTokens != 200000 {
		t.Fatalf("compaction %+v guardrails %+v limits %+v", d.Compaction, d.Guardrails, d.Limits)
	}
	if d.Metadata["team"] != "platform" {
		t.Fatalf("metadata %v", d.Metadata)
	}
}

func TestDigestIgnoresLineEndings(t *testing.T) {
	a, err := Parse([]byte(minimal("x", "1.0.0")), "a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse([]byte("\ufeff"+strings.ReplaceAll(minimal("x", "1.0.0"), "\n", "\r\n")), "b")
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest != b.Digest {
		t.Fatalf("digests differ: %s %s", a.Digest, b.Digest)
	}
	c, _ := Parse([]byte(minimal("x", "1.0.0")+"more"), "c")
	if c.Digest == a.Digest {
		t.Fatal("a changed body kept the digest")
	}
}

func minimal(name, version string) string {
	return "---\napiVersion: saige/v1\nname: " + name + "\nversion: " + version + "\n---\nPrompt.\n"
}

func header(lines string) string {
	return "---\napiVersion: saige/v1\nname: x\nversion: 1.0.0\n" + lines + "---\nbody\n"
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name, file, code, path, msg string
		line                        int
	}{
		{"no frontmatter", "hello", CodeFrontmatter, "", "must start with", 1},
		{"no closing line", "---\nname: x\n", CodeFrontmatter, "", "no closing", 1},
		{"empty frontmatter", "---\n---\nbody", CodeMissing, "", "empty", 2},
		{"syntax", "---\nname: [x\n---\n", CodeSyntax, "", "line", 0},
		{"unknown key with suggestion", header("descriptio: x\n"), CodeUnknownKey, "descriptio", `did you mean "description"`, 5},
		{"nested unknown key", header("tools:\n  harnes: [read]\n"), CodeUnknownKey, "tools.harnes", `"harness"`, 6},
		{"duplicate key", header("name: y\n"), CodeDuplicateKey, "name", "duplicate", 5},
		{"wrong type", header("skills: lazy\n"), CodeWrongType, "skills", "want a list", 5},
		{"wrong scalar type", header("limits:\n  max_iterations: many\n"), CodeWrongType, "limits.max_iterations", "want an integer", 6},
		{"bool for string object", header("model: true\n"), "", "", "", 0}, // a scalar stands for model.use
		{"null", header("description:\n"), CodeNullNotAllowed, "description", "null", 5},
		{"anchor", header("metadata: &m\n  a: b\n"), CodeUnsupported, "metadata", "anchors", 5},
		{"alias", header("metadata: &m\n  a: b\nlimits: *m\n"), CodeUnsupported, "metadata", "anchors", 5},
		{"tag", header("description: !!str x\n"), CodeUnsupported, "description", "tags", 5},
		{"merge key", header("metadata:\n  <<: {a: b}\n"), CodeUnsupported, "metadata", "merge", 6},
		{"bad duration", header("limits:\n  llm_timeout: soon\n"), CodeBadValue, "limits.llm_timeout", "duration", 6},
		{"missing api version", "---\nname: x\nversion: 1.0.0\n---\n", CodeMissing, "apiVersion", "required", 2},
		{"unknown api version", "---\napiVersion: saige/v2\nname: x\nversion: 1.0.0\n---\n", CodeVersion, "apiVersion", "saige/v2", 2},
		{"bad name", "---\napiVersion: saige/v1\nname: Bad_Name\nversion: 1.0.0\n---\n", CodeBadValue, "name", "lowercase", 3},
		{"missing version", "---\napiVersion: saige/v1\nname: x\n---\n", CodeMissing, "version", "required", 0},
		{"bad version", "---\napiVersion: saige/v1\nname: x\nversion: 1.0\n---\n", CodeBadValue, "version", "major.minor.patch", 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.file), "x.agent.md")
			if tc.code == "" {
				if err != nil {
					t.Fatalf("unexpected error %v", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || !errors.Is(err, ErrInvalid) {
				t.Fatalf("want a ValidationError, got %v", err)
			}
			is := ve.Issues[0]
			if is.Code != tc.code || is.Path != tc.path || !strings.Contains(is.Message, tc.msg) {
				t.Fatalf("got %+v", ve.Issues)
			}
			if tc.line != 0 && is.Line != tc.line {
				t.Fatalf("line %d, want %d (%+v)", is.Line, tc.line, is)
			}
			if !strings.Contains(err.Error(), "x.agent.md") {
				t.Fatalf("error does not name the file: %v", err)
			}
		})
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct{ name, lines, path, msg string }{
		{"provider model", "model:\n  use: anthropic/\n", "model.use", "provider/model"},
		{"fallback", "model:\n  use: balanced\n  fallback: [gpt]\n", "model.fallback[0]", "provider/model"},
		{"harness group", "tools:\n  harness: [reed]\n", "tools.harness[0]", `did you mean "read"`},
		{"duplicate group", "tools:\n  harness: [read, read]\n", "tools.harness[1]", "twice"},
		{"skill mode", "skills:\n  - name: a\n    mode: lazzy\n", "skills[0].mode", `"lazy"`},
		{"trigger without patterns", "skills:\n  - name: a\n    mode: trigger\n", "skills[0].triggers", "at least one"},
		{"triggers on lazy", "skills:\n  - name: a\n    triggers: [x]\n", "skills[0].triggers", "only to mode trigger"},
		{"bad trigger", "skills:\n  - name: a\n    mode: trigger\n    triggers: [\"(\"]\n", "skills[0].triggers[0]", "invalid pattern"},
		{"pinned without hash", "skills:\n  - name: a\n    mode: pinned\n", "skills[0].hash", "hash"},
		{"bad hash", "skills:\n  - name: a\n    mode: pinned\n    hash: md5:x\n", "skills[0].hash", "sha256"},
		{"skill name", "skills:\n  - Bad\n", "skills[0].name", "lowercase"},
		{"memory store", "memory:\n  recall: tool\n", "memory.store", "store"},
		{"memory read only write", "memory:\n  store: s\n  read_only: true\n  write: [semantic]\n", "memory.write", "read_only"},
		{"memory kind", "memory:\n  store: s\n  write: [facts]\n", "memory.write[0]", "semantic"},
		{"self subagent", "subagents: [x]\n", "subagents[0].ref", "own sub-agent"},
		{"subagent ref", "subagents: [\"y@^nope\"]\n", "subagents[0].ref", "invalid"},
		{"subagent twice", "subagents: [y, y@1.0.0]\n", "subagents[1].ref", "twice"},
		{"subagent mode", "subagents:\n  - ref: y\n    mode: fork\n", "subagents[0].mode", "delegate"},
		{"handoff budget", "subagents:\n  - ref: y\n    mode: handoff\n    budget:\n      max_cost: 1\n", "subagents[0].budget", "handoff"},
		{"capability class", "approval:\n  capabilities:\n    reads: allow\n", "approval.capabilities.reads", `"read"`},
		{"capability decision", "approval:\n  capabilities:\n    read: maybe\n", "approval.capabilities.read", "allow"},
		{"rule syntax", "approval:\n  allow: [\"Bash(git status\"]\n", "approval.allow[0]", "parenthesis"},
		{"rule specifier", "approval:\n  deny: [\"kg_search(x)\"]\n", "approval.deny[0]", "specifier"},
		{"grant scope", "approval:\n  grant: forever\n", "approval.grant", "session"},
		{"hide denied", "approval:\n  hide_denied: true\n", "approval.hide_denied", "deny_after"},
		{"compaction strategy", "compaction:\n  strategy: shrink\n", "compaction.strategy", "summary"},
		{"chain needs steps", "compaction:\n  strategy: chain\n", "compaction.chain", "chain"},
		{"guardrail name", "guardrails:\n  input: [toxicity]\n", "guardrails.input[0].name", "pii"},
		{"guardrail field", "guardrails:\n  input:\n    - name: pii\n      max: 3\n", "guardrails.input[0].max", "does not apply"},
		{"max length", "guardrails:\n  input: [max_length]\n", "guardrails.input[0].max", "positive"},
		{"regex patterns", "guardrails:\n  output: [regex]\n", "guardrails.output[0].patterns", "at least one"},
		{"classifier policy", "guardrails:\n  output: [classifier]\n", "guardrails.output[0].policy", "policy"},
		{"parallel output", "guardrails:\n  output:\n    - name: pii\n      parallel: true\n", "guardrails.output[0].parallel", "input"},
		{"budget empty", "limits:\n  budget:\n    warn_at: 0.5\n", "limits.budget", "max_cost"},
		{"budget warn", "limits:\n  budget:\n    max_cost: 1\n    warn_at: 2\n", "limits.budget.warn_at", "between"},
		{"on exceed", "limits:\n  budget:\n    max_cost: 1\n    on_exceed: pause\n", "limits.budget.on_exceed", "stop"},
		{"dials", "dials:\n  creativity: wild\n", "dials", "creativity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(header(tc.lines)), "x.agent.md")
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("want a ValidationError, got %v", err)
			}
			for _, is := range ve.Issues {
				if is.Path == tc.path && strings.Contains(is.Message, tc.msg) {
					if is.Line == 0 {
						t.Fatalf("issue has no line: %+v", is)
					}
					return
				}
			}
			t.Fatalf("no issue at %s containing %q in %+v", tc.path, tc.msg, ve.Issues)
		})
	}
}
