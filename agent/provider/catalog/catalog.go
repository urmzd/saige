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
// This table is data about the outside world and goes stale on the vendors'
// release schedule, not this repo's. Limits are only declared where they are
// solid; a zero ContextWindow or MaxOutputTokens means undeclared, never
// unlimited. Register lets callers add or override entries at runtime without
// waiting for an SDK release.
package catalog

import (
	"sort"
	"strings"
	"sync"

	"github.com/urmzd/saige/agent/registry"
	"github.com/urmzd/saige/agent/types"
)

// Entry is one row of the capability table: a model-name prefix and the
// capabilities every model matching it declares.
type Entry struct {
	// Provider is the adapter name the entry applies to.
	Provider string
	// Prefix matches model identifiers by longest-prefix. Use the family stem
	// ("claude-sonnet-4"), not a dated full name, so new point releases inherit
	// the entry instead of falling through to the baseline.
	Prefix string
	// Caps is the capability surface, minus Provider/Model/Family/Known, which
	// Lookup fills in from the match.
	Caps types.ModelCapabilities
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
	baseline = map[string]types.ModelCapabilities{}
)

// key is the registry name for one row.
func key(provider, prefix string) string { return provider + "/" + prefix }

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
	return e
}

// History returns every revision of one row, oldest first. Use it to see what a
// row said before a correction, and when it changed.
func History(provider, prefix string) []registry.Entry[Entry] {
	history := models.History(key(provider, prefix))
	for i := range history {
		history[i].Value = cloneEntry(history[i].Value)
	}
	return history
}

// Pin freezes a row to a revision. A deployment whose cost model was validated
// against a particular rate card pins it, so a later table update cannot move
// the numbers underneath a running budget.
func Pin(provider, prefix string, rev registry.Revision) error {
	return models.Pin(key(provider, prefix), rev)
}

// Unpin releases a pin.
func Unpin(provider, prefix string) { models.Unpin(key(provider, prefix)) }

// Rollback pins a row to its previous revision, for when a correction turns out
// to be the wrong correction.
func Rollback(provider, prefix string) (Entry, error) {
	e, err := models.Rollback(key(provider, prefix))
	return cloneEntry(e.Value), err
}

// Revisions returns how many revisions a row has.
func Revisions(provider, prefix string) int {
	return len(models.History(key(provider, prefix)))
}

// RegisterBaseline sets the conservative fallback for a provider, used when no
// prefix matches. Capabilities resolved from a baseline have Known false.
func RegisterBaseline(provider string, caps types.ModelCapabilities) {
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
func Lookup(provider, model string) (types.ModelCapabilities, bool) {
	mu.RLock()
	defer mu.RUnlock()

	want := normalize(model)
	var best Entry
	bestLen := -1
	for _, re := range models.All() {
		e := re.Value
		if e.Provider != provider {
			continue
		}
		p := normalize(e.Prefix)
		// Exact tag/path declarations override a normalized family with the
		// same length; otherwise registering pinned local weights would lose
		// to their shorter family name after normalization.
		if strings.EqualFold(strings.TrimSpace(model), strings.TrimSpace(e.Prefix)) {
			best, bestLen = e, len(p)
			break
		}
		if !strings.HasPrefix(want, p) {
			continue
		}
		if len(p) > bestLen {
			best, bestLen = e, len(p)
		}
	}

	if bestLen >= 0 {
		out := best.Caps.ForModel(model)
		out.Provider = provider
		out.Family = best.Prefix
		// A prefix or stripped Ollama tag/path identifies a possible family,
		// not a declaration for the requested model or weights.
		out.Known = strings.EqualFold(strings.TrimSpace(model), strings.TrimSpace(best.Prefix))
		if !out.Known {
			out.Notes = append(out.Notes, "capabilities inferred from family prefix; register the exact model to mark this declaration known")
		}
		return out, out.Known
	}

	if b, ok := baseline[provider]; ok {
		out := b.ForModel(model)
		out.Provider = provider
		out.Family = ""
		out.Known = false
		return out, false
	}
	return types.ModelCapabilities{Provider: provider, Model: model}, false
}

// MustLookup is Lookup without the found flag, for callers that already treat
// a baseline as an acceptable answer (adapters reporting their own
// capabilities, since a user pointing an adapter at an unlisted model is
// routine, not an error).
func MustLookup(provider, model string) types.ModelCapabilities {
	caps, _ := Lookup(provider, model)
	return caps
}

// Families returns the registered prefixes for a provider, sorted. Useful for
// `saige models` style listings and for tests that assert coverage.
func Families(provider string) []string {
	var out []string
	for _, re := range models.All() {
		if re.Value.Provider == provider {
			out = append(out, re.Value.Prefix)
		}
	}
	sort.Strings(out)
	return out
}

// Providers returns every provider with at least one entry or baseline, sorted.
func Providers() []string {
	mu.RLock()
	defer mu.RUnlock()
	seen := map[string]bool{}
	for _, re := range models.All() {
		seen[re.Value.Provider] = true
	}
	for p := range baseline {
		seen[p] = true
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
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

// caps is a small constructor for table rows: it turns a capability list into
// the map form and leaves Provider/Model/Family/Known for Lookup to fill.
func caps(list ...types.Capability) types.ModelCapabilities {
	m := make(map[types.Capability]bool, len(list))
	for _, c := range list {
		m[c] = true
	}
	return types.ModelCapabilities{Caps: m}
}

// media builds a ContentSupport set for a row.
func media(list ...types.MediaType) types.ContentSupport {
	m := make(map[types.MediaType]bool, len(list))
	for _, mt := range list {
		m[mt] = true
	}
	return types.ContentSupport{NativeTypes: m}
}
