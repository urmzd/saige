// Package preset builds the provider chain a catalog preset declares.
//
// A preset is data: an ordered chain of complete entries, each a provider, a
// model and the options resolved for that model alone (see
// catalog.Catalog.Resolve). Build turns every entry into its own adapter,
// wraps it in a retry decorator and an optional per-attempt deadline, and
// puts all of them behind one router. Failover therefore moves between
// configurations someone wrote, never copies one model's temperature,
// reasoning or cache settings onto another model.
//
// The bundle's router has one group per preset, in chain order, and the
// primary preset is the default group. ConfigContent.Model, or an outcome
// policy's model switch, can name another built preset or a single profile
// ID to select it.
package preset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/urmzd/saige/agent/provider"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/retry"
	"github.com/urmzd/saige/agent/provider/router"
	"github.com/urmzd/saige/agent/types"
)

// Options configures Build.
type Options struct {
	// Getenv reads credentials. Nil uses os.Getenv.
	Getenv func(string) string
	// HTTPClient is passed to every adapter.
	HTTPClient *http.Client
	// Factory builds one adapter. Nil uses provider.Build. Tests and hosts
	// with custom clients (for example an Ollama client with a logger)
	// inject their own.
	Factory func(context.Context, provider.Config) (types.Provider, error)
	// Budget, when set, is read by the router for headroom. Entries without
	// a rate card are reported as warnings, since the budget cannot be
	// enforced against them.
	Budget *types.Budget
	// Now is the router clock. Nil uses time.Now.
	Now func() time.Time
	// Probe checks that an optional entry which needs no credentials can be
	// reached, so Build can drop it the way it drops an optional entry with
	// missing credentials. It is called with the entry's adapter
	// configuration and should return quickly. Nil uses ProbeOllama, which
	// checks a local Ollama server and accepts every other provider.
	Probe func(context.Context, provider.Config) error
	// ListLocal lists the models pulled on a local server, for entries
	// with local_fallback. Nil uses ListOllama.
	ListLocal func(context.Context, provider.Config) ([]catalog.RemoteModel, error)
}

// probeTimeout bounds the default reachability check.
const probeTimeout = 2 * time.Second

// ProbeOllama is the default Options.Probe. For an Ollama entry it asks the
// server for its version and fails when the server does not answer; every
// other provider passes. The host is the entry's base URL, then
// OLLAMA_HOST, then provider.DefaultOllamaHost.
func ProbeOllama(ctx context.Context, cfg provider.Config) error {
	if cfg.Provider != provider.Ollama {
		return nil
	}
	host := ollamaHost(cfg)
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(host, "/")+"/api/version", nil)
	if err != nil {
		return fmt.Errorf("ollama at %s: %w", host, err)
	}
	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ollama is not reachable at %s", host)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama at %s answered HTTP %d", host, resp.StatusCode)
	}
	return nil
}

// Available reports whether an entry can serve: its credentials are set
// and, for an entry that needs none (a local Ollama server), the probe
// reaches it. A nil error means Build would keep the entry even if it were
// optional. Hosts use it to pick the first usable entry of a chain.
func Available(ctx context.Context, e catalog.ResolvedEntry, o Options) error {
	o = o.withDefaults()
	if _, missing := credentials(e, o.Getenv); missing {
		return fmt.Errorf("no credentials for %s: %s", e.Provider, credentialHint(e))
	}
	if needsNoKey(e) {
		if err := o.Probe(ctx, config(e, "", o)); err != nil {
			return err
		}
	}
	if e.Provider == provider.Ollama && e.LocalFallback {
		if pulled, err := o.ListLocal(ctx, config(e, "", o)); err == nil && chooseLocalModel(e.Model, pulled) == "" {
			return noLocalModel(e.Model)
		}
	}
	return nil
}

// needsNoKey reports whether an entry runs without credentials, which is
// when only a reachability probe can tell that it will serve.
func needsNoKey(e catalog.ResolvedEntry) bool {
	return e.APIKeyEnv == "" && len(provider.APIKeyEnv[e.Provider]) == 0
}

func (o Options) withDefaults() Options {
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Factory == nil {
		o.Factory = provider.Build
	}
	if o.Probe == nil {
		o.Probe = ProbeOllama
	}
	if o.ListLocal == nil {
		o.ListLocal = ListOllama
	}
	return o
}

// Bundle is a built set of presets behind one router.
type Bundle struct {
	router   *router.Router
	primary  string
	resolved map[string]catalog.ResolvedPreset
	revision string
	warnings []catalog.Issue
}

