package bind

import (
	"fmt"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/definition"
	"github.com/urmzd/saige/agent/guardrail"
	"github.com/urmzd/saige/agent/privacy"
	"github.com/urmzd/saige/agent/types"
)

// bindGuardrails builds the named built-in guardrails. A classifier runs
// on the agent's own model, or the host's for a root agent the host
// overrides.
func bindGuardrails(env *Env, d *definition.Definition, p *parts, root bool) error {
	provider := func() types.Provider {
		if p.preset != nil {
			return p.preset.Provider()
		}
		if root && env.Preset != nil {
			return env.Preset.Provider()
		}
		return nil
	}
	build := func(g definition.GuardrailSpec) (agent.Guardrail, error) {
		switch g.Name {
		case definition.GuardrailPII:
			return guardrail.PII(g.Redact), nil
		case definition.GuardrailMaxLength:
			return guardrail.MaxLength(g.Max), nil
		case definition.GuardrailRegex:
			patterns := make([]privacy.Pattern, len(g.Patterns))
			for i, pt := range g.Patterns {
				patterns[i] = privacy.Pattern{Label: pt.Label, Expr: pt.Expr}
			}
			return guardrail.Regex("regex", g.Redact, patterns...)
		case definition.GuardrailClassifier:
			prov := provider()
			if prov == nil {
				return nil, fmt.Errorf("%w: agent %s: a classifier guardrail needs the agent to name its own model", ErrUnsupported, d.Name)
			}
			return guardrail.Classifier("", prov, g.Policy), nil
		}
		return nil, fmt.Errorf("%w: agent %s: unknown guardrail %q", ErrUnsupported, d.Name, g.Name)
	}
	for _, g := range d.Guardrails.Input {
		gr, err := build(g)
		if err != nil {
			return err
		}
		mode := agent.GuardrailSequential
		if g.Parallel {
			mode = agent.GuardrailParallel
		}
		p.input = append(p.input, agent.InputGuardrail{Guardrail: gr, Mode: mode})
	}
	for _, g := range d.Guardrails.Output {
		gr, err := build(g)
		if err != nil {
			return err
		}
		p.output = append(p.output, agent.OutputGuardrail{Guardrail: gr})
	}
	return nil
}
