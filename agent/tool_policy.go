package agent

import (
	"context"
	"fmt"

	"github.com/urmzd/saige/agent/types"
)

// ToolPolicy selects the tools visible and executable in one model turn.
// It runs after handoff resolution and before routing/provider execution.
// Implementations can disclose tools eagerly or expose a small set until a
// discovery tool updates their session state. They do not replace ToolGate.
type ToolPolicy interface {
	Select(context.Context, string, []types.ToolDef) ([]string, error)
}

type ToolPolicyFunc func(context.Context, string, []types.ToolDef) ([]string, error)

func (f ToolPolicyFunc) Select(ctx context.Context, name string, defs []types.ToolDef) ([]string, error) {
	return f(ctx, name, defs)
}

func WithToolPolicy(policy ToolPolicy) AgentOption {
	return func(cfg *AgentConfig) { cfg.ToolPolicy = policy }
}

func (a *Agent) selectTools(ctx context.Context, active activeContext) (activeContext, error) {
	if a.cfg.ToolPolicy == nil {
		return active, nil
	}
	names, err := a.cfg.ToolPolicy.Select(ctx, active.name, active.tools.Definitions())
	if err != nil {
		return active, err
	}
	selected := types.NewToolRegistry()
	seen := map[string]bool{}
	for _, name := range names {
		tool, ok := active.tools.Get(name)
		if !ok || seen[name] {
			return active, fmt.Errorf("tool policy selected invalid tool %q", name)
		}
		seen[name] = true
		selected.Register(tool)
	}
	active.tools, active.toolDefs = selected, selected.Definitions()
	return active, nil
}
