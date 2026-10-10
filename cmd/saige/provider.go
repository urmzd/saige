package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/urmzd/saige/agent/provider"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/agent/provider/preset"
	"github.com/urmzd/saige/agent/types"
)

var defaultEmbedModels = map[string]string{
	providerAnthropic: "",
	providerOpenAI:    "text-embedding-3-small",
	providerGoogle:    "text-embedding-004",
	providerVertex:    "text-embedding-004",
	providerOllama:    "nomic-embed-text",
}

// commonFlags holds flags shared by chat and ask commands.
type commonFlags struct {
	provider      *string
	model         *string
	preset        *string
	catalogs      *[]string
	system        *string
	ollamaHost    *string
	baseURL       *string
	embedProvider *string
	embedModel    *string
	ragDB         *string
	kgDB          *string
	format        *string
}

// persistentFlagVars holds the package-level vars bound to PersistentFlags on the root command.
var persistentFlagVars = &commonFlags{
	provider:      new(string),
	model:         new(string),
	preset:        new(string),
	catalogs:      new([]string),
	system:        new(string),
	ollamaHost:    new(string),
	baseURL:       new(string),
	embedProvider: new(string),
	embedModel:    new(string),
	ragDB:         new(string),
	kgDB:          new(string),
	format:        new(string),
}

// addPersistentFlags registers provider and connection flags on the root command's PersistentFlags.
func addPersistentFlags(cmd *cobra.Command) {
	pf := cmd.PersistentFlags()
	pf.StringVar(persistentFlagVars.provider, "provider", envOr("SAIGE_PROVIDER", ""), "LLM provider (anthropic|openai|google|vertex|ollama)")
	pf.StringVar(persistentFlagVars.model, "model", "", "Model name; builds a one-entry chain from the catalog's model defaults")
	pf.StringVar(persistentFlagVars.preset, "preset", envOr("SAIGE_PRESET", ""), "Catalog preset to run (see saige catalog show)")
	pf.StringArrayVar(persistentFlagVars.catalogs, "catalog", nil, "Catalog layer: a path, file:// or https:// URL (repeatable; also $SAIGE_CATALOG)")
	pf.StringVar(persistentFlagVars.system, "system", "You are a helpful assistant.", "System prompt")
	pf.StringVar(persistentFlagVars.ollamaHost, "ollama-host", envOr("OLLAMA_HOST", "http://localhost:11434"), "Ollama host URL")
	pf.StringVar(persistentFlagVars.baseURL, "base-url", "", "API base URL for the selected provider's entries, such as an OpenAI-compatible server")
	pf.StringVar(persistentFlagVars.embedProvider, "embed-provider", envOr("SAIGE_EMBED_PROVIDER", ""), "Embedding provider (openai|google|ollama); defaults to the LLM provider")
	pf.StringVar(persistentFlagVars.embedModel, "embed-model", "", "Embedding model name (embed-provider-specific default)")
	pf.StringVar(persistentFlagVars.ragDB, "rag-db", envOr("SAIGE_RAG_DB", ""), "Postgres DSN for RAG tools")
	pf.StringVar(persistentFlagVars.kgDB, "kg-db", envOr("SAIGE_KG_DB", ""), "Postgres DSN for KG tools")
	pf.StringVar(persistentFlagVars.format, "format", "human", "Output format: json|human")
}

// isJSON returns true when the user requested JSON output via --format json.
func (cf *commonFlags) isJSON() bool {
	return *cf.format == formatJSON
}

// resolvedProvider returns the provider name, falling back to env then auto-detect.
func (cf *commonFlags) resolvedProvider() string {
	if *cf.provider != "" {
		return *cf.provider
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return providerAnthropic
	}
	if os.Getenv("OPENAI_API_KEY") != "" {
		return providerOpenAI
	}
	if provider.VertexEnabled(os.Getenv) {
		return providerVertex
	}
	if os.Getenv("GOOGLE_API_KEY") != "" {
		return providerGoogle
	}
	return providerOllama
}

// resolvedModel returns the model name. Without --model it is the first
// entry of the catalog preset named after the provider, so the default
// model is catalog data rather than a table in this command.
func (cf *commonFlags) resolvedModel() string {
	if *cf.model != "" {
		return *cf.model
	}
	cat, err := cf.catalog()
	if err != nil {
		cat = catalog.Default()
	}
	if p, ok := cat.Presets[cf.resolvedProvider()]; ok && len(p.Chain) > 0 {
		return p.Chain[0].Model
	}
	return ""
}

