package bind

import (
	"context"
	"fmt"
	"slices"

	"github.com/urmzd/saige/agent/definition"
	"github.com/urmzd/saige/agent/mcp"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/tools"
)

// bindTools adds the definition's registry tools, harness groups and MCP
// servers to p. Each agent gets its own harness toolset and its own MCP
// pool, closed with the Bound.
func bindTools(ctx context.Context, env *Env, d *definition.Definition, p *parts, unmark func(types.Tool) types.Tool, b *Bound, mcpGate *types.ToolGate) error {
	spec := d.Tools
	for _, name := range spec.Registry {
		if env.Tools == nil {
			return fmt.Errorf("%w: agent %s names tool %q, but the host provides no registry tools", ErrUnsupported, d.Name, name)
		}
		t, ok := env.Tools.Get(name)
		if !ok {
			return fmt.Errorf("%w: agent %s names tool %q, which the host does not provide (available: %v)", ErrUnsupported, d.Name, name, toolNames(env.Tools))
		}
		p.tools = append(p.tools, unmark(t))
	}

	if len(spec.Harness) > 0 {
		opts := env.Harness
		if opts.Root == "" {
			return fmt.Errorf("%w: agent %s uses harness tools, but the host gives them no workspace root", ErrUnsupported, d.Name)
		}
		opts.Groups = nil
		for _, g := range spec.Harness {
			opts.Groups = append(opts.Groups, tools.Group(g))
		}
		set, err := tools.Harness(ctx, opts)
		if err != nil {
			return fmt.Errorf("agent %s: harness: %w", d.Name, err)
		}
		for _, t := range set.Tools {
			p.tools = append(p.tools, unmark(t))
		}
		p.workspace = set.Workspace
	}

	if len(spec.MCP) > 0 {
		pool := mcp.NewPool()
		b.closers = append(b.closers, pool.Close)
		for _, ref := range spec.MCP {
			server, ok := env.MCPServers[ref.Server]
			if !ok {
				return fmt.Errorf("%w: agent %s names MCP server %q, which the host does not configure", ErrUnsupported, d.Name, ref.Server)
			}
			if len(ref.Allow) > 0 {
				if len(server.AllowedTools) > 0 {
					for _, name := range ref.Allow {
						if !slices.Contains(server.AllowedTools, name) {
							return fmt.Errorf("%w: agent %s allows %s from MCP server %s, which the host's configuration does not import",
								ErrUnsupported, d.Name, name, ref.Server)
						}
					}
				}
				server.AllowedTools = slices.Clone(ref.Allow)
			}
			if _, err := pool.Add(ctx, server); err != nil {
				return fmt.Errorf("agent %s: MCP server %s: %w", d.Name, ref.Server, err)
			}
		}
		reg := types.NewToolRegistry()
		if _, err := pool.RegisterAll(ctx, reg); err != nil {
			return fmt.Errorf("agent %s: MCP tools: %w", d.Name, err)
		}
		for _, t := range reg.All() {
			p.tools = append(p.tools, unmark(t))
		}
		*mcpGate = pool.Gate()
	}
	return nil
}

func toolNames(r *types.ToolRegistry) []string {
	var out []string
	for _, d := range r.Definitions() {
		out = append(out, d.Name)
	}
	slices.Sort(out)
	return out
}
