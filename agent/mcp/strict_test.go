package mcp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseInputSchemaKeepsTheTypeOfANullableProperty(t *testing.T) {
	raw := mustJSON(t, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"any_of":  map[string]any{"description": "a or null", "anyOf": []any{map[string]any{"type": "string", "description": "member"}, map[string]any{"type": "null"}}},
			"one_of":  map[string]any{"oneOf": []any{map[string]any{"type": "null"}, map[string]any{"type": "integer"}}},
			"array":   map[string]any{"type": []any{"boolean", "null"}},
			"nested":  map[string]any{"type": "object", "properties": map[string]any{"inner": map[string]any{"anyOf": []any{map[string]any{"type": "number"}, map[string]any{"type": "null"}}}}},
			"listing": map[string]any{"type": "array", "items": map[string]any{"type": []any{"string", "null"}}},
			"plain":   map[string]any{"type": "string"},
		},
	})
	s, _, err := ParseInputSchema(raw)
	if err != nil {
		t.Fatal(err)
	}
	for name, typ := range map[string]string{"any_of": "string", "one_of": "integer", "array": "boolean"} {
		if p := s.Properties[name]; p.Type != typ || !p.Nullable {
			t.Errorf("%s = {type %q nullable %v}, want {type %q nullable true}", name, p.Type, p.Nullable, typ)
		}
	}
	if d := s.Properties["any_of"].Description; d != "a or null" {
		t.Errorf("any_of description = %q, want the keyword beside the union to win", d)
	}
	if in := s.Properties["nested"].Properties["inner"]; in.Type != "number" || !in.Nullable {
		t.Errorf("nested.inner = %+v, want nullable number", in)
	}
	if it := s.Properties["listing"].Items; it == nil || it.Type != "string" || !it.Nullable {
		t.Errorf("listing items = %+v, want nullable string", it)
	}
	if p := s.Properties["plain"]; p.Type != "string" || p.Nullable {
		t.Errorf("plain = %+v, want non-nullable string", p)
	}
}

func TestParseInputSchemaInlinesLocalDefs(t *testing.T) {
	s, stats, err := ParseInputSchema(mustJSON(t, pydanticSchema))
	if err != nil {
		t.Fatalf("pydantic schema refused: %v", err)
	}
	if stats.Inlined == 0 {
		t.Errorf("stats = %+v, want references inlined", stats)
	}
	addr := s.Properties["customer"].Properties["address"]
	if addr.Type != types.SchemaObject || addr.Properties["city"].Type != types.SchemaString {
		t.Errorf("customer.address = %+v, want the Address definition", addr)
	}
	if zip := addr.Properties["zip"]; zip.Type != types.SchemaString || !zip.Nullable {
		t.Errorf("customer.address.zip = %+v, want nullable string", zip)
	}
	if ship := s.Properties["ship_to"]; ship.Type != types.SchemaObject || !ship.Nullable {
		t.Errorf("ship_to = %+v, want nullable Address", ship)
	}
	if it := s.Properties["lines"].Items; it == nil || it.Properties["qty"].Type != types.SchemaInteger {
		t.Errorf("lines items = %+v, want the Line definition", it)
	}
}

func TestParseInputSchemaRefusesWhatItCannotRepresent(t *testing.T) {
	prop := func(p map[string]any) map[string]any {
		return map[string]any{"type": "object", "properties": map[string]any{"v": p}}
	}
	deep := map[string]any{"type": "string"}
	for range MaxSchemaDepth + 1 {
		deep = map[string]any{"type": "object", "properties": map[string]any{"x": deep}}
	}
	for name, schema := range map[string]map[string]any{
		"any_of_two_types": prop(map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "integer"}}}),
		"one_of_no_null":   prop(map[string]any{"oneOf": []any{map[string]any{"type": "string"}}}),
		"type_array":       prop(map[string]any{"type": []any{"string", "integer"}}),
		"only_null_types":  prop(map[string]any{"type": []any{"null", "null"}}),
		"all_of":           prop(map[string]any{"allOf": []any{map[string]any{"type": "string"}}}),
		"remote_ref":       prop(map[string]any{"$ref": "https://example.com/s.json"}),
		"missing_def":      prop(map[string]any{"$ref": "#/$defs/Nope"}),
		"tuple_items":      prop(map[string]any{"type": "array", "items": []any{map[string]any{"type": "string"}}}),
		"nested_union":     prop(map[string]any{"type": "object", "properties": map[string]any{"w": map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "boolean"}}}}}),
		"root_all_of":      {"type": "object", "allOf": []any{map[string]any{"type": "object"}}},
		"root_any_of":      {"type": "object", "anyOf": []any{map[string]any{"type": "object"}, map[string]any{"type": "object"}}},
		"root_not_object":  {"type": "string"},
		"too_deep":         deep,
	} {
		t.Run(name, func(t *testing.T) {
			s, _, err := ParseInputSchema(mustJSON(t, schema))
			if err == nil {
				t.Fatalf("accepted as %+v, want a refusal", s)
			}
			if !errors.Is(err, ErrUnsupportedSchema) {
				t.Errorf("err = %v, want it to wrap ErrUnsupportedSchema", err)
			}
			if !strings.Contains(err.Error(), "$") {
				t.Errorf("err = %v, want it to name a location", err)
			}
		})
	}
}

func TestParseInputSchemaReadsAnEmptySchemaAsAnObject(t *testing.T) {
	for _, raw := range []string{"", "{}", "null"} {
		s, _, err := ParseInputSchema(json.RawMessage(raw))
		if err != nil || s.Type != types.SchemaObject || len(s.Properties) != 0 {
			t.Errorf("ParseInputSchema(%q) = %+v, %v; want an empty object", raw, s, err)
		}
	}
}

func TestParseInputSchemaRefusesJSONThatIsNotAnObject(t *testing.T) {
	for _, raw := range []string{"[]", `"x"`, "{"} {
		if _, _, err := ParseInputSchema(json.RawMessage(raw)); !errors.Is(err, ErrUnsupportedSchema) {
			t.Errorf("ParseInputSchema(%q) err = %v, want ErrUnsupportedSchema", raw, err)
		}
	}
}
