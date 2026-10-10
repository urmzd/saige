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
		"capability":   caps,
		"media":        media,
		"server_tool":  tools,
		"tier":         {"frontier", "standard", "economy"},
		"structured":   {"", "native", "tool_call"},
		"policy":       {PolicySticky, PolicyAffinity},
		"inherit":      {"all", "none"},
		"output_mode":  {"auto", "native", "tool", "prompt"},
		"prompt_cache": {PromptCacheOff, PromptCacheMarkers, PromptCacheAutomatic},
		"options_tool": {"auto", "none"},
		"unset":        unsetNames(),
		"creativity":   {"deterministic", "focused", "balanced", "creative"},
		"mode":         {"off", "adaptive", "on"},
		"depth":        {"minimal", "low", "medium", "high", "max"},
		"surface":      {types.SurfaceChat, types.SurfaceResponses},
		"depth_change": {"", types.DepthChangePerRequest},
		"tool_mode":    {"auto", "none", "required", "named"},
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
	patch := t == reflect.TypeFor[ModelSpec]()
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		s := g.field(t, name, f.Type)
		if patch && name != "provider" && name != "prefix" {
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

func (g *schemaGen) field(owner reflect.Type, name string, t reflect.Type) map[string]any {
	e := enums()
	enum := func(key string) map[string]any { return map[string]any{"type": "string", "enum": e[key]} }
	array := func(items map[string]any) map[string]any { return map[string]any{"type": "array", "items": items} }
	switch {
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
