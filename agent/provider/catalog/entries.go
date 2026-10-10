package catalog

import (
	"slices"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// CodeUnknownEndpoint reports a reference to an endpoint the catalog does
// not declare, and CodeUnknownOffering one to an offering of a model it
// does not declare.
const (
	CodeUnknownEndpoint = "unknown_endpoint"
	CodeUnknownOffering = "unknown_offering"
)

// defaultKeyEnv lists the environment variables an adapter reads its key
// from by default, so an endpoint naming one of them adds nothing to an
// entry.
var defaultKeyEnv = []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GOOGLE_API_KEY", "GEMINI_API_KEY"}

// NormalizeRetention returns a prompt cache retention in its canonical
// spelling: "in-memory" is "in_memory".
func NormalizeRetention(r string) string {
	if r == "in-memory" {
		return "in_memory"
	}
	return r
}

// envRef reports the variable an "env:NAME" reference names.
func envRef(s string) (string, bool) { return strings.CutPrefix(s, "env:") }

// literal returns s unless it is an environment reference, which the
// adapter resolves from its own defaults.
func literal(s string) string {
	if _, ok := envRef(s); ok {
		return ""
	}
	return s
}

// entryTarget sets an entry's provider, model and endpoint from the form
// it is written in: an offering, a model on a named endpoint, or the
// legacy provider and model (with vertex, base_url or api_key_env).
//
//nolint:gocyclo // one branch per entry form, each with its own checks
func (c *Catalog) entryTarget(preset types.PresetName, path string, es EntrySpec, e *ResolvedEntry, v view, found *issues) {
	var ix *index
	if v.index != nil {
		ix = v.index
	}
	primary := func(vendor types.ProviderName) string {
		if ix != nil {
			if p, ok := ix.primary[vendor]; ok {
				return p
			}
		}
		return primaryEndpointV1(vendor)
	}
	knownEndpoint := func(name, p string) bool {
		if ix == nil {
			return true
		}
		if _, ok := ix.endpoints[name]; !ok {
			found.errorf(p, CodeUnknownEndpoint, "unknown endpoint %q", name)
			return false
		}
		return true
	}
	switch {
	case es.Offering != "":
		mk, ep, ok := ParseOfferingID(es.Offering)
		if !ok {
			found.errorf(path+".offering", CodeBadValue, "offering must be <vendor>/<model>@<endpoint>, got %q", es.Offering)
			return
		}
		vendor, prefix, _ := splitModelKey(mk)
		if es.Provider != "" && es.Provider != vendor {
			found.errorf(path+".provider", CodeBadValue, "provider %q contradicts offering %q", es.Provider, es.Offering)
		}
		if es.Endpoint != "" && es.Endpoint != ep {
			found.errorf(path+".endpoint", CodeBadValue, "endpoint %q contradicts offering %q", es.Endpoint, es.Offering)
		}
		e.Provider, e.Endpoint = vendor, ep
		if e.Model == "" {
			e.Model = prefix
		}
		if knownEndpoint(ep, path+".offering") && ix != nil {
			if _, ok := ix.models[mk]; !ok {
				found.errorf(path+".offering", CodeUnknownOffering, "offering %q names a model the catalog does not declare", es.Offering)
			}
		}
	case es.Endpoint != "":
		e.Endpoint = es.Endpoint
		if !knownEndpoint(es.Endpoint, path+".endpoint") {
			return
		}
		var serves []types.ProviderName
		if ix != nil {
			serves = ix.endpoints[es.Endpoint].Serves
		}
		if e.Provider == "" {
			switch {
			case len(serves) == 1:
				e.Provider = serves[0]
			default:
				e.Provider, _ = v.inferProvider(string(e.Model))
			}
		}
		if e.Provider == "" {
			found.errorf(path+".provider", CodeMissing, "provider is required: endpoint %q serves several vendors", es.Endpoint)
		} else if ix != nil && !slices.Contains(serves, e.Provider) {
			found.errorf(path+".endpoint", CodeBadValue, "endpoint %q does not serve %s", es.Endpoint, e.Provider)
		}
	default:
		switch {
		case es.Vertex != nil:
			e.Endpoint = EndpointGoogleVertex
			if ix != nil {
				for _, name := range sortedKeys(ix.endpoints) {
					if ep := ix.endpoints[name]; ep.Surface == types.SurfaceVertex && slices.Contains(ep.Serves, e.Provider) {
						e.Endpoint = name
						break
					}
				}
			}
		case es.BaseURL != "" || es.APIKeyEnv != "":
			id := es.ID
			if id == "" {
				id = modelKey(e.Provider, e.Model)
			}
			e.Endpoint = string(preset) + "/" + id
		default:
			e.Endpoint = primary(e.Provider)
		}
		return
	}
	if e.Endpoint != primary(e.Provider) {
		e.hashEndpoint = e.Endpoint
	}
}

// entryCaps resolves the capabilities an entry's model has on its
// endpoint, and the offering they come from. An endpoint the catalog does
// not declare (an entry's own base_url) serves like the vendor's primary.
func (v view) entryCaps(endpoint string, provider types.ProviderName, model string) (types.ModelCapabilities, string) {
	caps, _ := v.lookup(provider, model)
	if v.index == nil {
		return caps, offeringIDOf(caps)
	}
	if _, ok := v.index.endpoints[endpoint]; !ok || endpoint == v.index.primary[provider] {
		return caps, offeringIDOf(caps)
	}
	e, matched := v.match(provider, model)
	var off *types.Offering
	if matched {
		if r := v.index.offerings[endpoint][modelKey(e.Provider, e.Prefix)]; r != nil {
			off = &r.off
		}
	} else if b := v.index.baselines[endpoint][provider]; b != nil {
		off = &b.off
	}
	if off == nil {
		return caps, offeringIDOf(caps)
	}
	out := off.Capabilities().ForModel(model)
	out.Provider, out.Family, out.Known, out.Notes = caps.Provider, caps.Family, caps.Known, slices.Clone(caps.Notes)
	if matched {
		out.Notes = append(slices.Clone(off.Model.Notes), off.Notes...)
		if !caps.Known {
			out.Notes = append(out.Notes, "capabilities inferred from family prefix; register the exact model to mark this declaration known")
		}
	}
	return out, offeringIDOf(out)
}

func offeringIDOf(caps types.ModelCapabilities) string {
	if caps.Offering == nil || !caps.Offering.Model.Known && caps.Family == "" {
		return ""
	}
	return caps.Offering.ID
}

// applyEndpoint fills what an entry's catalog endpoint says about reaching
// it: the base URL, a key variable other than the adapter's default,
// Vertex placement, and the endpoint's own model identifier.
func (c *Catalog) applyEndpoint(e *ResolvedEntry, v view) {
	if v.index == nil {
		return
	}
	ep, ok := v.index.endpoints[e.Endpoint]
	if !ok {
		return
	}
	if t := ep.Transport; t != nil && e.BaseURL == "" {
		e.BaseURL = literal(t.BaseURL)
	}
	if a := ep.Auth; a != nil && e.APIKeyEnv == "" {
		if name, ok := envRef(a.Secret); ok && !slices.Contains(defaultKeyEnv, name) {
			e.APIKeyEnv = name
		}
	}
	if ep.Surface == types.SurfaceVertex && e.Vertex == nil {
		e.Vertex = &VertexSpec{}
		if l := ep.Location; l != nil {
			e.Vertex.Project, e.Vertex.Location = literal(l.Project), literal(l.Region)
		}
	}
	if id, ok := ep.ModelIDs[types.ModelID(e.Caps.Family)]; ok && id != "" {
		e.Model = id
	}
}