// resolvedEmbedProvider returns the embedding provider name, falling back to
// the LLM provider when --embed-provider (or SAIGE_EMBED_PROVIDER) is unset.
func (cf *commonFlags) resolvedEmbedProvider() string {
	if *cf.embedProvider != "" {
		return *cf.embedProvider
	}
	return cf.resolvedProvider()
}

// resolvedEmbedModel returns the embed model name, falling back to the embed provider default.
func (cf *commonFlags) resolvedEmbedModel() string {
	if *cf.embedModel != "" {
		return *cf.embedModel
	}
	return defaultEmbedModels[cf.resolvedEmbedProvider()]
}

// loadedCatalog caches the merged layers for one set of flags.
var loadedCatalog struct {
	sync.Mutex
	key    string
	done   bool
	cat    *catalog.Catalog
	layers []layer
	err    error
}

// catalog loads, validates and installs the merged catalog layers, once per
// set of flags. Installing makes `saige models` and every adapter see the
// rows the layers declare.
func (cf *commonFlags) catalog() (*catalog.Catalog, error) {
	cat, _, err := cf.catalogLayers()
	return cat, err
}

func (cf *commonFlags) catalogLayers() (*catalog.Catalog, []layer, error) {
	loadedCatalog.Lock()
	defer loadedCatalog.Unlock()
	key := strings.Join(*cf.catalogs, "\x00") + "\x01" + os.Getenv(envCatalog)
	if loadedCatalog.done && loadedCatalog.key == key {
		return loadedCatalog.cat, loadedCatalog.layers, loadedCatalog.err
	}
	layers, err := discoverLayers(*cf.catalogs, os.Getenv)
	var cat *catalog.Catalog
	if err == nil {
		cat, err = mergeLayers(context.Background(), layers)
	}
	if err == nil {
		_, err = catalog.Install(cat, layerSource(layers))
	}
	loadedCatalog.key, loadedCatalog.done = key, true
	loadedCatalog.cat, loadedCatalog.layers, loadedCatalog.err = cat, layers, err
	return cat, layers, err
}

// resolveProvider builds the provider chain the flags select and returns a
// routing session over it. See resolveBundle.
func resolveProvider(ctx context.Context, cf *commonFlags, verbose bool) (types.Provider, error) {
	b, err := resolveBundle(ctx, cf, verbose)
	if err != nil {
		return nil, err
	}
	return b.Session(), nil
}

// resolveBundle builds the preset the flags select. Precedence: --preset,
// then --model (with --provider or the provider inferred from the model),
// then --provider alone (the catalog preset of that name), then the
// catalog's default_preset narrowed to its first usable entry. Each chain
// entry is built with its own options and wrapped in its own retry
// decorator, as the preset declares.
func resolveBundle(ctx context.Context, cf *commonFlags, verbose bool) (*preset.Bundle, error) {
	cat, err := cf.catalog()
	if err != nil {
		return nil, err
	}
	name, cat, defaulted, err := cf.selectPreset(cat)
	if err != nil {
		return nil, err
	}
	opts := cf.presetOptions(verbose)
	if defaulted {
		if cat, err = cf.narrowDefault(ctx, cat, name, opts); err != nil {
			return nil, err
		}
		if cat, err = cf.applyBaseURL(cat, name); err != nil {
			return nil, err
		}
	}
	return preset.Build(ctx, cat, name, nil, opts)
}

// presetOptions are the build options of every CLI preset: the CLI's
// adapter factory, and a reachability probe that honors --ollama-host.
func (cf *commonFlags) presetOptions(verbose bool) preset.Options {
	return preset.Options{
		Factory: cliFactory(cf, verbose),
		Probe: func(ctx context.Context, cfg provider.Config) error {
			if cfg.Provider == providerOllama && cfg.BaseURL == "" {
				cfg.BaseURL = *cf.ollamaHost
			}
			return preset.ProbeOllama(ctx, cfg)
		},
		ListLocal: func(ctx context.Context, cfg provider.Config) ([]catalog.RemoteModel, error) {
			if cfg.Provider == providerOllama && cfg.BaseURL == "" {
				cfg.BaseURL = *cf.ollamaHost
			}
			return preset.ListOllama(ctx, cfg)
		},
	}
}

