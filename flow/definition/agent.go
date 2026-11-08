package definition

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	sdef "github.com/urmzd/saige/agent/definition"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/turn"
)

// Providers every flow knows. A deployment adds its own (Checks.Providers).
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
	ProviderOffline   = "offline"
)

// KnownProviders lists the built-in providers, sorted.
func KnownProviders() []string {
	return []string{ProviderAnthropic, ProviderOffline, ProviderOpenAI}
}

// MaxFallbacks bounds the models a definition falls back to.
const MaxFallbacks = 4

// Checks are the deployment facts a definition is validated against.
type Checks struct {
	// Providers is the provider table. Empty selects KnownProviders.
	Providers []string
	// Credential reports whether the deployment holds a credential for a
	// provider. Nil skips the check. When set, the primary model and every
	// fallback must have one: a fallback that cannot be reached is not a
	// fallback.
	Credential func(provider string) error
	// Accept reports whether a provider outside Providers may be named
	// anyway: one the deployment adds while it runs, such as a model
	// provider an administrator set up. Nil accepts none beyond Providers.
	Accept func(provider string) bool
}

// known reports whether a definition may name provider.
func (c Checks) known(provider string) bool {
	return slices.Contains(c.providers(), provider) || (c.Accept != nil && c.Accept(provider))
}

func (c Checks) providers() []string {
	if len(c.Providers) == 0 {
		return KnownProviders()
	}
	return c.Providers
}

// Budget is a saige budget in flow's units. Money is int64 micro-USD.
type Budget struct {
	MaxCostMicros int64 `json:"max_cost_micros,omitempty"`
	MaxTokens     int   `json:"max_tokens,omitempty"`
	MaxRequests   int   `json:"max_requests,omitempty"`
}

// Agent is one definition as flow runs it: the saige definition, its
// policy, its budgets in micro-USD, and the compiled turn.Spec a Runtime
// executes.
type Agent struct {
	Definition *sdef.Definition
	Policy     Policy
	// Budget is limits.budget, and Subagents the budget of each sub-agent
	// delegation by name.
	Budget    Budget
	Subagents map[string]Budget
	Spec      turn.Spec
}

