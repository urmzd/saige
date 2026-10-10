// Package bind turns a resolved agent definition into a runnable agent:
// its model into a preset bundle, its tool packs into tools, its skills,
// memory, sub-agents, approval rules, compaction, guardrails and limits into
// the agent options that already implement them.
//
// Everything a definition names by reference (a preset, an MCP server, a
// registry tool, a skill, a memory store) comes from the host's Env, so the
// same definition binds differently in the CLI, an MCP server or a test,
// and never reaches a server or credential the host did not provide.
package bind

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/definition"
	"github.com/urmzd/saige/agent/mcp"
	"github.com/urmzd/saige/agent/memory"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/preset"
	"github.com/urmzd/saige/agent/skills"
	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/agent/workspace"
	"github.com/urmzd/saige/tools"
)

// ErrUnsupported reports a definition the host's environment cannot bind:
// a reference it does not provide, or a combination binding cannot honor.
var ErrUnsupported = errors.New("agent definition: unsupported")

// Env is what the host provides to bind definitions.
type Env struct {
	// Catalog resolves model references. Nil uses catalog.Active().
	Catalog *catalog.Catalog
	// PresetOptions build each model's bundle (adapter factory, probes).
	PresetOptions preset.Options
	// Preset, when set, serves the root agent instead of its definition's
	// model, for a host flag that overrides the model. It is also the
	// model of a root definition that names none.
	Preset types.Preset
	// Harness configures the built-in tools; its Groups come from each
	// definition. Root is required by any definition that names a harness
	// group.
	Harness tools.HarnessOptions
	// MCPServers are the servers a definition may name, by name.
	MCPServers map[string]mcp.ServerSpec
	// Tools holds the tools a definition may name under tools.registry.
	Tools *types.ToolRegistry
	// Skills is the catalog skill references resolve against.
	Skills skills.SkillCatalog
	// MemoryStores are the stores a definition may name, and MemoryScope
	// maps an agent to its scope. Both are needed by a definition with a
	// memory block.
	MemoryStores map[string]memory.Store
	MemoryScope  func(ctx context.Context, owner string) (memory.Scope, error)
	// ToolGate is the host's own gate. It applies to every agent of the
	// tree, before each agent's approval rules.
	ToolGate types.ToolGate
}

func (e *Env) catalog() *catalog.Catalog {
	if e.Catalog != nil {
		return e.Catalog
	}
	return catalog.Active()
}

// Pin identifies the definition a bound agent runs: its name, version and
// resolved digest, which covers every sub-agent. Record it with a run's
// provenance; Registry.Pinned finds the same resolution again.
type Pin struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

func (p Pin) String() string { return p.Name + "@" + p.Version + " " + p.Digest }

// Bound is a definition bound to an environment. Config and Options build
// the root agent; NewAgent applies them. Close releases what binding
// opened, such as MCP connections.
type Bound struct {
	Resolved *definition.Resolved
	Config   agent.AgentConfig
	Options  []agent.AgentOption
	// MaxGrant is the widest grant scope an approval may carry, from the
	// definition's approval.grant. Empty allows every scope. Hosts that
	// accept grants check them with CheckGrant.
	MaxGrant types.GrantScope

	closers []func(context.Context) error
}

// NewAgent builds the root agent. extra options apply after the binding's.
// Each agent gets a budget of its own, with the definition's policy, so two
// agents built from one Bound never share an allowance.
func (b *Bound) NewAgent(extra ...agent.AgentOption) *agent.Agent {
	cfg := b.Config
	if cfg.Budget != nil {
		cfg.Budget = types.NewBudget(cfg.Budget.Policy())
	}
	return agent.NewAgent(cfg, append(slices.Clone(b.Options), extra...)...)
}

// Pin returns the bound definition's pin.
func (b *Bound) Pin() Pin {
	return Pin{Name: b.Resolved.Name, Version: b.Resolved.Version, Digest: b.Resolved.Digest}
}

// CheckGrant rejects a grant wider than the definition allows.
func (b *Bound) CheckGrant(g *types.GrantRequest) error {
	if g == nil || b.MaxGrant == "" {
		return nil
	}
	if definition.GrantRank(g.Scope) > definition.GrantRank(b.MaxGrant) {
		return fmt.Errorf("%w: agent %s allows grants up to %q, not %q", types.ErrInvalidGrant, b.Resolved.Name, b.MaxGrant, g.Scope)
	}
	return nil
}

