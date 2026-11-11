package definition

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	sdef "github.com/urmzd/saige/agent/definition"
)

// AgentSpec is the JSON shape agents were stored in before saige
// definitions: one revision of one agent. It is read only, by FromAgentSpec.
type AgentSpec struct {
	Name         string           `json:"name"`
	Description  string           `json:"description,omitempty"`
	Model        string           `json:"model"`
	Provider     string           `json:"provider"`
	SystemPrompt string           `json:"system_prompt"`
	Tools        []string         `json:"tools,omitempty"`
	Sources      []Corpus         `json:"sources,omitempty"`
	Access       AgentSpecAccess  `json:"access,omitzero"`
	Fallbacks    []AgentSpecModel `json:"fallbacks,omitempty"`
	FallbackOn   []string         `json:"fallback_on,omitempty"`
	Approvals    []string         `json:"approvals,omitempty"`
	Approvers    []string         `json:"approvers,omitempty"`
	SelfApproval bool             `json:"self_approval,omitempty"`
	Answerers    []string         `json:"answerers,omitempty"`
	TaggedOnly   bool             `json:"tagged_only,omitempty"`
	Choices      *Choices         `json:"choices,omitempty"`
}

// AgentSpecAccess is the old access block.
type AgentSpecAccess struct {
	Spawn          []string `json:"spawn,omitempty"`
	WorkspaceAreas []string `json:"workspace_areas,omitempty"`
}

// AgentSpecModel is one old fallback model.
type AgentSpecModel struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// NameError lists the names in an old spec that break saige's name rule.
// An agent with one cannot be converted; the host disables and reports it.
type NameError struct {
	Agent string
	Names []string
}

func (e *NameError) Error() string {
	return fmt.Sprintf("agent %q: %s break the name rule (lowercase letters, digits and hyphens, starting with a letter, at most 48 characters)",
		e.Agent, strings.Join(quoted(e.Names), ", "))
}

// Is lets callers match ErrInvalid.
func (e *NameError) Is(target error) bool { return target == ErrInvalid }

// FromAgentSpec converts revision rev of an old spec. It is deterministic:
// the same spec and revision always produce the same content and policy.
//
//   - rev N becomes version N.0.0
//   - provider and model become model.use, fallbacks model.fallback
//   - access.spawn becomes subagents, approvals becomes approval.ask
//   - "mcp:<p>" and "mcp:<p>:write" become tools.mcp, the write bit
//     Policy.MCPWrite
//   - everything else goes to the Policy
//
// The result is a Record with no tenant, author or time.
func FromAgentSpec(spec AgentSpec, rev int) (Record, error) {
	if rev < 1 {
		return Record{}, fmt.Errorf("%w: revision %d; revisions count from 1", ErrInvalid, rev)
	}
	name := NormalizeName(spec.Name)
	var bad []string
	if _, err := sdef.ParseRef(name); err != nil || strings.Contains(name, "@") {
		bad = append(bad, name)
	}
	spawn := set(spec.Access.Spawn, true)
	for _, t := range spawn {
		if _, err := sdef.ParseRef(t); err != nil || strings.Contains(t, "@") {
			bad = append(bad, t)
		}
	}
	if len(bad) > 0 {
		return Record{}, &NameError{Agent: name, Names: bad}
	}

	var registry, mcp, write []string
	for _, t := range set(spec.Tools, false) {
		g, ok, err := ParseMCPGrant(t)
		switch {
		case err != nil:
			return Record{}, fmt.Errorf("agent %q: %w", name, err)
		case !ok:
			registry = append(registry, t)
		default:
			if !slices.Contains(mcp, g.Provider) {
				mcp = append(mcp, g.Provider)
			}
			if g.Write && !slices.Contains(write, g.Provider) {
				write = append(write, g.Provider)
			}
		}
	}

	var b strings.Builder
	b.WriteString("---\n")
	line(&b, 0, "apiVersion", sdef.APIVersion)
	line(&b, 0, "name", name)
	line(&b, 0, "version", fmt.Sprintf("%d.0.0", rev))
	if d := strings.TrimSpace(spec.Description); d != "" {
		line(&b, 0, "description", d)
	}
	b.WriteString("model:\n")
	line(&b, 1, "use", strings.ToLower(strings.TrimSpace(spec.Provider))+"/"+strings.TrimSpace(spec.Model))
	if len(spec.Fallbacks) > 0 {
		b.WriteString("  fallback:\n")
		for _, f := range spec.Fallbacks {
			item(&b, 2, strings.ToLower(strings.TrimSpace(f.Provider))+"/"+strings.TrimSpace(f.Model))
		}
	}
	if len(registry) > 0 || len(mcp) > 0 {
		b.WriteString("tools:\n")
		if len(registry) > 0 {
			b.WriteString("  registry:\n")
			for _, t := range registry {
				item(&b, 2, t)
			}
		}
		if len(mcp) > 0 {
			b.WriteString("  mcp:\n")
			for _, s := range mcp {
				b.WriteString("    - server: " + quote(s) + "\n")
			}
		}
	}
	if len(spawn) > 0 {
		b.WriteString("subagents:\n")
		for _, t := range spawn {
			b.WriteString("  - ref: " + quote(t) + "\n")
		}
	}
	if asks := set(spec.Approvals, false); len(asks) > 0 {
		b.WriteString("approval:\n  ask:\n")
		for _, t := range asks {
			item(&b, 2, t)
		}
	}
	b.WriteString("---\n")
	b.WriteString(strings.TrimSpace(spec.SystemPrompt))
	b.WriteString("\n")

	policy := Policy{
		WorkspaceAreas: spec.Access.WorkspaceAreas,
		MCPWrite:       write,
		FallbackOn:     spec.FallbackOn,
		Approvers:      spec.Approvers,
		SelfApproval:   spec.SelfApproval,
		Answerers:      spec.Answerers,
		TaggedOnly:     spec.TaggedOnly,
		Choices:        spec.Choices,
	}
	for _, s := range spec.Sources {
		policy.Knowledge = append(policy.Knowledge, Corpus{Name: s.Name, URI: s.URI, Options: maps.Clone(s.Options)})
	}
	policy = policy.Normalize()
	return Record{Name: name, Version: fmt.Sprintf("%d.0.0", rev), Content: b.String(), Policy: policy}, nil
}

// quote writes a YAML double-quoted scalar. A JSON string is one.
func quote(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

func line(b *strings.Builder, depth int, key, value string) {
	b.WriteString(strings.Repeat("  ", depth) + key + ": " + quote(value) + "\n")
}

func item(b *strings.Builder, depth int, value string) {
	b.WriteString(strings.Repeat("  ", depth) + "- " + quote(value) + "\n")
}

func quoted(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = fmt.Sprintf("%q", n)
	}
	return out
}
