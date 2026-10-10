// Package catalog is the capability list: it maps a (provider, model) pair to
// the types.ModelCapabilities that model declares.
//
// It exists because "which provider am I talking to" and "what may I ask for"
// are different questions. Every adapter in this SDK implements the same
// Provider interface, but the models behind them accept wildly different
// request shapes: Anthropic sizes reasoning with a token budget, OpenAI's
// reasoning models take an effort enum and reject temperature outright, Gemini
// takes a thinking level or a budget, and ollama exposes a bare on/off toggle
// whose availability depends on which weights were pulled. Code that assumes
// one shape and gets another fails at the API boundary, or worse, is accepted
// and silently ignored.
//
// # Matching
//
// Model identifiers carry dated and versioned suffixes
// ("claude-sonnet-4-5-20250514", "gemini-3-flash-preview"), so entries are
// matched by longest declared prefix. Only an exact declared name has Known
// true; prefix/tag/path inference keeps Known false. An unrecognised model falls through to
// the provider's Baseline: a deliberately conservative entry with Known set
// false, so callers that must fail closed can tell "declared unsupported" from
// "never heard of it".
//
// # Maintenance
//
// The table is data about the outside world and goes stale on the vendors'
// release schedule, not this repo's, so it ships as JSON (data/default.json)
// rather than code. Limits are only declared where they are solid; a zero
// ContextWindow or MaxOutputTokens means undeclared, never unlimited. Hosts
// layer their own catalogs from any Source and install them with Use or
// Install; Register adds or overrides single entries at runtime.
//
// # Presets
//
// A catalog also names presets: ordered chains of complete provider
// configurations. Resolve applies the option precedence to every chain entry
// and validates each against its own model. Presets never become global
// state; package preset builds them into providers.
package catalog

import (
	"slices"
	"strings"
	"sync"

	"github.com/urmzd/saige/agent/registry"
	"github.com/urmzd/saige/agent/types"
)

// Entry is one row of the capability table: a model-name prefix and the
// capabilities every model matching it declares.
type Entry struct {
	// Provider is the vendor the entry applies to.
	Provider types.ProviderName
	// Prefix matches model identifiers by longest-prefix. Use the family stem
	// ("claude-sonnet-4"), not a dated full name, so new point releases inherit
	// the entry instead of falling through to the baseline.
	Prefix types.ModelID
	// Caps is the capability surface, minus Provider/Model/Family/Known, which
	// Lookup fills in from the match.
	Caps types.ModelCapabilities
	// Tier places the family on a price and quality ladder within its
	// provider. Empty means unranked.
	Tier Tier
	// SupersededBy names the prefix of the family the vendor recommends
	// instead. Empty means the family is current.
	SupersededBy types.ModelID
	// ServerToolFees holds per-use charges for provider-executed tools, which
	// are billed on top of tokens. A kind absent from the map is unpriced,
	// not free.
	ServerToolFees map[types.ServerToolKind]Fee
	// Defaults are the model-level option defaults a preset entry starts
	// from. Nil means none.
	Defaults *OptionsSpec
	// Dials is the row's dials declaration, merged through its templates.
	// Caps.DialMap holds its compiled form. Nil means none.
	Dials *DialsSpec `json:",omitempty"`
	// Offerings are the model's offerings by endpoint name. Caps projects
	// the one on the vendor's primary endpoint. Nil for a row registered as
	// capabilities alone.
	Offerings map[string]types.Offering `json:"-"`

	// removed marks a tombstone: a row a later Install no longer declares.
	removed bool
}

// models is the revisioned store behind the table, keyed by "provider/prefix".
//
// Revisions matter here more than anywhere else in the SDK. Rows encode
// third-party facts that change without warning: a vendor cuts a price, adds a
// reasoning knob, deprecates a family. Registering a correction appends a
// revision rather than overwriting, so a deployment can see what a row used to
// say, pin to the version its budget was calculated against, and roll back a
// bad update without redeploying.
var (
	mu       sync.RWMutex
	models   = registry.New[Entry]()
	baseline = map[types.ProviderName]types.ModelCapabilities{}
)

// key is the registry name for one row.
func key(provider types.ProviderName, prefix types.ModelID) string { return modelKey(provider, prefix) }

// Register adds a revision for a provider+prefix row and returns it. Resolution
// takes the newest revision unless the row is pinned, so this both seeds the
// table at init and corrects it at runtime. Inputs and returned values are
// detached snapshots; subsequent caller mutations cannot rewrite a revision.
func Register(e Entry, opts ...registry.Option) registry.Entry[Entry] {
	stored := models.Register(key(e.Provider, e.Prefix), cloneEntry(e), opts...)
	stored.Value = cloneEntry(stored.Value)
	return stored
}

