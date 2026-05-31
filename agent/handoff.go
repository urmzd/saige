package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/urmzd/saige/agent/types"
)

// Handoff errors.
var (
	// ErrHandoffLimitExceeded is raised when the number of control transfers in
	// a single run exceeds AgentConfig.MaxHandoffs (ping-pong guard).
	ErrHandoffLimitExceeded = errors.New("handoff limit exceeded")
	// ErrUnknownHandoffTarget is raised when a handoff names an agent that is
	// not a member of the group.
	ErrUnknownHandoffTarget = errors.New("unknown handoff target")
)

// HandoffDef defines one agent that participates in a handoff group. Unlike
// SubAgentDef (a fresh, stateless child per call), a handoff agent shares the
// entry agent's tree and continues the same conversation via a stable root.
type HandoffDef struct {
	Name         string
	Description  string // shown to the LLM in the handoff_to_<name> tool
	SystemPrompt string // this agent's persona overlay (NOT the stable root)
	Provider     types.Provider
	Tools        *types.ToolRegistry
	MaxIter      int // 0 = inherit entry agent's MaxIter

	// CanHandOffTo restricts which agents this one may transfer to. Empty means
	// it may hand off to anyone in the group (including back to the entry agent).
	CanHandOffTo []string
}

// WithHandoffs registers a handoff group. The agent NewAgent is called on
// becomes the entry agent. Additive: composes with WithSubAgents.
func WithHandoffs(defs ...HandoffDef) AgentOption {
	return func(c *AgentConfig) { c.Handoffs = append(c.Handoffs, defs...) }
}

// WithMaxHandoffs overrides the maximum number of control transfers per run.
func WithMaxHandoffs(n int) AgentOption {
	return func(c *AgentConfig) { c.MaxHandoffs = n }
}

// HandoffSignaler is implemented by the handoff_to_<name> tool. The agent loop
// checks for this interface (mirroring SubAgentInvoker) to detect a transfer
// instead of treating the tool's output as a normal result.
type HandoffSignaler interface {
	HandoffTarget() string
}

// handoffTool is the sentinel tool registered as handoff_to_<target>.
type handoffTool struct {
	def    types.ToolDef
	target string
}

func (t *handoffTool) Definition() types.ToolDef { return t.def }
func (t *handoffTool) HandoffTarget() string     { return t.target }

// Execute is the fallback when a consumer ignores the signaler interface: it
// returns a short confirmation so the transcript stays coherent.
func (t *handoffTool) Execute(_ context.Context, _ map[string]any) (string, error) {
	return "Transferring control to " + t.target + ".", nil
}

// registerHandoff registers a handoff_to_<target> tool on the given registry.
func registerHandoff(registry *types.ToolRegistry, target, description string) {
	if description == "" {
		description = "Transfer control of the conversation to the " + target + " agent."
	}
	registry.Register(&handoffTool{
		target: target,
		def: types.ToolDef{
			Name:        "handoff_to_" + target,
			Description: description,
			Parameters: types.ParameterSchema{
				Type: "object",
				Properties: map[string]types.PropertyDef{
					"reason": {Type: "string", Description: "Why control is being transferred (optional)."},
				},
			},
		},
	})
}

// handoffGroup is the resolved, immutable set of agents that share one tree.
type handoffGroup struct {
	entry   string
	members map[string]*handoffMember
}

// handoffMember holds the swappable per-agent triple selected at flatten time.
type handoffMember struct {
	name         string
	systemPrompt string // "" for the entry agent => use the root system text
	provider     types.Provider
	tools        *types.ToolRegistry // includes every handoff_to_* tool it may use
	maxIter      int
}

// buildHandoffGroup validates the defs and wires each member's handoff tools.
func buildHandoffGroup(entry *handoffMember, defs []HandoffDef) (*handoffGroup, error) {
	names := map[string]bool{entry.name: true}
	for _, d := range defs {
		if d.Name == "" {
			return nil, fmt.Errorf("handoff def has empty name")
		}
		if names[d.Name] {
			return nil, fmt.Errorf("duplicate handoff agent name: %s", d.Name)
		}
		if d.Provider == nil {
			return nil, fmt.Errorf("handoff agent %q has no provider", d.Name)
		}
		names[d.Name] = true
	}

	members := map[string]*handoffMember{entry.name: entry}
	descByName := map[string]string{}
	for _, d := range defs {
		tools := d.Tools
		if tools == nil {
			tools = types.NewToolRegistry()
		}
		members[d.Name] = &handoffMember{
			name:         d.Name,
			systemPrompt: d.SystemPrompt,
			provider:     d.Provider,
			tools:        tools,
			maxIter:      d.MaxIter,
		}
		descByName[d.Name] = d.Description
	}

	allNames := make([]string, 0, len(members))
	for n := range members {
		allNames = append(allNames, n)
	}

	register := func(m *handoffMember, allowed []string) error {
		for _, target := range allowed {
			if target == m.name {
				continue
			}
			if !names[target] {
				return fmt.Errorf("%w: agent %q cannot hand off to %q", ErrUnknownHandoffTarget, m.name, target)
			}
			registerHandoff(m.tools, target, descByName[target])
		}
		return nil
	}

	// Entry agent may hand off to every defined agent.
	entryAllowed := make([]string, 0, len(defs))
	for _, d := range defs {
		entryAllowed = append(entryAllowed, d.Name)
	}
	if err := register(entry, entryAllowed); err != nil {
		return nil, err
	}

	for _, d := range defs {
		m := members[d.Name]
		allowed := d.CanHandOffTo
		if len(allowed) == 0 {
			for _, n := range allNames {
				if n != d.Name {
					allowed = append(allowed, n)
				}
			}
		}
		if err := register(m, allowed); err != nil {
			return nil, err
		}
	}

	return &handoffGroup{entry: entry.name, members: members}, nil
}

// activeMember resolves the member active for the given handoff target name.
// Returns nil when the agent has no handoff group (callers fall back to the
// entry agent's own cfg). An empty or unknown name resolves to the entry agent.
func (a *Agent) activeMember(name string) *handoffMember {
	if a.handoffs == nil {
		return nil
	}
	if name == "" {
		return a.handoffs.members[a.handoffs.entry]
	}
	if m, ok := a.handoffs.members[name]; ok {
		return m
	}
	return a.handoffs.members[a.handoffs.entry]
}

// overlaySystem merges an agent persona into the leading system message as a
// second text block on a copy of the message slice. The immutable root is never
// mutated. If there is no leading system message, one is prepended.
func overlaySystem(messages []types.Message, persona string) []types.Message {
	out := make([]types.Message, len(messages))
	copy(out, messages)
	if len(out) > 0 {
		if sm, ok := out[0].(types.SystemMessage); ok {
			content := make([]types.SystemContent, len(sm.Content), len(sm.Content)+1)
			copy(content, sm.Content)
			content = append(content, types.TextContent{Text: persona})
			out[0] = types.SystemMessage{Content: content}
			return out
		}
	}
	return append([]types.Message{types.NewSystemMessage(persona)}, out...)
}