// Close releases every resource binding opened.
func (b *Bound) Close(ctx context.Context) error {
	var errs []error
	for i := len(b.closers) - 1; i >= 0; i-- {
		errs = append(errs, b.closers[i](ctx))
	}
	b.closers = nil
	return errors.Join(errs...)
}

// Bind binds res and every sub-agent it resolved to. On error nothing stays
// open.
func Bind(ctx context.Context, res *definition.Resolved, env Env) (*Bound, error) {
	if res == nil || res.Definition == nil {
		return nil, errors.New("agent definition: nothing to bind")
	}
	b := &Bound{Resolved: res}
	if res.Approval != nil && res.Approval.Grant != "" {
		b.MaxGrant = types.GrantScope(res.Approval.Grant)
	}
	p, err := bindParts(ctx, &env, res, true, b)
	if err != nil {
		_ = b.Close(ctx)
		return nil, err
	}
	b.Config = agent.AgentConfig{
		Name:             res.Name,
		SystemPrompt:     p.prompt,
		Tools:            p.registry(),
		CompactCfg:       p.compact,
		MaxIter:          p.maxIter,
		SubAgents:        p.subagents,
		ToolGate:         gates(env.ToolGate, p.gate),
		ApprovalPolicy:   p.approval,
		Budget:           p.budget,
		LLMTimeout:       p.llmTimeout,
		ToolTimeout:      p.toolTimeout,
		Hooks:            p.hooks,
		InputGuardrails:  p.input,
		OutputGuardrails: p.output,
	}
	if b.Config.ToolGate == nil {
		b.Config.ToolGate = types.AllowAllGate{}
	}
	if p.dials != nil {
		b.Config.Dials = p.dials.Clone()
	}
	if p.preset != nil {
		b.Options = append(b.Options, agent.WithPreset(p.preset))
	}
	if p.workspace != nil {
		b.Config.Workspace = p.workspace
	}
	if p.toolPolicy != nil {
		b.Config.ToolPolicy = p.toolPolicy(nil)
	}
	if len(p.handoffs) > 0 {
		b.Options = append(b.Options, agent.WithHandoffs(p.handoffs...))
	}
	return b, nil
}

// parts is one bound definition, before it becomes a root AgentConfig, a
// SubAgentDef or a HandoffDef.
type parts struct {
	def         *definition.Definition
	prompt      string
	preset      types.Preset // nil inherits
	tools       []types.Tool
	gate        types.ToolGate // the definition's own, nil without an approval block
	approval    *agent.ApprovalPolicy
	toolPolicy  func(inner agent.ToolPolicy) agent.ToolPolicy
	hooks       []agent.Hooks
	input       []agent.InputGuardrail
	output      []agent.OutputGuardrail
	compact     *types.CompactConfig
	dials       *types.Dials
	maxIter     int
	llmTimeout  time.Duration
	toolTimeout time.Duration
	budget      *types.Budget
	subagents   []agent.SubAgentDef
	handoffs    []agent.HandoffDef
	workspace   workspace.Workspace
}

func (p *parts) registry() *types.ToolRegistry {
	if len(p.tools) == 0 {
		return nil
	}
	return types.NewToolRegistry(p.tools...)
}

