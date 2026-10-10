package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	agentsdk "github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/definition"
	"github.com/urmzd/saige/agent/definition/bind"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/preset"
	"github.com/urmzd/saige/agent/skills"
	agenttypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/cmd/internal/agenthost"
	"github.com/urmzd/saige/tools"
)

// definitionFlags are what --agent needs to run a definition.
type definitionFlags struct {
	agentFlags
	// dir holds the definitions; root confines their harness tools.
	dir, root string
	// set names the flags given on the command line, so a definition's
	// own values apply unless one overrides them.
	set map[string]bool
}

// newDefinitionTool publishes the definition --agent names as the agent
// tool. ok is false when no definition in the directory has that name, so
// --agent falls back to a preset or provider/model. The definition's tools
// come from its own declaration: harness groups under --root, and registry
// tools from the packs this server exposes.
func newDefinitionTool(ctx context.Context, f definitionFlags, packs *agenttypes.ToolRegistry) (at agentTool, ok bool, err error) {
	if f.set["agent-system"] {
		return agentTool{}, false, errors.New("--agent-system cannot be combined with an agent definition, which declares its own system prompt")
	}
	cat := catalog.Default()
	if f.catalog != "" {
		if cat, err = catalog.LoadFile(f.catalog); err != nil {
			return agentTool{}, false, err
		}
	}
	home, _ := os.UserHomeDir()
	sk, err := skills.NewCatalog(ctx, skills.StandardSources(f.root, home))
	if err != nil {
		return agentTool{}, false, err
	}
	reg := definition.NewRegistry(definition.DirSource(f.dir), definition.Checks{Model: bind.ModelCheck(cat), Skill: bind.SkillCheck(sk)})
	if _, err := reg.Load(ctx); err != nil {
		return agentTool{}, false, fmt.Errorf("--agents-dir %s: %w", f.dir, err)
	}
	res, err := reg.Resolve(f.ref)
	if errors.Is(err, definition.ErrNotFound) {
		return agentTool{}, false, nil
	}
	if err != nil {
		return agentTool{}, false, fmt.Errorf("--agent %s: %w", f.ref, err)
	}
	env := bind.Env{Catalog: cat, PresetOptions: preset.Options{}, Harness: tools.HarnessOptions{Root: f.root}, Skills: sk}
	if len(packs.Definitions()) > 0 {
		env.Tools = packs
	}
	var opts []agentsdk.Option
	if f.set["agent-max-iter"] {
		opts = append(opts, agentsdk.WithMaxIter(f.maxIter))
	}
	var schema *agenttypes.ParameterSchema
	if f.schemaFile != "" {
		if schema, err = loadSchema(f.schemaFile); err != nil {
			return agentTool{}, false, err
		}
		opts = append(opts, agentsdk.WithResponseSchema(schema))
	}
	// Bind once now, so a definition this server cannot run fails at start.
	first, err := bind.Bind(ctx, res, env)
	if err != nil {
		return agentTool{}, false, fmt.Errorf("--agent %s: %w", f.ref, err)
	}
	gated := boundGated(first)
	_ = first.Close(ctx)

	at = agentTool{name: f.name, description: f.description, schema: schema, timeout: f.timeout, gated: gated,
		// Each call binds the resolution pinned at start, so calls share no
		// tools, scratch workspace or budget.
		newSession: func(ctx context.Context) (agenthost.Agent, error) {
			b, err := bind.Bind(ctx, res, env)
			if err != nil {
				return agenthost.Agent{}, err
			}
			return agenthost.FromBound(b, opts...)
		},
	}
	if !f.set["agent-tool"] {
		at.name = res.Name
	}
	if !f.set["agent-description"] {
		at.description = res.Description
		if at.description == "" {
			at.description = "Hand a task to the " + res.Name + " agent, which works on it with its own tools and returns the final answer."
		}
	}
	if _, clash := packs.Get(at.name); clash {
		return agentTool{}, false, fmt.Errorf("agent tool %q collides with a pack tool; set --agent-tool", at.name)
	}
	return at, true, nil
}

// boundGated reports whether a bound agent may stop for an approval: a tool
// with a marker, an approval gate, or sub-agents whose calls may ask.
func boundGated(b *bind.Bound) bool {
	if _, open := b.Config.ToolGate.(agenttypes.AllowAllGate); !open || len(b.Config.SubAgents) > 0 {
		return true
	}
	return b.Config.Tools != nil && anyGated(b.Config.Tools)
}
