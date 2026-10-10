package bind

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/definition"
	"github.com/urmzd/saige/agent/memory"
	"github.com/urmzd/saige/agent/types"
)

// bindMemory connects the definition to a store the host provides. Recall
// tool exposes recall; inject and select add matching memories in front of
// each user message of the owning agent's runs, as Policy.StartMessage
// builds them. Writes keep their approval marker, or ask through the
// approval gate when the definition has an approval block.
func bindMemory(env *Env, d *definition.Definition, p *parts, unmark func(types.Tool) types.Tool) error {
	spec := d.Memory
	store, ok := env.MemoryStores[spec.Store]
	if !ok {
		return fmt.Errorf("%w: agent %s names memory store %q, which the host does not provide", ErrUnsupported, d.Name, spec.Store)
	}
	if env.MemoryScope == nil {
		return fmt.Errorf("%w: agent %s uses memory, but the host maps no agent to a memory scope", ErrUnsupported, d.Name)
	}
	policy := memory.Policy{
		InjectBudget: spec.Budget,
		Retention:    time.Duration(spec.Retention),
		Scope: func(ctx context.Context, owner string) (memory.Scope, error) {
			s, err := env.MemoryScope(ctx, owner)
			if err != nil {
				return memory.Scope{}, err
			}
			s = s.Narrow(spec.Namespace)
			if spec.ReadOnly {
				s = s.AsReadOnly()
			}
			return s, nil
		},
	}
	for _, k := range spec.Write {
		policy.Write = append(policy.Write, memory.Kind(k))
	}
	switch spec.Recall {
	case definition.RecallInject:
		policy.Recall = memory.RecallByInjection
	case definition.RecallSelect:
		policy.Recall = memory.RecallBySelector
	case definition.RecallOff:
		policy.Recall = memory.RecallDisabled
	default:
		policy.Recall = memory.RecallByTool
	}

	var list []types.Tool
	switch {
	case spec.ReadOnly && policy.Recall != memory.RecallDisabled:
		list = []types.Tool{memory.RecallTool(store, policy)}
	case !spec.ReadOnly:
		list = memory.Tools(store, policy)
	}
	for _, t := range list {
		p.tools = append(p.tools, unmark(t))
	}
	if policy.Recall == memory.RecallByInjection || policy.Recall == memory.RecallBySelector {
		p.hooks = append(p.hooks, recallHook(d.Name, store, policy))
	}
	return nil
}

// recallHook puts recalled memories in front of each user message the
// owning agent receives.
func recallHook(owner string, store memory.Store, policy memory.Policy) agent.Hooks {
	return agent.Hooks{
		Name: "memory-recall:" + owner,
		UserInput: func(ctx context.Context, ev *agent.UserInputEvent) error {
			if ev.Agent != owner {
				return nil
			}
			msg, ok, err := policy.StartMessage(ctx, store, owner, userText(ev.Message))
			if err != nil {
				// Recall is context, not a precondition: the run goes on
				// without it.
				slog.Default().Warn("memory recall failed", "agent", owner, "error", err)
				return nil
			}
			if !ok {
				return nil
			}
			ev.Message = withPrefix(ev.Message, msg)
			return nil
		},
	}
}
