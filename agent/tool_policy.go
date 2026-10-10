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

// ToolPolicyFunc adapts a function to a ToolPolicy.
type ToolPolicyFunc func(context.Context, string, []types.ToolDef) ([]string, error)

// Select calls f.
func (f ToolPolicyFunc) Select(ctx context.Context, name string, defs []types.ToolDef) ([]string, error) {
	return f(ctx, name, defs)
}

// WithToolPolicy sets the policy that picks which tools each turn offers.
func WithToolPolicy(policy ToolPolicy) Option {
	return func(cfg *Config) { cfg.ToolPolicy = policy }
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