// cliPresetName names the preset built from --model.
const cliPresetName = "cli"

// selectPreset applies the flag precedence and returns the preset to build.
// --model builds a one-entry preset through the same resolution as any
// other. --base-url is applied to the selected provider's entries, or
// rejected when that is ambiguous; it is never dropped. defaulted is true
// when no flag chose the preset and the catalog's default_preset applies;
// the caller then narrows it to one vendor and applies --base-url.
func (cf *commonFlags) selectPreset(cat *catalog.Catalog) (name string, out *catalog.Catalog, defaulted bool, err error) {
	switch {
	case *cf.preset != "":
		if _, ok := cat.Presets[*cf.preset]; !ok {
			return "", nil, false, fmt.Errorf("unknown preset %q (available: %s)", *cf.preset, strings.Join(cat.PresetNames(), ", "))
		}
		out, err = cf.applyBaseURL(cat, *cf.preset)
		return *cf.preset, out, false, err
	case *cf.model != "":
		prov := *cf.provider
		if prov == "" {
			if p, _, err := provider.Infer(*cf.model); err == nil {
				prov = p
			} else {
				prov = cf.resolvedProvider()
			}
		}
		entry := catalog.EntrySpec{ID: prov + "/" + *cf.model, Provider: prov, Model: *cf.model, BaseURL: *cf.baseURL}
		if prov == providerVertex {
			entry.Provider, entry.Vertex = providerGoogle, &catalog.VertexSpec{}
		}
		if prov == providerOllama {
			// A local server that is not running should fail at once, not
			// after a round of backoff.
			entry.Retry = &catalog.RetrySpec{Disable: true}
		}
		out := cat.Clone()
		if out.Presets == nil {
			out.Presets = map[string]catalog.PresetSpec{}
		}
		out.Presets[cliPresetName] = catalog.PresetSpec{Description: "built from --model", Chain: []catalog.EntrySpec{entry}}
		return cliPresetName, out, false, nil
	case *cf.provider != "":
		if _, ok := cat.Presets[*cf.provider]; !ok {
			return "", nil, false, fmt.Errorf("no catalog preset named %q: pass --model or --preset", *cf.provider)
		}
		out, err = cf.applyBaseURL(cat, *cf.provider)
		return *cf.provider, out, false, err
	case cat.DefaultPreset != "":
		return cat.DefaultPreset, cat, true, nil
	}
	return "", nil, false, errors.New("the catalog declares no default_preset: pass --preset or --model")
}

// presetChain returns the chain a preset runs, following extends to the
// nearest preset that declares one.
func presetChain(cat *catalog.Catalog, name string) []catalog.EntrySpec {
	seen := map[string]bool{}
	for name != "" && !seen[name] {
		seen[name] = true
		p, ok := cat.Presets[name]
		if !ok {
			return nil
		}
		if p.Chain != nil {
			return p.Chain
		}
		name = p.Extends
	}
	return nil
}

// withChain returns a copy of cat in which preset name runs chain. The
// preset keeps its name and every other key, so profile IDs and route
// events are unchanged.
func withChain(cat *catalog.Catalog, name string, chain []catalog.EntrySpec) *catalog.Catalog {
	out := cat.Clone()
	p := out.Presets[name]
	p.Chain = chain
	out.Presets[name] = p
	return out
}

// applyBaseURL sets --base-url on the entries of the selected provider:
// --provider when given, otherwise the one provider the chain uses. A chain
// that spans several vendors without --provider is an error, since the
// flag cannot apply to all of them.
func (cf *commonFlags) applyBaseURL(cat *catalog.Catalog, name string) (*catalog.Catalog, error) {
	if *cf.baseURL == "" {
		return cat, nil
	}
	chain := presetChain(cat, name)
	var vendors []string
	for _, e := range chain {
		if !slices.Contains(vendors, e.Provider) {
			vendors = append(vendors, e.Provider)
		}
	}
	target := *cf.provider
	switch {
	case target != "" && !slices.Contains(vendors, target):
		return nil, fmt.Errorf("--base-url: preset %q has no %s entry", name, target)
	case target == "" && len(vendors) != 1:
		return nil, fmt.Errorf("--base-url is ambiguous: preset %q spans %s; pass --provider to choose one, or set base_url on the entries in a catalog",
			name, strings.Join(vendors, ", "))
	case target == "":
		target = vendors[0]
	}
	next := make([]catalog.EntrySpec, len(chain))
	for i, e := range chain {
		if e.Provider == target {
			e.BaseURL = *cf.baseURL
		}
		next[i] = e
	}
	return withChain(cat, name, next), nil
}

