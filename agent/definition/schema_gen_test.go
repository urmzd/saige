package definition

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/provider/catalog"
	"github.com/urmzd/saige/agent/types"
)

var update = flag.Bool("update", false, "rewrite golden files")

// TestSchemaGolden keeps definition.schema.json in step with the Go types:
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
		if err := os.WriteFile("definition.schema.json", b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if !bytes.Equal(b.Bytes(), Schema()) {
		t.Fatal("definition.schema.json is stale; run go test -run TestSchemaGolden -update")
	}
}

func generateSchema() map[string]any {
	g := &schemaGen{defs: map[string]any{}}
	root := g.object(reflect.TypeFor[Definition]())
	root["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	root["$id"] = "https://raw.githubusercontent.com/urmzd/saige/main/agent/definition/definition.schema.json"
	root["title"] = "saige agent definition frontmatter"
	root["$defs"] = g.defs
	return root
}

type schemaGen struct{ defs map[string]any }

func strEnum(values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values}
}

func (g *schemaGen) ref(t reflect.Type) map[string]any {
	name := t.Name()
	if _, ok := g.defs[name]; !ok {
		g.defs[name] = nil // reserve against recursion
		g.defs[name] = g.object(t)
	}
	r := map[string]any{"$ref": "#/$defs/" + name}
	if t.Implements(shorthandType) {
		return map[string]any{"anyOf": []any{map[string]any{"type": "string"}, r}}
	}
	return r
}

func (g *schemaGen) object(t reflect.Type) map[string]any {
	props := map[string]any{}
	var required []string
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		props[name] = g.field(t, name, f.Type)
		if !strings.Contains(opts, "omit") {
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
	var modes []string
	for _, m := range SkillModes() {
		modes = append(modes, string(m))
	}
	switch {
	case owner == reflect.TypeFor[Definition]() && name == "apiVersion":
		return strEnum(APIVersion)
	case owner == reflect.TypeFor[Definition]() && name == "name":
		return map[string]any{"type": "string", "pattern": "^[a-z]([a-z0-9-]*[a-z0-9])?$", "maxLength": maxNameLen}
	case owner == reflect.TypeFor[ToolsSpec]() && name == "harness":
		return map[string]any{"type": "array", "items": strEnum(harnessGroups...)}
	case t == reflect.TypeFor[SkillMode]():
		return strEnum(modes...)
	case owner == reflect.TypeFor[MemorySpec]() && name == "recall":
		return strEnum(RecallTool, RecallInject, RecallSelect, RecallOff)
	case owner == reflect.TypeFor[MemorySpec]() && name == "write":
		return map[string]any{"type": "array", "items": strEnum(memoryKinds...)}
	case owner == reflect.TypeFor[SubagentRef]() && name == "mode":
		return strEnum(SubagentDelegate, SubagentSpawn, SubagentHandoff)
	case owner == reflect.TypeFor[ApprovalSpec]() && name == "capabilities":
		return map[string]any{"type": "object", "propertyNames": strEnum(capabilityClasses...),
			"additionalProperties": strEnum(DecisionAllow, DecisionAsk, DecisionDeny)}
	case owner == reflect.TypeFor[ApprovalSpec]() && name == "grant":
		return strEnum(grantScopes...)
	case owner == reflect.TypeFor[GuardrailSpec]() && name == "name":
		return strEnum(GuardrailPII, GuardrailRegex, GuardrailMaxLength, GuardrailClassifier)
	case owner == reflect.TypeFor[BudgetSpec]() && name == "on_exceed":
		return strEnum(OnExceedStop, OnExceedAsk)
	case owner == reflect.TypeFor[catalog.CompactionSpec]() && name == "strategy":
		return strEnum(compactStrategies...)
	case t == reflect.TypeFor[catalog.Duration]():
		return map[string]any{"type": "string", "description": "Go duration, for example \"30s\" or \"2m\""}
	case t == reflect.TypeFor[*types.Creativity]():
		return strEnum("deterministic", "focused", "balanced", "creative")
	case t == reflect.TypeFor[types.ReasoningMode]():
		return strEnum("off", "adaptive", "on")
	case t == reflect.TypeFor[types.Depth]():
		return strEnum("minimal", "low", "medium", "high", "max")
	case owner == reflect.TypeFor[types.ToolChoice]() && name == "mode":
		return strEnum("auto", "none", "required", "named")
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
		return map[string]any{"type": "array", "items": g.field(owner, name, t.Elem())}
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