// cloneEntry owns every mutable map/slice in model metadata. Keep copying at
// catalog boundaries rather than assuming the generic registry clones values.
func cloneEntry(e Entry) Entry {
	e.Caps = e.Caps.ForModel(e.Caps.Model)
	if e.ServerToolFees != nil {
		fees := make(map[types.ServerToolKind]Fee, len(e.ServerToolFees))
		for k, v := range e.ServerToolFees {
			fees[k] = v
		}
		e.ServerToolFees = fees
	}
	e.Defaults = e.Defaults.clone()
	e.Dials = e.Dials.clone()
	if e.Offerings != nil {
		offs := make(map[string]types.Offering, len(e.Offerings))
		for k, o := range e.Offerings {
			offs[k] = o.Clone()
		}
		e.Offerings = offs
	}
	return e
}

// History returns every revision of one row, oldest first. Use it to see what a
// row said before a correction, and when it changed.
func History(provider types.ProviderName, prefix types.ModelID) []registry.Entry[Entry] {
	history := models.History(key(provider, prefix))
	for i := range history {
		history[i].Value = cloneEntry(history[i].Value)
	}
	return history
}

// Pin freezes a row to a revision. A deployment whose cost model was validated
// against a particular rate card pins it, so a later table update cannot move
// the numbers underneath a running budget.
func Pin(provider types.ProviderName, prefix types.ModelID, rev registry.Revision) error {
	return models.Pin(key(provider, prefix), rev)
}

// Unpin releases a pin.
func Unpin(provider types.ProviderName, prefix types.ModelID) { models.Unpin(key(provider, prefix)) }

// Rollback pins a row to its previous revision, for when a correction turns out
// to be the wrong correction.
func Rollback(provider types.ProviderName, prefix types.ModelID) (Entry, error) {
	e, err := models.Rollback(key(provider, prefix))
	return cloneEntry(e.Value), err
}

// Revisions returns how many revisions a row has.
func Revisions(provider types.ProviderName, prefix types.ModelID) int {
	return len(models.History(key(provider, prefix)))
}

// RegisterBaseline sets the conservative fallback for a provider, used when no
// prefix matches. Capabilities resolved from a baseline have Known false.
func RegisterBaseline(provider types.ProviderName, caps types.ModelCapabilities) {
	mu.Lock()
	defer mu.Unlock()
	baseline[provider] = caps.ForModel(caps.Model)
}

// Lookup resolves the capabilities of one (provider, model) pair. The bool
// reports whether the exact model is declared (also ModelCapabilities.Known).
// A prefix-inferred result retains Family and capabilities but returns false;
// a provider baseline has an empty Family and also returns false. Neither is
// verified for the requested model.
//
// Matching is case-insensitive and ignores an ollama-style ":tag" suffix for
// prefix purposes only, so "qwen3:4b" matches the "qwen3" entry.
//
// The model may be a types.ModelID or any string-kinded vendor model type.
// A row read from a catalog file carries its offering on the vendor's
// primary endpoint in ModelCapabilities.Offering.
func Lookup[M ~string](provider types.ProviderName, model M) (types.ModelCapabilities, bool) {
	mu.RLock()
	defer mu.RUnlock()
	return globalView().lookup(provider, string(model))
}

// LookupOffering returns the offering that serves a model on an endpoint
// of the installed catalog, as Catalog.Offering does.
func LookupOffering[M ~string](endpoint string, provider types.ProviderName, model M) (types.Offering, bool) {
	cat := Active()
	mu.RLock()
	defer mu.RUnlock()
	if e, ok := globalView().match(provider, string(model)); ok && e.Offerings == nil {
		// A row registered as capabilities has no offerings of its own.
		o := types.OfferingFromCapabilities(e.Caps.ForModel(string(model)))
		return o, true
	}
	return cat.view().offering(endpoint, provider, string(model))
}

// view is a set of resolved rows and baselines to match against: the global
// registry, or one catalog value that has not been installed.
type view struct {
	entries   []Entry // sorted by key
	baselines map[types.ProviderName]types.ModelCapabilities
	// index is the resolved catalog the view came from, nil for the
	// global registry.
	index *index
}

// globalView snapshots the registry. The caller holds mu.
func globalView() view {
	all := models.All()
	v := view{entries: make([]Entry, 0, len(all)), baselines: baseline}
	for _, re := range all {
		if !re.Value.removed {
			v.entries = append(v.entries, re.Value)
		}
	}
	return v
}