// bindParts binds one definition. root is true for the agent the host
// runs, whose model the host may override.
func bindParts(ctx context.Context, env *Env, res *definition.Resolved, root bool, b *Bound) (*parts, error) {
	d := res.Definition
	p := &parts{def: d, prompt: d.Prompt}
	var err error

	// Model.
	switch {
	case root && env.Preset != nil:
		p.preset = env.Preset
	case d.Model != nil:
		if p.preset, err = buildModel(ctx, env, d.Name, d.Model); err != nil {
			return nil, err
		}
		prov := p.preset.Provider()
		b.closers = append(b.closers, func(ctx context.Context) error { return types.CloseProvider(ctx, prov) })
	case root:
		return nil, fmt.Errorf("%w: agent %s names no model and the host provides none", ErrUnsupported, d.Name)
	}
	if d.Dials != nil {
		dials := d.Dials.Clone()
		p.dials = &dials
	}

	// Approval comes first: tools bound below give up their markers to it.
	var gate *approvalGate
	if d.Approval != nil {
		if gate, err = newApprovalGate(d.Approval); err != nil {
			return nil, fmt.Errorf("agent %s: approval: %w", d.Name, err)
		}
		p.approval = approvalPolicy(d.Approval)
	}
	unmark := func(t types.Tool) types.Tool {
		if gate == nil {
			return t
		}
		return gate.unmark(t)
	}

	var mcpGate types.ToolGate
	if d.Tools != nil {
		if err := bindTools(ctx, env, d, p, unmark, b, &mcpGate); err != nil {
			return nil, err
		}
	}
	if len(d.Skills) > 0 {
		if err := bindSkills(ctx, env, d, p); err != nil {
			return nil, err
		}
	}
	if d.Memory != nil {
		if err := bindMemory(env, d, p, unmark); err != nil {
			return nil, err
		}
	}
	if d.Compaction != nil {
		cc := d.Compaction.Config()
		p.compact = &cc
	}
	if d.Guardrails != nil {
		if err := bindGuardrails(env, d, p, root); err != nil {
			return nil, err
		}
	}
	if l := d.Limits; l != nil {
		p.maxIter = l.MaxIterations
		p.llmTimeout = time.Duration(l.LLMTimeout)
		p.toolTimeout = time.Duration(l.ToolTimeout)
		if l.Budget != nil {
			p.budget = newBudget(l.Budget.MaxCost, l.Budget.MaxTokens, l.Budget.MaxRequests, l.Budget)
		}
	}

	for _, sub := range res.Subagents {
		cp, err := bindParts(ctx, env, sub.Agent, false, b)
		if err != nil {
			return nil, err
		}
		switch sub.EffectiveMode() {
		case definition.SubagentHandoff:
			h, err := handoffDef(sub, cp)
			if err != nil {
				return nil, err
			}
			p.handoffs = append(p.handoffs, h)
			if gate != nil {
				gate.free["handoff_to_"+cp.def.Name] = true
			}
		default:
			sd := subAgentDef(env, sub, cp)
			p.subagents = append(p.subagents, sd)
			if gate != nil {
				gate.free["delegate_to_"+cp.def.Name] = true
				gate.free["spawn_"+cp.def.Name] = true
			}
		}
	}
	if gate != nil {
		p.gate = gates(mcpGate, gate)
	} else {
		p.gate = mcpGate
	}
	return p, nil
}

// newBudget builds a budget from the spending fields of a definition.
func newBudget(maxCost float64, maxTokens, maxRequests int, spec *definition.BudgetSpec) *types.Budget {
	policy := types.BudgetPolicy{Limit: types.USD(maxCost), MaxTokens: maxTokens, MaxRequests: maxRequests}
	if spec != nil {
		policy.WarnAt = spec.WarnAt
		policy.AllowUnpriced = spec.AllowUnpriced
		if spec.OnExceed == definition.OnExceedAsk {
			policy.OnExceed = types.BudgetRequireApproval
		}
	}
	return types.NewBudget(policy)
}

