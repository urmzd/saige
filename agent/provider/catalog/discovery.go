package catalog

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// Tier ranks a family within its provider by price and capability, so a
// caller can ask for "the economy model" without hard-coding a name.
type Tier string

const (
	// TierFrontier is the provider's most capable and most expensive line.
	TierFrontier Tier = "frontier"
	// TierStandard is the default balance of quality, latency and price.
	TierStandard Tier = "standard"
	// TierEconomy is the small, fast, cheap line.
	TierEconomy Tier = "economy"
)

// Fee is a flat per-use charge for a provider-executed tool, such as a web
// search billed per query on top of the tokens it adds to the prompt.
type Fee struct {
	Currency string  `json:"currency,omitempty"` // ISO code; empty means types.DefaultCurrency
	PerUse   float64 `json:"per_use"`            // charge for one invocation
	AsOf     string  `json:"as_of,omitempty"`    // date the rate was recorded
	Source   string  `json:"source,omitempty"`   // where the rate came from
}

// Reserve returns the most a call can spend on the tool when the provider may
// invoke it up to maxUses times. A budget reserves this before dispatch,
// because the invocations happen inside the provider and are only reported
// after the fact. maxUses <= 0 means the provider default, which is
// unbounded, so Reserve reports false.
func (f Fee) Reserve(maxUses int) (float64, bool) {
	if maxUses <= 0 {
		return 0, false
	}
	return f.PerUse * float64(maxUses), true
}

// ServerToolFee returns the per-use fee for a server tool on a model. The bool
// is false when the row does not price that tool, which a budget must treat as
// unpriced rather than free.
func ServerToolFee(provider, model string, kind types.ServerToolKind) (Fee, bool) {
	e, ok := Describe(provider, model)
	if !ok {
		return Fee{}, false
	}
	f, ok := e.ServerToolFees[kind]
	return f, ok
}

// Successor follows SupersededBy links from the row serving a model and
// returns the prefix of the newest family. The bool is false when the model's
// family is current or unknown. A cycle in the table stops at the last row
// not yet visited.
func Successor(provider, model string) (string, bool) {
	mu.RLock()
	defer mu.RUnlock()
	e, ok := match(provider, model)
	if !ok || e.SupersededBy == "" {
		return "", false
	}
	seen := map[string]bool{e.Prefix: true}
	next := e.SupersededBy
	for {
		row, ok := match(provider, next)
		if !ok || row.SupersededBy == "" || seen[row.SupersededBy] {
			return next, true
		}
		seen[row.Prefix] = true
		next = row.SupersededBy
	}
}

// Fits checks a request size against a model's declared limits. It returns
// an error matching types.ErrContextLength when the input exceeds the context
// window, when input plus output exceeds it, or when the output exceeds the
// declared output cap. A zero limit is undeclared, and an undeclared limit
// never rejects: check ContextWindow > 0 first when an unknown limit must
// fail closed.
func Fits(caps types.ModelCapabilities, inputTokens, outputTokens int) error {
	fail := func(reason string) error {
		return &types.ProviderError{Provider: caps.Provider, Model: caps.Model,
			Kind: types.ErrorKindContextLength, Err: fmt.Errorf("%w: %s", types.ErrContextLength, reason)}
	}
	if inputTokens < 0 || outputTokens < 0 {
		return fail("token counts must not be negative")
	}
	if caps.MaxOutputTokens > 0 && outputTokens > caps.MaxOutputTokens {
		return fail(fmt.Sprintf("output %d exceeds the declared cap %d", outputTokens, caps.MaxOutputTokens))
	}
	if caps.ContextWindow > 0 && inputTokens+outputTokens > caps.ContextWindow {
		return fail(fmt.Sprintf("input %d plus output %d exceeds the context window %d", inputTokens, outputTokens, caps.ContextWindow))
	}
	return nil
}

// InferProvider names the provider whose catalog rows match a model, by the
// longest declared prefix across every provider. An explicit
// "provider/model" form is honored first when the part before the slash is a
// cataloged provider. The bool is false when no row matches.
func InferProvider(model string) (provider string, ok bool) {
	if p, rest, found := strings.Cut(model, "/"); found && rest != "" {
		for _, known := range Providers() {
			if strings.EqualFold(p, known) {
				return known, true
			}
		}
	}
	mu.RLock()
	defer mu.RUnlock()
	want := normalize(model)
	bestLen := -1
	for _, e := range globalView().entries {
		if strings.EqualFold(strings.TrimSpace(model), strings.TrimSpace(e.Prefix)) {
			return e.Provider, true
		}
		p := normalize(e.Prefix)
		// Ties go to the alphabetically first provider so the answer does
		// not depend on map iteration order.
		if strings.HasPrefix(want, p) && (len(p) > bestLen || (len(p) == bestLen && e.Provider < provider)) {
			provider, bestLen = e.Provider, len(p)
		}
	}
	return provider, bestLen >= 0
}

