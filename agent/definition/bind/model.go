package bind

import (
	"context"
	"fmt"
	"strings"

	"github.com/urmzd/saige/agent/definition"
	"github.com/urmzd/saige/agent/provider"
	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/provider/preset"
	"github.com/urmzd/saige/agent/types"
)

// vertexProvider is the provider prefix that selects Google models served
// through Vertex AI, as the CLI's --provider vertex does.
const vertexProvider = "vertex"

// entry builds a one-model chain entry from provider/model.
func entry(ref string) (catalog.EntrySpec, error) {
	prov, model, ok := strings.Cut(ref, "/")
	if !ok || prov == "" || model == "" {
		return catalog.EntrySpec{}, fmt.Errorf("model %q must be provider/model", ref)
	}
	e := catalog.EntrySpec{ID: ref, Provider: types.ProviderName(prov), Model: types.ModelID(model)}
	if prov == vertexProvider {
		e.Provider, e.Vertex = "google", &catalog.VertexSpec{}
	}
	return e, nil
}

// presetChain returns the chain a preset runs, following extends to the
// nearest preset that declares one.
func presetChain(cat *catalog.Catalog, name types.PresetName) []catalog.EntrySpec {
	seen := map[types.PresetName]bool{}
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

// modelCatalog returns the catalog and preset name that serve m: the
// preset itself, or a preset added to a copy of the catalog for a
// provider/model or a fallback chain.
func modelCatalog(cat *catalog.Catalog, owner string, m *definition.ModelRef) (*catalog.Catalog, types.PresetName, error) {
	if len(m.Fallback) == 0 && (m.IsPreset() || !strings.HasPrefix(m.Use, vertexProvider+"/")) {
		return cat, types.PresetName(m.Use), nil
	}
	var spec catalog.PresetSpec
	if m.IsPreset() {
		chain := presetChain(cat, types.PresetName(m.Use))
		if chain == nil {
			return nil, "", fmt.Errorf("unknown preset %q", m.Use)
		}
		spec = catalog.PresetSpec{Extends: types.PresetName(m.Use), Chain: append([]catalog.EntrySpec(nil), chain...)}
	} else {
		e, err := entry(m.Use)
		if err != nil {
			return nil, "", err
		}
		spec.Chain = []catalog.EntrySpec{e}
	}
	for _, f := range m.Fallback {
		e, err := entry(f)
		if err != nil {
			return nil, "", err
		}
		// A fallback without credentials is dropped with a warning
		// rather than failing the bind: it is a spare, not the model.
		e.Optional = true
		spec.Chain = append(spec.Chain, e)
	}
	spec.Description = "model of agent definition " + owner
	name := types.PresetName("agent-" + owner)
	out := cat.Clone()
	if out.Presets == nil {
		out.Presets = map[types.PresetName]catalog.PresetSpec{}
	}
	out.Presets[name] = spec
	return out, name, nil
}

// buildModel builds the preset bundle a definition's model names.
func buildModel(ctx context.Context, env *Env, owner string, m *definition.ModelRef) (types.Preset, error) {
	cat, name, err := modelCatalog(env.catalog(), owner, m)
	if err != nil {
		return nil, fmt.Errorf("agent %s: model: %w", owner, err)
	}
	b, err := preset.Build(ctx, cat, name, nil, env.PresetOptions)
	if err != nil {
		return nil, fmt.Errorf("agent %s: model %s: %w", owner, m.Use, err)
	}
	return b, nil
}

// ModelCheck returns a definition.Checks.Model that resolves a reference
// against cat without building anything, so a registry rejects a model the
// catalog does not know at load time.
func ModelCheck(cat *catalog.Catalog) func(string) error {
	return func(ref string) error {
		if !strings.Contains(ref, "/") {
			if _, ok := cat.Presets[types.PresetName(ref)]; !ok {
				names := make([]string, 0, len(cat.Presets))
				for _, n := range cat.PresetNames() {
					names = append(names, string(n))
				}
				return fmt.Errorf("unknown preset %q (available: %s)", ref, strings.Join(names, ", "))
			}
			_, err := cat.Resolve(types.PresetName(ref))
			return err
		}
		e, err := entry(ref)
		if err != nil {
			return err
		}
		switch e.Provider {
		case provider.Anthropic, provider.OpenAI, provider.Google, provider.Ollama:
		default:
			return fmt.Errorf("%w %q in %s (want anthropic, openai, google, vertex or ollama)", provider.ErrUnknownProvider, e.Provider, ref)
		}
		_, err = cat.ResolveModel(e.Provider, e.Model)
		return err
	}
}