func (v view) lookup(provider types.ProviderName, model string) (types.ModelCapabilities, bool) {
	if best, ok := v.match(provider, model); ok {
		out := best.Caps.ForModel(model)
		out.Provider = string(provider)
		out.Family = string(best.Prefix)
		if out.Offering == nil {
			o := types.OfferingFromCapabilities(out)
			o.Model.Known = true
			out.Offering = &o
		}
		// A prefix or stripped Ollama tag/path identifies a possible family,
		// not a declaration for the requested model or weights.
		out.Known = strings.EqualFold(strings.TrimSpace(model), strings.TrimSpace(string(best.Prefix)))
		if !out.Known {
			out.Notes = append(out.Notes, "capabilities inferred from family prefix; register the exact model to mark this declaration known")
		}
		return out, out.Known
	}

	if b, ok := v.baselines[provider]; ok {
		out := b.ForModel(model)
		out.Provider = string(provider)
		out.Family = ""
		out.Known = false
		return out, false
	}
	return types.ModelCapabilities{Provider: string(provider), Model: model}, false
}

// offering returns the offering of the family that serves a model on an
// endpoint, or the endpoint's baseline.
func (v view) offering(endpoint string, provider types.ProviderName, model string) (types.Offering, bool) {
	if v.index == nil {
		return types.Offering{}, false
	}
	if e, ok := v.match(provider, model); ok {
		if r := v.index.offerings[endpoint][modelKey(e.Provider, e.Prefix)]; r != nil {
			return r.off.Clone(), true
		}
	}
	if b := v.index.baselines[endpoint][provider]; b != nil {
		return b.off.Clone(), true
	}
	return types.Offering{}, false
}

// match returns the row that serves a model: an exact declaration first,
// otherwise the longest matching prefix.
func (v view) match(provider types.ProviderName, model string) (Entry, bool) {
	want := normalize(model)
	var best Entry
	bestLen := -1
	for _, e := range v.entries {
		if e.Provider != provider {
			continue
		}
		p := normalize(string(e.Prefix))
		// Exact tag/path declarations override a normalized family with the
		// same length; otherwise registering pinned local weights would lose
		// to their shorter family name after normalization.
		if strings.EqualFold(strings.TrimSpace(model), strings.TrimSpace(string(e.Prefix))) {
			return e, true
		}
		if !strings.HasPrefix(want, p) {
			continue
		}
		if len(p) > bestLen {
			best, bestLen = e, len(p)
		}
	}
	return best, bestLen >= 0
}

// match returns the global row that serves a model. The caller holds mu.
func match(provider types.ProviderName, model string) (Entry, bool) {
	return globalView().match(provider, model)
}

// Describe returns the row that serves a model, including the row-level
// metadata Lookup does not carry (Tier, SupersededBy, ServerToolFees). The
// bool is false when only the provider baseline applies.
func Describe[M ~string](provider types.ProviderName, model M) (Entry, bool) {
	mu.RLock()
	defer mu.RUnlock()
	e, ok := match(provider, string(model))
	if !ok {
		return Entry{}, false
	}
	return cloneEntry(e), true
}

// MustLookup is Lookup without the found flag, for callers that already treat
// a baseline as an acceptable answer (adapters reporting their own
// capabilities, since a user pointing an adapter at an unlisted model is
// routine, not an error).
func MustLookup[M ~string](provider types.ProviderName, model M) types.ModelCapabilities {
	caps, _ := Lookup(provider, model)
	return caps
}

// Families returns the registered prefixes for a provider, sorted. Useful for
// `saige models` style listings and for tests that assert coverage.
func Families(provider types.ProviderName) []types.ModelID {
	mu.RLock()
	defer mu.RUnlock()
	var out []types.ModelID
	for _, e := range globalView().entries {
		if e.Provider == provider {
			out = append(out, e.Prefix)
		}
	}
	slices.Sort(out)
	return out
}

// Providers returns every provider with at least one entry or baseline, sorted.
func Providers() []types.ProviderName {
	mu.RLock()
	defer mu.RUnlock()
	seen := map[types.ProviderName]bool{}
	for _, e := range globalView().entries {
		seen[e.Provider] = true
	}
	for p := range baseline {
		seen[p] = true
	}
	out := make([]types.ProviderName, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// normalize lowercases and strips an ollama tag suffix ("qwen3:4b" -> "qwen3")
// plus any registry path prefix ("hf.co/user/qwen3" -> "qwen3"), so tags and
// mirrors do not defeat prefix matching.
func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[:i]
	}
	return s
}
