package preset

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/urmzd/saige/agent/provider"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/ollama"
	"github.com/urmzd/saige/agent/types"
)

// ErrNoLocalModel means an Ollama server answered but has no chat model
// pulled, so an entry with local_fallback has nothing to serve.
var ErrNoLocalModel = errors.New("no chat model is pulled")

// WarnLocalModel is the warning code recorded when an entry with
// local_fallback serves another pulled model than the one it names.
const WarnLocalModel = "local_model_substituted"

// ListOllama is the default Options.ListLocal. For an Ollama entry it lists
// the models pulled on the server (GET /api/tags); the host is resolved as
// ProbeOllama resolves it. Every other provider returns nil.
func ListOllama(ctx context.Context, cfg provider.Config) ([]catalog.RemoteModel, error) {
	if cfg.Provider != provider.Ollama {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var opts []ollama.Option
	if cfg.HTTPClient != nil {
		opts = append(opts, ollama.WithHTTPClient(cfg.HTTPClient))
	}
	c, err := ollama.NewClient(ollama.Config{Host: ollamaHost(cfg)}, opts...)
	if err != nil {
		return nil, err
	}
	return c.ListModels(ctx)
}

// ollamaHost is the entry's base URL, then OLLAMA_HOST, then
// provider.DefaultOllamaHost, with a scheme.
func ollamaHost(cfg provider.Config) string {
	host := cfg.BaseURL
	if host == "" && cfg.Getenv != nil {
		host = cfg.Getenv("OLLAMA_HOST")
	}
	if host == "" {
		host = provider.DefaultOllamaHost
	}
	if !strings.Contains(host, "://") {
		host = "http://" + host
	}
	return host
}

// chooseLocalModel returns the model to serve for an entry that names want:
// want itself when it is pulled, else the first pulled model the catalog
// says calls tools, else the first pulled model that is not an embedding
// model. It returns "" when no chat model is pulled.
func chooseLocalModel(want string, pulled []catalog.RemoteModel) string {
	var chat []string
	for _, m := range pulled {
		if m.ID == want || m.ID == want+":latest" {
			return want
		}
		if !m.Embedding {
			chat = append(chat, m.ID)
		}
	}
	for _, id := range chat {
		if catalog.MustLookup(provider.Ollama, id).Supports(types.CapTools) {
			return id
		}
	}
	if len(chat) > 0 {
		return chat[0]
	}
	return ""
}

// noLocalModel is the error for a server with no chat model pulled. It
// names no host: a caller's ListLocal may list another host than the
// entry's configuration names.
func noLocalModel(want string) error {
	return fmt.Errorf("%w in Ollama: run `ollama pull %s`", ErrNoLocalModel, want)
}

// localKey identifies an Ollama entry by the server and model it names.
func localKey(baseURL, model string) string { return baseURL + "\x00" + model }

// pickLocalModels rewrites, in a copy of cat, every Ollama entry with
// local_fallback in the named presets whose model is not pulled, so that
// it names a pulled chat model and resolves against that model's own row.
// An entry whose server cannot be listed is left as written; the probe and
// the requests report that server. Entries whose server has no chat model
// are returned in missing, keyed by localKey, for Build to drop or fail.
func pickLocalModels(ctx context.Context, cat *catalog.Catalog, names []types.PresetName, o Options) (*catalog.Catalog, []catalog.Issue, map[string]error) {
	var (
		out      = cat
		warnings []catalog.Issue
		missing  = map[string]error{}
		listed   = map[string][]catalog.RemoteModel{}
		failed   = map[string]bool{}
	)
	for _, name := range names {
		decl := declaringPreset(out, name)
		if decl == "" {
			continue
		}
		for i, es := range out.Presets[decl].Chain {
			if es.Provider != provider.Ollama || !es.LocalFallback {
				continue
			}
			cfg := provider.Config{Provider: es.Provider, BaseURL: es.BaseURL, HTTPClient: o.HTTPClient, Getenv: o.Getenv}
			host := ollamaHost(cfg)
			if failed[host] {
				continue
			}
			pulled, ok := listed[host]
			if !ok {
				var err error
				if pulled, err = o.ListLocal(ctx, cfg); err != nil {
					failed[host] = true
					continue
				}
				listed[host] = pulled
			}
			pick := types.ModelID(chooseLocalModel(string(es.Model), pulled))
			switch pick {
			case es.Model:
			case "":
				missing[localKey(es.BaseURL, string(es.Model))] = noLocalModel(string(es.Model))
			default:
				if out == cat {
					out = cat.Clone()
				}
				spec := out.Presets[decl]
				chain := append([]catalog.EntrySpec(nil), spec.Chain...)
				id := chain[i].ID
				if id == "" {
					// Keep the profile ID the entry had under its own model.
					id = string(es.Provider) + "/" + string(es.Model)
				}
				chain[i].ID, chain[i].Model = id, pick
				spec.Chain = chain
				out.Presets[decl] = spec
				warnings = append(warnings, catalog.Issue{Path: "presets." + string(decl), Code: WarnLocalModel, Severity: catalog.SeverityWarning,
					Message: fmt.Sprintf("entry %s: %s is not pulled in Ollama; serving %s", id, es.Model, pick)})
			}
		}
	}
	return out, warnings, missing
}

// declaringPreset follows extends from name to the preset that declares
// the chain name runs, or returns "" when there is none.
func declaringPreset(cat *catalog.Catalog, name types.PresetName) types.PresetName {
	seen := map[types.PresetName]bool{}
	for name != "" && !seen[name] {
		seen[name] = true
		p, ok := cat.Presets[name]
		if !ok {
			return ""
		}
		if p.Chain != nil {
			return name
		}
		name = p.Extends
	}
	return ""
}