// Parse reads one .agent.md file and its policy and checks both: saige's
// own validation, then flow's. path names the file in errors.
func Parse(content string, policy Policy, checks Checks) (Agent, error) {
	def, err := sdef.Parse([]byte(content), "")
	if err != nil {
		return Agent{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return check(def, policy, checks)
}

// check applies flow's rules to a parsed definition. Everything flow
// cannot honor is refused rather than ignored.
//
//nolint:gocyclo // one check per definition field, in the order a person reads them
func check(def *sdef.Definition, policy Policy, checks Checks) (Agent, error) {
	name := def.Name
	fail := func(format string, args ...any) (Agent, error) {
		return Agent{}, fmt.Errorf("%w: agent %q: %s", ErrInvalid, name, fmt.Sprintf(format, args...))
	}
	if !event.ValidToken(name) {
		return fail("the name is not a valid channel segment")
	}
	if err := policy.Validate(); err != nil {
		return Agent{}, fmt.Errorf("agent %q: %w", name, err)
	}
	policy = policy.Normalize()
	if strings.TrimSpace(def.Prompt) == "" {
		return fail("the prompt is empty; an agent with no instructions is a definition of nothing")
	}
	switch {
	case def.Dials != nil:
		return fail("dials are not supported")
	case len(def.Skills) > 0:
		return fail("skills are not supported")
	case def.Memory != nil:
		return fail("memory is not supported")
	case def.Guardrails != nil:
		return fail("guardrails are not supported")
	case def.Compaction != nil:
		return fail("compaction is set by the host, not the definition")
	case def.Tools != nil && len(def.Tools.Harness) > 0:
		return fail("harness tools are not supported")
	}

	// The model.
	if def.Model == nil || strings.TrimSpace(def.Model.Use) == "" {
		return fail("a model is required")
	}
	providers := checks.providers()
	refs := append([]string{def.Model.Use}, def.Model.Fallback...)
	if len(refs)-1 > MaxFallbacks {
		return fail("%d fallback models, the limit is %d", len(refs)-1, MaxFallbacks)
	}
	models := make([]turn.ModelRef, 0, len(refs))
	seen := map[turn.ModelRef]int{}
	for i, ref := range refs {
		what := "the model"
		if i > 0 {
			what = fmt.Sprintf("fallback %d", i)
		}
		prov, model, ok := strings.Cut(ref, "/")
		if !ok || prov == "" || model == "" {
			return fail("%s %q must be provider/model; presets are not supported", what, ref)
		}
		if !checks.known(prov) {
			return fail("%s names provider %q; this deployment knows %s", what, prov, strings.Join(sorted(providers), ", "))
		}
		if checks.Credential != nil {
			if err := checks.Credential(prov); err != nil {
				return fail("%s needs a credential for %q: %v", what, prov, err)
			}
		}
		m := turn.ModelRef{Provider: prov, Model: model}
		if prev, dup := seen[m]; dup {
			repeated := "the model"
			if prev > 0 {
				repeated = fmt.Sprintf("fallback %d", prev)
			}
			return fail("%s repeats %s", what, repeated)
		}
		seen[m] = i
		models = append(models, m)
	}
	if c := policy.Choices; c != nil {
		for _, m := range c.Models {
			prov, _, _ := strings.Cut(m, "/")
			if !checks.known(prov) {
				return fail("model choice %q names provider %q; this deployment knows %s", m, prov, strings.Join(sorted(providers), ", "))
			}
		}
	}
	if len(policy.FallbackOn) > 0 && len(models) == 1 {
		return fail("the policy says when to fall back but the definition names no fallback models")
	}

	// Tools: registry names, and MCP servers as group grants.
	var tools []string
	var servers []string
	if def.Tools != nil {
		for _, t := range def.Tools.Registry {
			if strings.HasPrefix(t, MCPGroupPrefix) {
				return fail("tool %q is an MCP grant; list the server under tools.mcp", t)
			}
			tools = append(tools, t)
		}
		for _, m := range def.Tools.MCP {
			if len(m.Allow) > 0 {
				return fail("tools.mcp %q lists tools; fiber grants a server's accepted tools as a whole", m.Server)
			}
			if _, _, err := ParseMCPGrant(MCPGroupPrefix + m.Server); err != nil {
				return fail("%v", err)
			}
			servers = append(servers, m.Server)
			tools = append(tools, MCPGrant{Provider: m.Server, Write: policy.MayWrite(m.Server)}.String())
		}
	}
	for _, s := range policy.MCPWrite {
		if !slices.Contains(servers, s) {
			return fail("the policy grants writes to MCP server %q, which tools.mcp does not list", s)
		}
	}

	// Sub-agents are what the agent may delegate to.
	var spawn []string
	subBudgets := map[string]Budget{}
	for _, s := range def.Subagents {
		if s.EffectiveMode() != sdef.SubagentDelegate {
			return fail("sub-agent %q is a %s; fiber delegates only", s.Ref, s.EffectiveMode())
		}
		ref, err := sdef.ParseRef(s.Ref)
		if err != nil {
			return fail("sub-agent %q: %v", s.Ref, err)
		}
		spawn = append(spawn, ref.Name)
		if s.Budget != nil {
			b, err := budget(s.Budget.MaxCost, s.Budget.MaxTokens, s.Budget.MaxRequests)
			if err != nil {
				return fail("sub-agent %q budget: %v", ref.Name, err)
			}
			subBudgets[ref.Name] = b
		}
	}

	// Approvals: only ask, and only for tools the agent holds.
	var approvals []string
	if a := def.Approval; a != nil {
		if len(a.Capabilities) > 0 || len(a.Allow) > 0 || len(a.Deny) > 0 || a.Grant != "" || a.DenyAfter != 0 || a.RampAfter != 0 || a.HideDenied {
			return fail("approval supports ask only")
		}
		for _, t := range a.Ask {
			t = strings.TrimSpace(t)
			if strings.ContainsAny(t, "(*") {
				return fail("approval.ask %q must name one tool", t)
			}
			if strings.HasPrefix(t, MCPGroupPrefix) {
				return fail("approval.ask %q is a tool group; name each tool that needs approval", t)
			}
			granted := slices.Contains(tools, t) || len(servers) > 0
			if target, ok := strings.CutPrefix(t, "tag_"); ok && slices.Contains(spawn, target) {
				granted = true
			}
			if !granted {
				return fail("approval.ask %q is not a tool it is granted", t)
			}
			approvals = append(approvals, t)
		}
	}
	if len(approvals) == 0 && (len(policy.Approvers) > 0 || policy.SelfApproval) {
		return fail("the policy names who may approve but the definition asks for no approvals")
	}

	var limits Budget
	if def.Limits != nil && def.Limits.Budget != nil {
		b, err := budget(def.Limits.Budget.MaxCost, def.Limits.Budget.MaxTokens, def.Limits.Budget.MaxRequests)
		if err != nil {
			return fail("limits.budget: %v", err)
		}
		limits = b
	}

	spec := turn.Spec{
		Name:         name,
		Description:  def.Description,
		Provider:     models[0].Provider,
		Model:        models[0].Model,
		SystemPrompt: strings.TrimSpace(def.Prompt),
		Tools:        sorted(tools),
		Access:       turn.Policy{Spawn: sorted(spawn), WorkspaceAreas: policy.WorkspaceAreas},
		FallbackOn:   policy.FallbackOn,
		Approvals:    sorted(approvals),
		Approvers:    policy.Approvers,
		SelfApproval: policy.SelfApproval,
		Answerers:    policy.Answerers,
		TaggedOnly:   policy.TaggedOnly,
	}
	if len(models) > 1 {
		spec.Fallbacks = models[1:]
	}
	for _, c := range policy.Knowledge {
		spec.Sources = append(spec.Sources, turn.Source{Name: c.Name, URI: c.URI, Options: c.Options})
	}
	if len(subBudgets) == 0 {
		subBudgets = nil
	}
	return Agent{Definition: def, Policy: policy, Budget: limits, Subagents: subBudgets, Spec: spec}, nil
}

// maxUSD keeps a converted budget well inside int64 micro-USD and inside
// the integers a float64 holds exactly.
const maxUSD = 9_000_000_000

// Micros converts a saige dollar amount to micro-USD. A value that is
// negative, not finite, too large, or finer than one micro-dollar cannot be
// represented and is refused.
func Micros(usd float64) (int64, error) {
	if math.IsNaN(usd) || math.IsInf(usd, 0) || usd < 0 {
		return 0, fmt.Errorf("%w: %v is not an amount of money", ErrInvalid, usd)
	}
	if usd > maxUSD {
		return 0, fmt.Errorf("%w: %v is above the %d limit", ErrInvalid, usd, maxUSD)
	}
	text := strconv.FormatFloat(usd, 'f', -1, 64)
	whole, frac, _ := strings.Cut(text, ".")
	if len(frac) > 6 {
		return 0, fmt.Errorf("%w: %s is finer than one micro-dollar", ErrInvalid, text)
	}
	frac += strings.Repeat("0", 6-len(frac))
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: %v", ErrInvalid, text, err)
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: %v", ErrInvalid, text, err)
	}
	return w*1_000_000 + f, nil
}

func budget(cost float64, tokens, requests int) (Budget, error) {
	m, err := Micros(cost)
	if err != nil {
		return Budget{}, err
	}
	if tokens < 0 || requests < 0 {
		return Budget{}, errors.New("limits must not be negative")
	}
	return Budget{MaxCostMicros: m, MaxTokens: tokens, MaxRequests: requests}, nil
}

func sorted(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := slices.Clone(values)
	slices.Sort(out)
	return out
}

// parseVersion is saige's semantic version parser.
func parseVersion(s string) (sdef.Version, error) { return sdef.ParseVersion(s) }

// CompareVersions compares two semantic versions: negative when a is below
// b, zero when equal, positive when above.
func CompareVersions(a, b string) (int, error) {
	va, err := parseVersion(a)
	if err != nil {
		return 0, fmt.Errorf("%w: version %q: %v", ErrInvalid, a, err)
	}
	vb, err := parseVersion(b)
	if err != nil {
		return 0, fmt.Errorf("%w: version %q: %v", ErrInvalid, b, err)
	}
	return va.Compare(vb), nil
}

// Major is the major number of a semantic version.
func Major(v string) (int, error) {
	pv, err := parseVersion(v)
	if err != nil {
		return 0, fmt.Errorf("%w: version %q: %v", ErrInvalid, v, err)
	}
	return pv.Major, nil
}