var _ types.Preset = (*Bundle)(nil)

// Build resolves primary and every preset in also from cat, builds each
// chain entry's adapter, and returns them behind one router whose default
// group is primary. A name that is not a preset but has the form
// "provider/model" resolves through catalog.ResolveModel. Construction fails
// on the first error, except that an optional entry whose credentials are
// missing is dropped with a warning.
//
// An Ollama entry with local_fallback whose model is not pulled serves
// another pulled chat model instead (see EntrySpec.LocalFallback), with a
// WarnLocalModel warning. When the server has no chat model pulled, the
// entry fails with ErrNoLocalModel, or is dropped when it is optional.
func Build(ctx context.Context, cat *catalog.Catalog, primary string, also []string, o Options) (*Bundle, error) {
	if cat == nil {
		return nil, errors.New("preset: Build needs a catalog")
	}
	o = o.withDefaults()
	names := []string{primary}
	for _, n := range also {
		if !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	b := &Bundle{primary: primary, resolved: map[string]catalog.ResolvedPreset{}, revision: cat.Revision}
	var profiles []router.Profile
	groups := map[string][]string{}
	var closers []types.Provider
	fail := func(err error) (*Bundle, error) {
		for _, p := range closers {
			_ = types.CloseProvider(p)
		}
		return nil, err
	}
	// Entries with local_fallback name a model that is pulled before they
	// resolve, so each is validated against the model that will serve.
	cat, localWarnings, noLocal := pickLocalModels(ctx, cat, names, o)
	b.warnings = append(b.warnings, localWarnings...)
	var primarySpec catalog.ResolvedPreset
	for _, name := range names {
		rp, err := resolve(cat, name)
		if err != nil {
			return fail(fmt.Errorf("preset %s: %w", name, err))
		}
		b.warnings = append(b.warnings, rp.Warnings...)
		var kept []catalog.ResolvedEntry
		for _, e := range rp.Chain {
			if slices.ContainsFunc(profiles, func(p router.Profile) bool { return p.ID == e.ProfileID }) {
				groups[name] = append(groups[name], e.ProfileID)
				kept = append(kept, e)
				continue
			}
			key, missing := credentials(e, o.Getenv)
			if missing {
				if e.Optional {
					b.warnings = append(b.warnings, catalog.Issue{Path: "presets." + name, Code: "entry_dropped",
						Message: fmt.Sprintf("optional entry %s dropped: no credentials for %s", e.ID, e.Provider), Severity: catalog.SeverityWarning})
					continue
				}
				return fail(&types.ProviderError{Provider: e.Provider, Model: e.Model, Kind: types.ErrorKindAuth,
					Err: fmt.Errorf("%w: preset %s entry %s: %s", types.ErrAuth, name, e.ID, credentialHint(e))})
			}
			if e.Provider == provider.Ollama {
				if err := noLocal[localKey(e.BaseURL, e.Model)]; err != nil {
					if !e.Optional {
						return fail(fmt.Errorf("preset %s entry %s: %w", name, e.ID, err))
					}
					b.warnings = append(b.warnings, catalog.Issue{Path: "presets." + name, Code: "entry_dropped",
						Message: fmt.Sprintf("optional entry %s dropped: %v", e.ID, err), Severity: catalog.SeverityWarning})
					continue
				}
			}
			if e.Optional && needsNoKey(e) {
				// An entry without credentials is only known to serve once
				// something answers: an optional local server that is not
				// running is dropped like an optional entry without a key.
				if err := o.Probe(ctx, config(e, key, o)); err != nil {
					b.warnings = append(b.warnings, catalog.Issue{Path: "presets." + name, Code: "entry_dropped",
						Message: fmt.Sprintf("optional entry %s dropped: %v", e.ID, err), Severity: catalog.SeverityWarning})
					continue
				}
			}
			if o.Budget != nil && e.Caps.Pricing.IsZero() {
				b.warnings = append(b.warnings, catalog.Issue{Path: "presets." + name, Code: catalog.WarnUnpriced,
					Message: fmt.Sprintf("entry %s (%s/%s) is unpriced, so the budget cannot be enforced when it serves", e.ID, e.Provider, e.Model), Severity: catalog.SeverityWarning})
			}
			p, err := o.Factory(ctx, config(e, key, o))
			if err != nil {
				return fail(fmt.Errorf("preset %s entry %s: %w", name, e.ID, err))
			}
			closers = append(closers, p)
			if e.Retry == nil || !e.Retry.Disable {
				p = retry.New(p, retryConfig(e.Retry))
			}
			// The deadline sits inside retry, so each attempt gets its own.
			if e.AttemptTimeout > 0 {
				p = retryInside(p, e.AttemptTimeout)
			}
			profiles = append(profiles, router.Profile{ID: e.ProfileID, Provider: p,
				ConfigHash: e.ConfigHash, Preset: name, CatalogRevision: rp.CatalogRevision})
			groups[name] = append(groups[name], e.ProfileID)
			kept = append(kept, e)
		}
		if len(kept) == 0 {
			return fail(fmt.Errorf("preset %s: every entry was dropped; set credentials for at least one", name))
		}
		rp.Chain = kept
		b.resolved[name] = rp
		if name == primary {
			primarySpec = rp
		}
	}
	cfg := router.Config{Profiles: profiles, Groups: groups, DefaultGroup: primary,
		Required: primarySpec.Routing.Required, Revision: cat.Revision, Budget: o.Budget, Now: o.Now}
	if rs := primarySpec.Routing; rs.UsesAffinity() {
		cfg.SessionPolicy = router.Affinity{FailThreshold: rs.FailThreshold, ReprobeAfter: rs.ReprobeAfter}
	}
	cfg.FailoverOn = failoverOn(primarySpec.Routing)
	r, err := router.New(cfg)
	if err != nil {
		return fail(err)
	}
	b.router = r
	return b, nil
}

// failoverOn is router.DefaultFailoverOn plus the permanent errors the
// routing opts into. Without an opt-in, a content-filter refusal or an
// authentication failure ends the request.
func failoverOn(rs catalog.RoutingSpec) func(error) bool {
	return func(err error) bool {
		switch {
		case router.DefaultFailoverOn(err):
			return true
		case err == nil || errors.Is(err, context.Canceled):
			return false
		case rs.FailoverOnContentFilter && types.IsContentFilter(err):
			return true
		case rs.FailoverOnAuth && types.IsAuth(err):
			return true
		}
		return false
	}
}

// BuildFrom loads src, validates it, and builds from the result. It does not
// install the catalog: hosts that want Lookup to see its rows call
// catalog.Use as well.
func BuildFrom(ctx context.Context, src catalog.Source, primary string, also []string, o Options) (*Bundle, error) {
	cat, err := catalog.LoadSource(ctx, src)
	if err != nil {
		return nil, err
	}
	return Build(ctx, cat, primary, also, o)
}

// resolve accepts a preset name or a "provider/model" reference.
func resolve(cat *catalog.Catalog, name string) (catalog.ResolvedPreset, error) {
	if _, ok := cat.Presets[name]; ok {
		return cat.Resolve(name)
	}
	if p, m, ok := strings.Cut(name, "/"); ok && p != "" && m != "" {
		return cat.ResolveModel(p, m)
	}
	return cat.Resolve(name)
}

// retryInside places the deadline under an existing retry decorator.
func retryInside(p types.Provider, d time.Duration) types.Provider {
	if r, ok := p.(*retry.Provider); ok {
		return &retry.Provider{Inner: withAttemptTimeout(r.Inner, d), Config: r.Config}
	}
	return withAttemptTimeout(p, d)
}

// credentials returns the API key to pass and whether the entry has none.
func credentials(e catalog.ResolvedEntry, getenv func(string) string) (string, bool) {
	if e.APIKeyEnv != "" {
		key := getenv(e.APIKeyEnv)
		return key, key == ""
	}
	switch {
	case e.Provider == provider.Ollama:
		return "", false
	case e.Provider == provider.OpenAI && e.BaseURL != "":
		return "", false
	case e.Provider == provider.Google && (e.Vertex != nil || provider.VertexEnabled(getenv)):
		// Vertex uses Application Default Credentials; it needs a project.
		var v *provider.Vertex
		if e.Vertex != nil {
			v = &provider.Vertex{Project: e.Vertex.Project, Location: e.Vertex.Location}
		}
		return "", provider.ResolveVertex(v, getenv).Project == ""
	}
	for _, env := range provider.APIKeyEnv[e.Provider] {
		if getenv(env) != "" {
			return "", false
		}
	}
	return "", len(provider.APIKeyEnv[e.Provider]) > 0
}

func credentialHint(e catalog.ResolvedEntry) string {
	if e.APIKeyEnv != "" {
		return "set " + e.APIKeyEnv
	}
	if e.Provider == provider.Google && e.Vertex != nil {
		return "set " + provider.EnvCloudProject + " (vertex)"
	}
	return "set " + strings.Join(provider.APIKeyEnv[e.Provider], " or ")
}

func config(e catalog.ResolvedEntry, key string, o Options) provider.Config {
	cfg := provider.Config{Provider: e.Provider, Model: e.Model, APIKey: key, BaseURL: e.BaseURL,
		HTTPClient: o.HTTPClient, Options: e.Options.Clone(), ServerTools: append([]types.ServerTool(nil), e.ServerTools...), Getenv: o.Getenv}
	for _, l := range e.Dials {
		// An empty, non-nil list tells Build the layers are resolved, so the
		// model's dial defaults are not added twice.
		cfg.DialLayers = append(cfg.DialLayers, l.Clone())
	}
	if cfg.DialLayers == nil {
		cfg.DialLayers = []types.DialLayer{}
	}
	if e.Vertex != nil {
		cfg.Vertex = &provider.Vertex{Project: e.Vertex.Project, Location: e.Vertex.Location}
	}
	if pc := e.PromptCache; pc != nil && pc.Mode != catalog.PromptCacheOff {
		cfg.PromptCache = &provider.PromptCache{Mode: pc.Mode, TTL: pc.TTL, Tools: pc.Tools, System: pc.System,
			Conversation: pc.Conversation, Retention: pc.Retention, Key: pc.Key}
	}
	return cfg
}

func retryConfig(s *catalog.RetrySpec) retry.Config {
	c := retry.DefaultConfig()
	if s == nil {
		return c
	}
	if s.MaxAttempts > 0 {
		c.MaxAttempts = s.MaxAttempts
	}
	if s.BaseDelay > 0 {
		c.BaseDelay = time.Duration(s.BaseDelay)
	}
	if s.MaxDelay > 0 {
		c.MaxDelay = time.Duration(s.MaxDelay)
	}
	if s.Multiplier > 0 {
		c.Multiplier = s.Multiplier
	}
	if s.MaxRetryAfter > 0 {
		c.MaxRetryAfter = time.Duration(s.MaxRetryAfter)
	}
	return c
}

// Session returns a new routing session whose default group is the primary
// preset. Use one session per conversation.
func (b *Bundle) Session() types.Provider { return b.router.Session() }

// Provider implements types.Preset with a new session.
func (b *Bundle) Provider() types.Provider { return b.Session() }

// Router returns the router behind the bundle.
func (b *Bundle) Router() *router.Router { return b.router }

// Defaults implements types.Preset: the primary preset's agent defaults.
func (b *Bundle) Defaults() types.PresetDefaults {
	rp := b.resolved[b.primary]
	d := types.PresetDefaults{Name: rp.Name, CatalogRevision: rp.CatalogRevision, OutputMode: rp.OutputMode, LLMTimeout: rp.LLMTimeout}
	if rp.ToolChoice != nil {
		tc := *rp.ToolChoice
		d.ToolChoice = &tc
	}
	return d
}

// Resolved returns a built preset as resolved, after dropped entries were
// removed.
func (b *Bundle) Resolved(name string) (catalog.ResolvedPreset, bool) {
	rp, ok := b.resolved[name]
	return rp, ok
}

// Presets returns the built preset names, primary first.
func (b *Bundle) Presets() []string {
	out := []string{b.primary}
	for _, n := range b.router.Groups() {
		if n != b.primary {
			out = append(out, n)
		}
	}
	return out
}

// Warnings returns the resolution and build warnings.
func (b *Bundle) Warnings() []catalog.Issue { return append([]catalog.Issue(nil), b.warnings...) }

// ConfigHashes maps every built profile ID to its configuration hash.
func (b *Bundle) ConfigHashes() map[string]string {
	out := map[string]string{}
	for _, rp := range b.resolved {
		for _, e := range rp.Chain {
			out[e.ProfileID] = e.ConfigHash
		}
	}
	return out
}

// ConfigKey identifies a group's or a profile's complete configuration for a
// response cache (cache.Config.ConfigKey): a hash over the members'
// configuration hashes in order and the catalog revision. It is empty for an
// unknown name.
func (b *Bundle) ConfigKey(name string) string {
	hashes := b.ConfigHashes()
	ids := b.router.Group(name)
	if len(ids) == 0 {
		if _, ok := hashes[name]; !ok {
			return ""
		}
		ids = []string{name}
	}
	h := sha256.New()
	h.Write([]byte(b.revision))
	for _, id := range ids {
		h.Write([]byte{0})
		h.Write([]byte(id + "=" + hashes[id]))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Close closes every built adapter.
func (b *Bundle) Close() error { return b.router.Close() }
