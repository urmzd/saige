// Package definition reads portable, versioned agent definitions: an agent
// written as a Markdown file whose YAML frontmatter declares what the agent
// is (model, tools, skills, memory, sub-agents, approvals, compaction,
// guardrails and limits) and whose body is its system prompt.
//
// The package keeps four concerns apart:
//
//   - what a definition is: Definition, Parse and the JSON Schema;
//   - where definitions come from: Source and its implementations (a
//     directory, an fs.FS, a reader, HTTP, and Layered over any of them;
//     Postgres lives in package pgsource);
//   - how a reference resolves: Registry, which picks one version for
//     name@range, checks every reference, and pins the result by digest;
//   - how a definition becomes a running agent: package bind.
//
// A definition is data. Loading one never connects to a server, reads a
// credential or runs a tool; only bind does, with the host's environment.
package definition

import (
	"fmt"
	"strings"

	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/types"
)

// APIVersion is the one frontmatter version this package reads. A file
// with any other apiVersion is rejected, so a newer format is never read
// as an older one.
const APIVersion = "saige/v1"

// Definition is one agent definition. The frontmatter fields carry json
// tags, which name them in the file, the JSON Schema and error paths. The
// fields after Metadata describe where the definition came from and are
// never read from the file.
type Definition struct {
	// APIVersion must be APIVersion.
	APIVersion string `json:"apiVersion"`
	// Name identifies the agent: lowercase letters, digits and hyphens,
	// starting with a letter. It is also the agent's name at run time, so
	// a sub-agent is delegated to as delegate_to_<name>.
	Name string `json:"name"`
	// Version is a semantic version, such as 1.4.0.
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	// Model selects the model: a catalog preset, or provider/model, with
	// an optional fallback chain. Omitted, the host's default applies.
	Model *ModelRef `json:"model,omitempty"`
	// Dials are model-neutral generation intents (see types.Dials).
	Dials *types.Dials `json:"dials,omitempty"`
	// Tools names the tool packs the agent may use.
	Tools *ToolsSpec `json:"tools,omitempty"`
	// Skills names skill packages and how each reaches the model.
	Skills []SkillRef `json:"skills,omitempty"`
	// Memory connects the agent to a durable memory store.
	Memory *MemorySpec `json:"memory,omitempty"`
	// Subagents reference other definitions by name and version range.
	Subagents []SubagentRef `json:"subagents,omitempty"`
	// Approval is the human-in-the-loop policy for tool calls.
	Approval *ApprovalSpec `json:"approval,omitempty"`
	// Compaction is the context compaction strategy, in the catalog's
	// form.
	Compaction *catalog.CompactionSpec `json:"compaction,omitempty"`
	// Guardrails check the user's input and the final answer.
	Guardrails *GuardrailsSpec `json:"guardrails,omitempty"`
	// Limits bound one run.
	Limits *LimitsSpec `json:"limits,omitempty"`
	// Metadata is free-form and never changes behavior.
	Metadata map[string]string `json:"metadata,omitempty"`

	// Prompt is the Markdown body: the agent's system prompt.
	Prompt string `json:"-"`
	// Digest is "sha256:<hex>" over the file's bytes, with line endings
	// normalized, so it names exactly one definition.
	Digest string `json:"-"`
	// Source names the source the definition was loaded from, and Path the
	// file within it, when there is one.
	Source string `json:"-"`
	Path   string `json:"-"`
	// Trusted is false for a definition from a source marked Untrusted.
	Trusted bool `json:"-"`

	raw []byte
}

// ID returns name@version.
func (d *Definition) ID() string { return d.Name + "@" + d.Version }

// Raw returns the file the definition was parsed from.
func (d *Definition) Raw() []byte { return append([]byte(nil), d.raw...) }

// Location names the definition in errors: its source and path.
func (d *Definition) Location() string {
	switch {
	case d.Path != "" && d.Source != "" && d.Source != d.Path:
		return d.Source + ":" + d.Path
	case d.Path != "":
		return d.Path
	}
	return d.Source
}

// shorthand is implemented by types that a scalar may stand for: a plain
// string in the file is read as an object with only that field set.
type shorthand interface{ shorthandField() string }

// ModelRef selects the model. In the file it is either a string, the Use
// field alone, or an object.
type ModelRef struct {
	// Use is a catalog preset name, or provider/model.
	Use string `json:"use"`
	// Fallback lists provider/model entries tried, in order, when Use
	// fails over.
	Fallback []string `json:"fallback,omitempty"`
}

func (ModelRef) shorthandField() string { return "use" }

// IsPreset reports whether Use names a preset rather than provider/model.
func (m ModelRef) IsPreset() bool { return !strings.Contains(m.Use, "/") }

// ToolsSpec names the tools the agent may use.
type ToolsSpec struct {
	// Harness lists built-in harness groups: read, write, exec and web.
	Harness []string `json:"harness,omitempty"`
	// MCP lists MCP servers by the name the host configures them under.
	MCP []MCPRef `json:"mcp,omitempty"`
	// Registry names tools the host registers, such as rag_search.
	Registry []string `json:"registry,omitempty"`
}