// RemoteModel is one model as a provider's list endpoint reports it. Fields
// the endpoint does not return are zero.
type RemoteModel struct {
	ID              string
	DisplayName     string
	Created         time.Time
	ContextWindow   int   // declared input limit, if the endpoint reports one
	MaxOutputTokens int   // declared output limit, if the endpoint reports one
	SizeBytes       int64 // weight size, for local runtimes
	Embedding       bool  // true when the endpoint marks it as an embedding model
}

// ModelLister is implemented by adapters that can enumerate the models their
// endpoint serves. It is optional: a caller type-asserts for it.
type ModelLister interface {
	ListModels(ctx context.Context) ([]RemoteModel, error)
}

// Status says how the catalog accounts for a remote model.
type Status string

const (
	// StatusDeclared means the exact model has a row.
	StatusDeclared Status = "declared"
	// StatusInferred means a family prefix matches, but the model itself has
	// no row.
	StatusInferred Status = "inferred"
	// StatusUndeclared means only the provider baseline applies.
	StatusUndeclared Status = "undeclared"
)

// ReconciledModel is one remote model with the catalog's view of it.
type ReconciledModel struct {
	Remote       RemoteModel
	Status       Status
	Family       string // matched prefix; empty when undeclared
	Tier         Tier
	SupersededBy string
	// Drift lists limits the endpoint reports that disagree with the row.
	Drift []string
}

// Reconciliation compares what an endpoint serves with what the catalog
// declares for its provider.
type Reconciliation struct {
	Provider string
	Models   []ReconciledModel // sorted by ID
	// Unserved lists the provider's catalog prefixes that no remote model
	// matched. For a hosted provider this often means a retired family; for
	// a local runtime it means the weights are not pulled.
	Unserved []string
}

// Undeclared returns the IDs of remote models that only the baseline covers.
func (r Reconciliation) Undeclared() []string {
	var out []string
	for _, m := range r.Models {
		if m.Status == StatusUndeclared {
			out = append(out, m.Remote.ID)
		}
	}
	return out
}

// Reconcile classifies each remote model against the catalog. It does not
// register anything: discovery says what an endpoint serves, not what the
// model accepts, so adding a row stays a deliberate Register call.
func Reconcile(provider string, remote []RemoteModel) Reconciliation {
	mu.RLock()
	defer mu.RUnlock()
	out := Reconciliation{Provider: provider}
	matched := map[string]bool{}
	for _, rm := range remote {
		r := ReconciledModel{Remote: rm, Status: StatusUndeclared}
		if e, ok := match(provider, rm.ID); ok {
			matched[e.Prefix] = true
			r.Family, r.Tier, r.SupersededBy = e.Prefix, e.Tier, e.SupersededBy
			r.Status = StatusInferred
			if strings.EqualFold(strings.TrimSpace(rm.ID), strings.TrimSpace(e.Prefix)) {
				r.Status = StatusDeclared
			}
			if rm.ContextWindow > 0 && e.Caps.ContextWindow > 0 && rm.ContextWindow != e.Caps.ContextWindow {
				r.Drift = append(r.Drift, fmt.Sprintf("context window: endpoint %d, catalog %d", rm.ContextWindow, e.Caps.ContextWindow))
			}
			if rm.MaxOutputTokens > 0 && e.Caps.MaxOutputTokens > 0 && rm.MaxOutputTokens != e.Caps.MaxOutputTokens {
				r.Drift = append(r.Drift, fmt.Sprintf("max output tokens: endpoint %d, catalog %d", rm.MaxOutputTokens, e.Caps.MaxOutputTokens))
			}
		}
		out.Models = append(out.Models, r)
	}
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].Remote.ID < out.Models[j].Remote.ID })
	for _, e := range globalView().entries {
		if e.Provider == provider && !matched[e.Prefix] {
			out.Unserved = append(out.Unserved, e.Prefix)
		}
	}
	sort.Strings(out.Unserved)
	return out
}
