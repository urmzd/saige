package registry

import (
	"fmt"

	"github.com/urmzd/saige/agent/types"
)

// ToolSet is a revisioned registry of tools.
//
// types.ToolRegistry is the hot path: the agent loop looks up a tool by name on
// every call and wants a plain map. ToolSet sits above it and answers the
// questions the hot path cannot: which version of this tool is running, what
// did it look like before someone changed its schema, and can we go back.
//
// The two are connected by Snapshot, which materialises the currently-resolving
// revision of every tool into a plain ToolRegistry to hand to an agent. Take a
// snapshot when building the agent; changing the ToolSet afterwards does not
// affect a running agent, which is the correct behaviour: a tool set that
// mutated mid-run would change what the model was told it could call after it
// had already been told.
type ToolSet struct {
	reg *Registry[types.Tool]
}

// NewToolSet returns an empty tool set.
func NewToolSet() *ToolSet { return &ToolSet{reg: New[types.Tool]()} }

// Register adds a revision of a tool, keyed by its declared name. Registering a
// tool whose Definition().Name differs from a previous revision's is rejected:
// the name is the identity the model calls, so silently changing it would
// orphan the history rather than extend it.
func (s *ToolSet) Register(tool types.Tool, opts ...Option) (Entry[types.Tool], error) {
	name := tool.Definition().Name
	if name == "" {
		return Entry[types.Tool]{}, fmt.Errorf("registry: tool has no name")
	}
	return s.reg.Register(name, tool, opts...), nil
}

// MustRegister is Register for package init, where a failure is a programming
// error rather than a runtime condition.
func (s *ToolSet) MustRegister(tool types.Tool, opts ...Option) Entry[types.Tool] {
	e, err := s.Register(tool, opts...)
	if err != nil {
		panic(err)
	}
	return e
}

// Get resolves a tool by name, honouring any pin.
func (s *ToolSet) Get(name string) (types.Tool, bool) { return s.reg.Get(name) }

// At returns a specific revision of a tool, which is what a reproduction needs:
// running today's agent against last week's tool schema.
func (s *ToolSet) At(name string, rev Revision) (types.Tool, bool) {
	e, ok := s.reg.At(name, rev)
	return e.Value, ok
}

// History, Pin, Unpin, Pinned, Rollback and Names delegate to the underlying
// registry, so a tool set is operated with the same verbs as a model registry.
func (s *ToolSet) History(name string) []Entry[types.Tool] { return s.reg.History(name) }
func (s *ToolSet) Pin(name string, rev Revision) error     { return s.reg.Pin(name, rev) }
func (s *ToolSet) Unpin(name string)                       { s.reg.Unpin(name) }
func (s *ToolSet) Pinned(name string) (Revision, bool)     { return s.reg.Pinned(name) }
func (s *ToolSet) Names() []string                         { return s.reg.Names() }
func (s *ToolSet) Len() int                                { return s.reg.Len() }

// Rollback pins a tool to its previous revision and returns it.
func (s *ToolSet) Rollback(name string) (types.Tool, error) {
	e, err := s.reg.Rollback(name)
	return e.Value, err
}

// Snapshot materialises the currently-resolving revision of every tool into a
// plain registry for an agent to use. The returned registry is independent:
// later registrations and pins do not reach a running agent.
func (s *ToolSet) Snapshot() *types.ToolRegistry {
	out := types.NewToolRegistry()
	for _, e := range s.reg.All() {
		out.Register(e.Value)
	}
	return out
}

// SnapshotFiltered is Snapshot restricted to the named tools, for building a
// sub-agent or handoff member that should see only part of the set. A name with
// no registered tool is an error rather than a silent omission: a sub-agent
// missing the one tool it exists to call fails in a much more confusing way
// later.
func (s *ToolSet) SnapshotFiltered(names ...string) (*types.ToolRegistry, error) {
	out := types.NewToolRegistry()
	for _, n := range names {
		t, ok := s.Get(n)
		if !ok {
			return nil, fmt.Errorf("registry: no tool named %q", n)
		}
		out.Register(t)
	}
	return out, nil
}