// MCPRef names one MCP server. In the file it is either the server name or
// an object.
type MCPRef struct {
	Server string `json:"server"`
	// Allow narrows the server's tools to these remote names. It can only
	// narrow what the host's configuration already imports.
	Allow []string `json:"allow,omitempty"`
}

func (MCPRef) shorthandField() string { return "server" }

// SkillMode says how a skill reaches the model.
type SkillMode string

// Skill modes.
const (
	// SkillLazy lists the skill in the system prompt; the model loads it
	// with load_skill when a task needs it. It is the default.
	SkillLazy SkillMode = "lazy"
	// SkillEager puts the skill's instructions in the system prompt.
	SkillEager SkillMode = "eager"
	// SkillPinned is SkillEager for one exact snapshot: the skill's hash
	// must equal Hash, or binding fails.
	SkillPinned SkillMode = "pinned"
	// SkillSearch leaves the skill out of the prompt; the model finds it
	// with search_skills.
	SkillSearch SkillMode = "search"
	// SkillTrigger adds the skill's instructions to a user message that
	// matches one of Triggers, and lists it like SkillLazy otherwise.
	SkillTrigger SkillMode = "trigger"
)

// SkillModes lists every mode, in documentation order.
func SkillModes() []SkillMode {
	return []SkillMode{SkillLazy, SkillEager, SkillPinned, SkillSearch, SkillTrigger}
}

// SkillRef names one skill package. In the file it is either the skill name
// or an object.
type SkillRef struct {
	Name string    `json:"name"`
	Mode SkillMode `json:"mode,omitempty"`
	// Triggers are regular expressions (Go syntax) for SkillTrigger.
	Triggers []string `json:"triggers,omitempty"`
	// Hash is the skill's snapshot hash, "sha256:<hex>", for SkillPinned.
	Hash string `json:"hash,omitempty"`
}

func (SkillRef) shorthandField() string { return "name" }

// EffectiveMode returns Mode, or SkillLazy when it is empty.
func (s SkillRef) EffectiveMode() SkillMode {
	if s.Mode == "" {
		return SkillLazy
	}
	return s.Mode
}

// Memory recall modes.
const (
	RecallTool   = "tool"
	RecallInject = "inject"
	RecallSelect = "select"
	RecallOff    = "off"
)

// MemorySpec connects the agent to a memory store the host provides.
type MemorySpec struct {
	// Store names the store in the host's environment.
	Store string `json:"store"`
	// Recall is tool (the default), inject, select or off.
	Recall string `json:"recall,omitempty"`
	// Write lists the kinds the model may write: semantic, episodic,
	// procedural. Empty admits every kind. Writes always ask first.
	Write []string `json:"write,omitempty"`
	// Budget is the token budget of injected memories.
	Budget int `json:"budget,omitempty"`
	// Retention sets an expiry on new records, such as "720h".
	Retention catalog.Duration `json:"retention,omitzero"`
	// Namespace narrows the host's scope to a sub-namespace.
	Namespace string `json:"namespace,omitempty"`
	// ReadOnly lets the agent recall but never write.
	ReadOnly bool `json:"read_only,omitempty"`
}

// Sub-agent modes.
const (
	SubagentDelegate = "delegate"
	SubagentSpawn    = "spawn"
	SubagentHandoff  = "handoff"
)

// SubagentRef references another definition. In the file it is either the
// reference or an object.
type SubagentRef struct {
	// Ref is name or name@range, such as reviewer@^1.2.
	Ref string `json:"ref"`
	// Mode is delegate (the default), spawn or handoff.
	Mode string `json:"mode,omitempty"`
	// Description replaces the sub-agent's own description in the tool the
	// parent sees.
	Description string          `json:"description,omitempty"`
	Budget      *SubagentBudget `json:"budget,omitempty"`
}

func (SubagentRef) shorthandField() string { return "ref" }

// EffectiveMode returns Mode, or SubagentDelegate when it is empty.
func (s SubagentRef) EffectiveMode() string {
	if s.Mode == "" {
		return SubagentDelegate
	}
	return s.Mode
}

// SubagentBudget bounds one delegation.
type SubagentBudget struct {
	MaxIterations int              `json:"max_iterations,omitempty"`
	Timeout       catalog.Duration `json:"timeout,omitzero"`
	// MaxCost, MaxTokens and MaxRequests give the sub-agent a budget of
	// its own, separate from the parent's.
	MaxCost     float64 `json:"max_cost,omitempty"`
	MaxTokens   int     `json:"max_tokens,omitempty"`
	MaxRequests int     `json:"max_requests,omitempty"`
}

// Approval decisions.
const (
	DecisionAllow = "allow"
	DecisionAsk   = "ask"
	DecisionDeny  = "deny"
)