// subAgentDef turns a bound child into a delegation. The child inherits
// the parent's run policy as every sub-agent does, and its own definition
// replaces what it declares: its approval rules (behind the host's gate),
// compaction, dials, limits and budget. Its guardrails and hooks run after
// the inherited ones.
func subAgentDef(env *Env, sub definition.ResolvedSubagent, cp *parts) agent.SubAgentDef {
	sd := agent.SubAgentDef{
		Name:         cp.def.Name,
		Description:  cp.def.Description,
		SystemPrompt: cp.prompt,
		Tools:        cp.registry(),
		SubAgents:    cp.subagents,
		MaxIter:      cp.maxIter,
	}
	if sub.Description != "" {
		sd.Description = sub.Description
	}
	if sd.Description == "" {
		sd.Description = "Delegate a task to the " + cp.def.Name + " agent."
	}
	if sub.EffectiveMode() == definition.SubagentSpawn {
		sd.Mode = agent.SubAgentSpawn
	}
	if cp.preset != nil {
		sd.Provider = cp.preset.Provider()
	}
	budget := cp.budget
	if bs := sub.Budget; bs != nil {
		if bs.MaxIterations > 0 {
			sd.MaxIter = bs.MaxIterations
		}
		sd.Timeout = time.Duration(bs.Timeout)
		if bs.MaxCost > 0 || bs.MaxTokens > 0 || bs.MaxRequests > 0 {
			budget = newBudget(bs.MaxCost, bs.MaxTokens, bs.MaxRequests, nil)
		}
	}
	var budgetPolicy *types.BudgetPolicy
	if budget != nil {
		p := budget.Policy()
		budgetPolicy = &p
	}
	hostGate := env.ToolGate
	sd.Options = append(sd.Options, func(c *agent.AgentConfig) {
		if cp.gate != nil || cp.approval != nil {
			c.ToolGate = gates(hostGate, cp.gate)
			if c.ToolGate == nil {
				c.ToolGate = types.AllowAllGate{}
			}
			c.ApprovalPolicy = cp.approval
		}
		if cp.toolPolicy != nil {
			c.ToolPolicy = cp.toolPolicy(c.ToolPolicy)
		}
		c.Hooks = append(c.Hooks, cp.hooks...)
		c.InputGuardrails = append(c.InputGuardrails, cp.input...)
		c.OutputGuardrails = append(c.OutputGuardrails, cp.output...)
		if cp.compact != nil {
			cc := cp.compact.Clone()
			c.CompactCfg = &cc
		}
		if cp.dials != nil {
			c.Dials = cp.dials.Clone()
		}
		if cp.llmTimeout > 0 {
			c.LLMTimeout = cp.llmTimeout
		}
		if cp.toolTimeout > 0 {
			c.ToolTimeout = cp.toolTimeout
		}
		if budgetPolicy != nil {
			// Options run for each delegation, so each gets its own
			// allowance.
			c.Budget = types.NewBudget(*budgetPolicy)
		}
	})
	if len(cp.handoffs) > 0 {
		sd.Options = append(sd.Options, agent.WithHandoffs(cp.handoffs...))
	}
	return sd
}

// handoffDef turns a bound child into a handoff member. A member shares
// the entry agent's run: its tree, gate, approvals, hooks, guardrails,
// compaction and budget. A definition that declares any of those, or has
// sub-agents of its own, cannot be a member.
func handoffDef(sub definition.ResolvedSubagent, cp *parts) (agent.HandoffDef, error) {
	d := cp.def
	var refused []string
	add := func(field string, set bool) {
		if set {
			refused = append(refused, field)
		}
	}
	add("approval", d.Approval != nil)
	add("skills", len(d.Skills) > 0)
	add("memory", d.Memory != nil)
	add("compaction", d.Compaction != nil)
	add("guardrails", d.Guardrails != nil)
	add("subagents", len(d.Subagents) > 0)
	add("tools.mcp", d.Tools != nil && len(d.Tools.MCP) > 0)
	add("limits.budget", d.Limits != nil && d.Limits.Budget != nil)
	add("limits.llm_timeout", d.Limits != nil && d.Limits.LLMTimeout != 0)
	add("limits.tool_timeout", d.Limits != nil && d.Limits.ToolTimeout != 0)
	if len(refused) > 0 {
		return agent.HandoffDef{}, fmt.Errorf("%w: %s is a handoff member and shares its entry agent's run, so it cannot declare %s; use mode delegate",
			ErrUnsupported, d.Name, strings.Join(refused, ", "))
	}
	h := agent.HandoffDef{
		Name:         d.Name,
		Description:  d.Description,
		SystemPrompt: cp.prompt,
		Tools:        cp.registry(),
		MaxIter:      cp.maxIter,
		Dials:        cp.dials,
	}
	if sub.Description != "" {
		h.Description = sub.Description
	}
	if sub.Budget != nil && sub.Budget.MaxIterations > 0 {
		h.MaxIter = sub.Budget.MaxIterations
	}
	if cp.preset != nil {
		h.Provider = cp.preset.Provider()
	}
	return h, nil
}