// narrowDefault turns the default preset into a one-entry chain: its first
// entry that has credentials, or for a local Ollama entry, a server that
// answers. Failover across vendors is opt-in: name a preset with --preset.
func (cf *commonFlags) narrowDefault(ctx context.Context, cat *catalog.Catalog, name string, opts preset.Options) (*catalog.Catalog, error) {
	rp, err := cat.Resolve(name)
	if err != nil {
		return nil, fmt.Errorf("preset %s: %w", name, err)
	}
	chain := presetChain(cat, name)
	if len(chain) != len(rp.Chain) {
		return nil, fmt.Errorf("preset %s: cannot match its chain to the resolved entries", name)
	}
	var noLocal error
	for i, e := range rp.Chain {
		err := preset.Available(ctx, e, opts)
		if err == nil {
			return withChain(cat, name, chain[i:i+1]), nil
		}
		if errors.Is(err, preset.ErrNoLocalModel) {
			noLocal = err
		}
	}
	return nil, noProviderError(rp.Chain, *cf.ollamaHost, noLocal)
}

// noProviderError says how to make one of the default entries usable.
// noLocal is the Ollama entry's ErrNoLocalModel when the server answered but
// has no chat model, so the advice is to pull one rather than to start it.
func noProviderError(chain []catalog.ResolvedEntry, ollamaHost string, noLocal error) error {
	var keys []string
	local := false
	for _, e := range chain {
		envs := provider.APIKeyEnv[e.Provider]
		if e.APIKeyEnv != "" {
			envs = []string{e.APIKeyEnv}
		}
		if e.Provider == providerOllama && e.APIKeyEnv == "" {
			local = true
		}
		for _, k := range envs {
			if !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
	}
	var ways []string
	if len(keys) > 0 {
		ways = append(ways, "set "+strings.Join(keys, " or "))
	}
	switch {
	case noLocal != nil:
		ways = append(ways, noLocal.Error())
	case local:
		ways = append(ways, fmt.Sprintf("start ollama (ollama serve) at %s", ollamaHost))
	}
	return fmt.Errorf("no model provider is available: %s", strings.Join(ways, ", or "))
}

// cliFactory builds adapters through provider.Build. For Ollama it applies
// --ollama-host when the entry names no host, sets the embedding model, and
// logs requests only with --verbose.
func cliFactory(cf *commonFlags, verbose bool) func(context.Context, provider.Config) (types.Provider, error) {
	return func(ctx context.Context, cfg provider.Config) (types.Provider, error) {
		if cfg.Provider != providerOllama {
			return provider.Build(ctx, cfg)
		}
		if cfg.BaseURL == "" {
			cfg.BaseURL = *cf.ollamaHost
		}
		p, err := provider.Build(ctx, cfg)
		if err != nil {
			return nil, err
		}
		if a, ok := p.(*ollama.Adapter); ok {
			embedModel := defaultEmbedModels[providerOllama]
			if cf.resolvedEmbedProvider() == providerOllama {
				embedModel = cf.resolvedEmbedModel()
			}
			a.Client.EmbeddingModel = embedModel
			if verbose {
				a.Client.Logger = log.Default()
			}
		}
		return p, nil
	}
}

// buildProvider creates the bare adapter for --provider and --model, without
// a retry decorator, for callers that add their own.
func buildProvider(ctx context.Context, cf *commonFlags, verbose bool) (types.Provider, error) {
	if _, err := cf.catalog(); err != nil {
		return nil, err
	}
	name := cf.resolvedProvider()
	cfg := provider.Config{Provider: name, Model: cf.resolvedModel(), BaseURL: *cf.baseURL}
	switch name {
	case providerVertex:
		cfg.Provider, cfg.Vertex = providerGoogle, &provider.Vertex{}
	case providerOllama, providerAnthropic, providerGoogle, providerOpenAI:
	default:
		return nil, fmt.Errorf("unknown provider: %s", name)
	}
	return cliFactory(cf, verbose)(ctx, cfg)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