// ApprovalSpec is the human-in-the-loop policy. Rules use the permission
// syntax of Claude Code: a tool name, or a tool name with a specifier in
// parentheses, such as Bash(git status:*). Deny wins over ask, and ask over
// allow; a call no rule names falls to the capability default.
type ApprovalSpec struct {
	// Capabilities maps a capability class (read, write, destructive,
	// unknown) to allow, ask or deny.
	Capabilities map[string]string `json:"capabilities,omitempty"`
	Allow        []string          `json:"allow,omitempty"`
	Ask          []string          `json:"ask,omitempty"`
	Deny         []string          `json:"deny,omitempty"`
	// Grant is the widest scope a person may grant with an approval: once,
	// args, tool or session. Empty allows every scope.
	Grant string `json:"grant,omitempty"`
	// DenyAfter refuses a tool without asking after this many denials.
	DenyAfter int `json:"deny_after,omitempty"`
	// RampAfter approves a write tool without asking after this many
	// approvals. Destructive tools always ask.
	RampAfter int `json:"ramp_after,omitempty"`
	// HideDenied hides a tool from the model once DenyAfter refuses it.
	HideDenied bool `json:"hide_denied,omitempty"`
}

// GuardrailsSpec lists the built-in guardrails on each edge.
type GuardrailsSpec struct {
	Input  []GuardrailSpec `json:"input,omitempty"`
	Output []GuardrailSpec `json:"output,omitempty"`
}

// Built-in guardrail names.
const (
	GuardrailPII        = "pii"
	GuardrailRegex      = "regex"
	GuardrailMaxLength  = "max_length"
	GuardrailClassifier = "classifier"
)

// GuardrailSpec configures one built-in guardrail. In the file it is either
// the name or an object.
type GuardrailSpec struct {
	// Name is pii, regex, max_length or classifier.
	Name string `json:"name"`
	// Redact rewrites matches instead of blocking (pii, regex).
	Redact bool `json:"redact,omitempty"`
	// Max is the character limit (max_length).
	Max int `json:"max,omitempty"`
	// Patterns are the expressions (regex).
	Patterns []PatternSpec `json:"patterns,omitempty"`
	// Policy is the plain-language policy (classifier). The classifier
	// runs on the agent's own model.
	Policy string `json:"policy,omitempty"`
	// Parallel races an input guardrail with the first model call.
	Parallel bool `json:"parallel,omitempty"`
}

func (GuardrailSpec) shorthandField() string { return "name" }

// PatternSpec is one labeled regular expression.
type PatternSpec struct {
	Label string `json:"label"`
	Expr  string `json:"expr"`
}

// LimitsSpec bounds one run.
type LimitsSpec struct {
	MaxIterations int              `json:"max_iterations,omitempty"`
	LLMTimeout    catalog.Duration `json:"llm_timeout,omitzero"`
	ToolTimeout   catalog.Duration `json:"tool_timeout,omitzero"`
	Budget        *BudgetSpec      `json:"budget,omitempty"`
}

// Budget actions.
const (
	OnExceedStop = "stop"
	OnExceedAsk  = "ask"
)

// BudgetSpec is the run's spending policy (see types.BudgetPolicy).
type BudgetSpec struct {
	// MaxCost is in the rate card's currency, such as 0.50.
	MaxCost     float64 `json:"max_cost,omitempty"`
	MaxTokens   int     `json:"max_tokens,omitempty"`
	MaxRequests int     `json:"max_requests,omitempty"`
	// WarnAt is the fraction of MaxCost that starts warnings.
	WarnAt float64 `json:"warn_at,omitempty"`
	// OnExceed is stop (the default) or ask.
	OnExceed      string `json:"on_exceed,omitempty"`
	AllowUnpriced bool   `json:"allow_unpriced,omitempty"`
}

// Ref is a parsed name@range reference.
type Ref struct {
	Name string
	// Range is the version constraint; empty means any version.
	Range string
}

func (r Ref) String() string {
	if r.Range == "" {
		return r.Name
	}
	return r.Name + "@" + r.Range
}

// ParseRef parses name or name@range and checks both parts.
func ParseRef(s string) (Ref, error) {
	name, rng, _ := strings.Cut(strings.TrimSpace(s), "@")
	if err := checkName(name); err != nil {
		return Ref{}, err
	}
	rng = strings.TrimSpace(rng)
	if _, err := ParseRange(rng); err != nil {
		return Ref{}, err
	}
	return Ref{Name: name, Range: rng}, nil
}

// maxNameLen keeps a delegate_to_<name> tool name within the 64 characters
// providers accept.
const maxNameLen = 48

func checkName(name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if len(name) > maxNameLen {
		return fmt.Errorf("name %q is longer than %d characters", name, maxNameLen)
	}
	for i, r := range name {
		ok := r >= 'a' && r <= 'z' || i > 0 && (r >= '0' && r <= '9' || r == '-')
		if !ok {
			return fmt.Errorf("name %q must be lowercase letters, digits and hyphens, starting with a letter", name)
		}
	}
	if strings.HasSuffix(name, "-") {
		return fmt.Errorf("name %q must not end with a hyphen", name)
	}
	return nil
}
