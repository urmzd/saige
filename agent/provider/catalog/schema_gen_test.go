package catalog

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// TestSchemaGolden keeps catalog.schema.json in step with the Go structs:
// the schema is generated from the json tags, so a new field shows up as a
// diff to review (go test -run TestSchemaGolden -update).
func TestSchemaGolden(t *testing.T) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(generateSchema()); err != nil {
		t.Fatal(err)
	}
	if *update {
		writeRepoFile(t, "catalog.schema.json", b.Bytes())
		return
	}
	if !bytes.Equal(b.Bytes(), Schema()) {
		t.Fatal("catalog.schema.json is stale; run go test -run TestSchemaGolden -update")
	}
}

func generateSchema() map[string]any {
	g := &schemaGen{defs: map[string]any{}}
	root := g.object(reflect.TypeFor[Catalog]())
	root["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	root["$id"] = "https://raw.githubusercontent.com/urmzd/saige/main/agent/provider/catalog/catalog.schema.json"
	root["title"] = "saige model catalog"
	root["$defs"] = g.defs
	return root
}

type schemaGen struct{ defs map[string]any }

// enums lists the closed vocabularies by field path suffix.
func enums() map[string][]string {
	var caps, media, tools []string
	for _, c := range types.KnownCapabilities() {
		caps = append(caps, string(c))
	}
	for _, m := range types.KnownMediaTypes() {
		media = append(media, string(m))
	}
	for _, k := range types.KnownServerToolKinds() {
		tools = append(tools, string(k))
	}
	return map[string][]string{
		"capability":       caps,
		"media":            media,
		"server_tool":      tools,
		"tier":             {"frontier", "standard", "economy"},
		"structured":       {"", "native", "tool_call"},
		"policy":           {PolicySticky, PolicyAffinity},
		"inherit":          {"all", "none"},
		"output_mode":      {"auto", "native", "tool", "prompt"},
		"prompt_cache":     {PromptCacheOff, PromptCacheMarkers, PromptCacheAutomatic},
		"options_tool":     {"auto", "none"},
		"unset":            unsetNames(),
		"creativity":       {"deterministic", "focused", "balanced", "creative"},
		"mode":             {"off", "adaptive", "on"},
		"depth":            {"minimal", "low", "medium", "high", "max"},
		"surface":          {types.SurfaceChat, types.SurfaceResponses},
		"depth_change":     {"", types.DepthChangePerRequest},
		"tool_mode":        {"auto", "none", "required", "named"},
		"endpoint_surface": KnownSurfaces(),
		"modality":         modalityNames(),
		"param":            paramNames(),
		"service_tier":     {"priority", "flex", "batch"},
		"source":           {"inline", "uri", "file"},
		"auth":             {"api_key", "adc", "none"},
		"tool_result":      {"", "inline", "follow_up_user", "none"},
		"transport":        {"", "batch"},
		"param_type":       {"number", "integer", "boolean", "enum", "string_list"},
		"compaction": {"none", "sliding_window", "summarize", "clear_tool_results",
			"keep_recent", "summary", "relevant_plus_summary", "chain"},
	}
}

func (g *schemaGen) ref(t reflect.Type) map[string]any {
	name := t.Name()
	if _, ok := g.defs[name]; !ok {
		g.defs[name] = nil // reserve against recursion
		g.defs[name] = g.object(t)
	}
	return map[string]any{"$ref": "#/$defs/" + name}
}

func (g *schemaGen) object(t reflect.Type) map[string]any {
	props := map[string]any{}
	var required []string
	patch := t == reflect.TypeFor[ModelSpec]() || t == reflect.TypeFor[OfferingSpec]() || t == reflect.TypeFor[EndpointSpec]()
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		s := g.field(t, name, f.Type)
		if patch && name != "model" && name != "endpoint" && name != "surface" {
			s = map[string]any{"anyOf": []any{s, map[string]any{"type": "null"}}}
		}
		props[name] = s
		if !strings.Contains(opts, "omit") && name != "$schema" {
			required = append(required, name)
		}
	}
	out := map[string]any{"type": "object", "additionalProperties": false, "properties": props}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func modalityNames() []string {
	var out []string
	for _, m := range types.KnownModalities() {
		out = append(out, string(m))
	}
	return out
}

func paramNames() []string {
	var out []string
	for _, p := range knownParams() {
		out = append(out, string(p))
	}
	return out
}

func (g *schemaGen) field(owner reflect.Type, name string, t reflect.Type) map[string]any {
	e := enums()
	enum := func(key string) map[string]any { return map[string]any{"type": "string", "enum": e[key]} }
	array := func(items map[string]any) map[string]any { return map[string]any{"type": "array", "items": items} }
	nullable := func(s map[string]any) map[string]any {
		return map[string]any{"anyOf": []any{s, map[string]any{"type": "null"}}}
	}
	keyed := func(key string, elem reflect.Type) map[string]any {
		return map[string]any{"type": "object", "propertyNames": enum(key), "additionalProperties": nullable(g.ref(elem))}
	}
	switch {
	case t == reflect.TypeFor[[]types.Modality]():
		return array(enum("modality"))
	case t == reflect.TypeFor[[]types.SourceKind]():
		return array(enum("source"))
	case t == reflect.TypeFor[map[types.ParamName]*ParamSpec]():
		return keyed("param", reflect.TypeFor[ParamSpec]())
	case t == reflect.TypeFor[map[types.Modality]*ModalityLimitSpec]():
		return keyed("modality", reflect.TypeFor[ModalityLimitSpec]())
	case t == reflect.TypeFor[map[types.ServiceTier]*TierSpec]():
		return keyed("service_tier", reflect.TypeFor[TierSpec]())
	case t == reflect.TypeFor[map[types.Modality]*types.ModalityRate]():
		return keyed("modality", reflect.TypeFor[types.ModalityRate]())
	case t == reflect.TypeFor[map[string]*types.Constraint]():
		return map[string]any{"type": "object", "additionalProperties": nullable(g.ref(reflect.TypeFor[types.Constraint]()))}
	case t == reflect.TypeFor[map[types.Modality]string]():
		return map[string]any{"type": "object", "propertyNames": enum("modality"), "additionalProperties": enum("tool_result")}
	case t == reflect.TypeFor[[]types.ParamName]():
		return array(map[string]any{"type": "string"})
	case owner == reflect.TypeFor[EndpointSpec]() && name == "surface":
		return map[string]any{"type": "string", "description": "one of " + strings.Join(e["endpoint_surface"], ", ") + ", or a custom surface"}
	case owner == reflect.TypeFor[AuthSpec]() && name == "type":
		return enum("auth")
	case owner == reflect.TypeFor[AuthSpec]() && name == "secret":
		return map[string]any{"type": "string", "pattern": "^(env:|op://|file:).+", "description": "a secret reference, never the secret"}
	case owner == reflect.TypeFor[TierSpec]() && name == "transport":
		return enum("transport")
	case owner == reflect.TypeFor[ParamSpec]() && name == "type":
		return enum("param_type")
	case owner == reflect.TypeFor[Catalog]() && (name == "models" || name == "model_templates"):
		return map[string]any{"type": "object", "additionalProperties": nullable(g.ref(reflect.TypeFor[ModelSpec]()))}
	case owner == reflect.TypeFor[Catalog]() && name == "offering_templates":
		return map[string]any{"type": "object", "additionalProperties": nullable(g.ref(reflect.TypeFor[OfferingSpec]()))}
	case owner == reflect.TypeFor[Catalog]() && name == "endpoints":
		return map[string]any{"type": "object", "additionalProperties": nullable(g.ref(reflect.TypeFor[EndpointSpec]()))}
	case t == reflect.TypeFor[[]types.Capability]():
		return array(enum("capability"))
	case t == reflect.TypeFor[[]types.MediaType]():
		return array(enum("media"))
	case t == reflect.TypeFor[[]types.ServerToolKind]():
		return array(enum("server_tool"))
	case t == reflect.TypeFor[types.ServerToolKind]():
		return enum("server_tool")
	case t == reflect.TypeFor[map[types.ServerToolKind]Fee]():
		return map[string]any{"type": "object", "propertyNames": enum("server_tool"), "additionalProperties": g.ref(reflect.TypeFor[Fee]())}
	case t == reflect.TypeFor[Tier]():
		return enum("tier")
	case t == reflect.TypeFor[*types.StructuredOutputMode]():
		return enum("structured")
	case t == reflect.TypeFor[Duration]():
		return map[string]any{"type": "string", "description": "Go duration, for example \"500ms\" or \"2m\""}
	case owner == reflect.TypeFor[RoutingSpec]() && name == "policy":
		return enum("policy")
	case owner == reflect.TypeFor[EntrySpec]() && name == "inherit":
		return enum("inherit")
	case owner == reflect.TypeFor[EntrySpec]() && name == "unset":
		return array(enum("unset"))
	case owner == reflect.TypeFor[PresetSpec]() && name == "output_mode":
		return enum("output_mode")
	case owner == reflect.TypeFor[CompactionSpec]() && name == "strategy":
		return enum("compaction")
	case owner == reflect.TypeFor[PresetSpec]() && name == "tool_choice":
		return map[string]any{"type": "string", "pattern": "^(auto|none|required|named:.+)$"}
	case owner == reflect.TypeFor[OptionsSpec]() && name == "tool_choice":
		return enum("options_tool")
	case owner == reflect.TypeFor[PromptCacheSpec]() && name == "mode":
		return enum("prompt_cache")
	case t == reflect.TypeFor[*types.Creativity]():
		return enum("creativity")
	case t == reflect.TypeFor[types.ReasoningMode]():
		return enum("mode")
	case t == reflect.TypeFor[types.Depth]():
		return enum("depth")
	case t == reflect.TypeFor[map[types.Creativity]OptionsSpec]():
		return map[string]any{"type": "object", "propertyNames": enum("creativity"), "additionalProperties": g.ref(reflect.TypeFor[OptionsSpec]())}
	case t == reflect.TypeFor[map[types.Depth]OptionsSpec]():
		return map[string]any{"type": "object", "propertyNames": enum("depth"), "additionalProperties": g.ref(reflect.TypeFor[OptionsSpec]())}
	case owner == reflect.TypeFor[ReasoningDialSpec]() && name == "with_tools":
		return map[string]any{"type": "object", "propertyNames": enum("surface"), "additionalProperties": g.ref(reflect.TypeFor[OptionsSpec]())}
	case owner == reflect.TypeFor[ReasoningDialSpec]() && name == "depth_change":
		return enum("depth_change")
	case owner == reflect.TypeFor[types.ToolChoice]() && name == "mode":
		return enum("tool_mode")
	case owner == reflect.TypeFor[Catalog]() && name == "presets":
		return map[string]any{"type": "object", "additionalProperties": map[string]any{
			"anyOf": []any{g.ref(reflect.TypeFor[PresetSpec]()), map[string]any{"type": "null"}}}}
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		return g.ref(t)
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": g.field(owner, name, t.Elem())}
	case reflect.Slice:
		return array(g.field(owner, name, t.Elem()))
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int64:
		return map[string]any{"type": "integer"}
	case reflect.Float64:
		return map[string]any{"type": "number"}
	}
	return map[string]any{}
}
