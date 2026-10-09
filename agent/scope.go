package agent

import (
	"context"

	"github.com/urmzd/saige/agent/types"
)

// RunScope names the owner and conversation a tool policy or tool call
// belongs to. The agent loop attaches it to the context it passes to
// ToolPolicy.Select and to every tool call, so stateful policies (deferred
// tool discovery, an active skill) can keep separate state per owner and
// conversation without a new parameter on every interface.
type RunScope struct {
	// Agent is the name of the agent that owns the turn. In a handoff group
	// it is the active member, not the entry agent.
	Agent string
	// Conversation identifies the conversation tree. The agent loop sets it
	// to the tree's root node ID, which is random per tree, so two
	// conversations of agents with the same name, or two delegations to the
	// same sub-agent, never share policy state.
	Conversation types.NodeID
	// Branch is the branch the run started on. It stays fixed for the whole
	// run, including after compaction moves the run to a new branch, so state
	// a policy keyed on it survives compaction.
	Branch types.BranchID
}

// Key is a stable string form of the scope, suitable as a map key.
func (s RunScope) Key() string {
	return s.Agent + "\x00" + string(s.Conversation) + "\x00" + string(s.Branch)
}

type runScopeKey struct{}

// WithRunScope returns ctx carrying scope. The agent loop sets it; callers
// set it themselves only when they drive a policy or tool outside a run,
// for example in a test.
func WithRunScope(ctx context.Context, scope RunScope) context.Context {
	return context.WithValue(ctx, runScopeKey{}, scope)
}

// RunScopeFromContext returns the scope the agent loop attached to ctx.
func RunScopeFromContext(ctx context.Context) (RunScope, bool) {
	scope, ok := ctx.Value(runScopeKey{}).(RunScope)
	return scope, ok
}
