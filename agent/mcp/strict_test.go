package mcp

import (
	"encoding/json"
	"errors"
	"slices"
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
		// Review cases: each was accepted with a different meaning before.
		"member_all_of":        prop(map[string]any{"anyOf": []any{map[string]any{"allOf": []any{map[string]any{"type": "string"}}}, map[string]any{"type": "null"}}}),
		"member_any_of":        prop(map[string]any{"anyOf": []any{map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "integer"}}}, map[string]any{"type": "null"}}}),
		"member_any_of_in_one": prop(map[string]any{"oneOf": []any{map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "integer"}}}, map[string]any{"type": "null"}}}),
		"any_of_and_one_of":    prop(map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "null"}}, "oneOf": []any{map[string]any{"type": "integer"}, map[string]any{"type": "null"}}}),
		"type_beside_union":    prop(map[string]any{"type": "string", "anyOf": []any{map[string]any{"type": "integer"}, map[string]any{"type": "null"}}}),
		"enum_beside_union":    prop(map[string]any{"enum": []any{"a"}, "anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "null"}}}),
		"required_conflict":    prop(map[string]any{"required": []any{"a"}, "anyOf": []any{map[string]any{"type": "object", "required": []any{"b"}}, map[string]any{"type": "null"}}}),
		"properties_conflict":  prop(map[string]any{"properties": map[string]any{"a": map[string]any{"type": "string"}}, "anyOf": []any{map[string]any{"type": "object", "properties": map[string]any{"b": map[string]any{"type": "string"}}}, map[string]any{"type": "null"}}}),
		"ref_with_type":        {"type": "object", "$defs": map[string]any{"A": map[string]any{"type": "object"}}, "properties": map[string]any{"v": map[string]any{"$ref": "#/$defs/A", "type": "string"}}},
		"const":                prop(map[string]any{"type": "string", "const": "x"}),
		"pattern":              prop(map[string]any{"type": "string", "pattern": "^a"}),
		"minimum":              prop(map[string]any{"type": "integer", "minimum": 1}),
		"not":                  prop(map[string]any{"type": "string", "not": map[string]any{"enum": []any{"x"}}}),
		"map_value_type":       prop(map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}}),
		"pattern_properties":   prop(map[string]any{"type": "object", "patternProperties": map[string]any{"^x": map[string]any{"type": "string"}}}),
		"integer_enum":         prop(map[string]any{"type": "integer", "enum": []any{1, 2}}),
		"mixed_enum":           prop(map[string]any{"type": "string", "enum": []any{"a", 1}}),
		"empty_enum":           prop(map[string]any{"type": "string", "enum": []any{}}),
		"null_only_enum":       prop(map[string]any{"type": "string", "enum": []any{nil}}),
		"no_type":              prop(map[string]any{"description": "x"}),
		"enum_without_type":    prop(map[string]any{"enum": []any{"a"}}),
		"unknown_type":         prop(map[string]any{"type": "date"}),
		"nullable_not_bool":    prop(map[string]any{"type": "string", "nullable": "yes"}),
		"root_nullable":        {"type": []any{"object", "null"}},
		"root_union":           {"anyOf": []any{map[string]any{"type": "object"}, map[string]any{"type": "null"}}},
		"root_enum":            {"type": "object", "enum": []any{"a"}},
		"root_legacy_defs":     {"type": "object", "definitions": map[string]any{}},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, err := ParseInputSchema(mustJSON(t, schema))
			if err == nil {
				t.Fatalf("accepted as %+v, want a refusal", s)
			}
			if !errors.Is(err, ErrUnsupportedSchema) {
				t.Errorf("err = %v, want it to wrap ErrUnsupportedSchema", err)
			}
			t.Log(err)
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

func TestParseInputSchemaReadsEnumNullAsJSONSchemaDoes(t *testing.T) {
	raw := mustJSON(t, map[string]any{"type": "object", "properties": map[string]any{
		// The type allows null but the enum does not: null is not valid.
		"list_no_null": map[string]any{"type": []any{"string", "null"}, "enum": []any{"a", "b"}},
		// The enum allows null but the type does not: null is not valid.
		"enum_null_only": map[string]any{"type": "string", "enum": []any{"a", nil}},
		// Both allow null.
		"both": map[string]any{"type": []any{"string", "null"}, "enum": []any{"a", nil}},
		// pydantic Optional[Literal["a","b"]]: null is its own branch.
		"optional_literal": map[string]any{"anyOf": []any{map[string]any{"type": "string", "enum": []any{"a", "b"}}, map[string]any{"type": "null"}}, "default": nil},
	}})
	s, _, err := ParseInputSchema(raw)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"list_no_null": false, "enum_null_only": false, "both": true, "optional_literal": true} {
		p := s.Properties[name]
		if p.Nullable != want || p.Type != types.SchemaString || len(p.Enum) == 0 || slices.Contains(p.Enum, "null") {
			t.Errorf("%s = %+v, want string enum with nullable %v", name, p, want)
		}
	}
}

func TestParseInputSchemaMergesKeywordsBesideAUnionAsBothApply(t *testing.T) {
	raw := mustJSON(t, map[string]any{"type": "object", "properties": map[string]any{
		"v": map[string]any{
			"description": "outer",
			"required":    []any{"a"},
			"anyOf": []any{
				map[string]any{"type": "object", "description": "inner", "required": []any{"a"}, "properties": map[string]any{"a": map[string]any{"type": "string"}}},
				map[string]any{"type": "null"},
			},
		},
	}})
	s, _, err := ParseInputSchema(raw)
	if err != nil {
		t.Fatal(err)
	}
	v := s.Properties["v"]
	if v.Type != types.SchemaObject || !v.Nullable || v.Description != "outer" || !slices.Equal(v.Required, []string{"a"}) || v.Properties["a"].Type != types.SchemaString {
		t.Errorf("v = %+v, want nullable object with outer description, required [a] and property a", v)
	}
}

// zod-to-json-schema writes $schema, and additionalProperties false on every
// object; the Go SDK writes type lists with null first.
func TestParseInputSchemaAcceptsZodAndGoSDKShapes(t *testing.T) {
	raw := mustJSON(t, map[string]any{
		"$schema":              "http://json-schema.org/draft-07/schema#",
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"mode"},
		"properties": map[string]any{
			"mode":  map[string]any{"type": "string", "enum": []any{"fast", "deep"}, "default": "fast"},
			"when":  map[string]any{"type": "string", "format": "date-time", "title": "When"},
			"ship":  map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"city": map[string]any{"type": "string"}}},
			"note":  map[string]any{"type": []any{"null", "string"}},
			"lines": map[string]any{"type": []any{"null", "array"}, "items": map[string]any{"type": "object", "additionalProperties": true}},
		},
	})
	s, _, err := ParseInputSchema(raw)
	if err != nil {
		t.Fatal(err)
	}
	if s.Properties["mode"].Default != "fast" || s.Properties["when"].Type != types.SchemaString || !s.Properties["note"].Nullable || !s.Properties["lines"].Nullable {
		t.Errorf("schema = %+v", s)
	}
}

func TestParseInputSchemaDoesNotModifyItsInput(t *testing.T) {
	raw := mustJSON(t, pydanticSchema)
	before := string(raw)
	if _, _, err := ParseInputSchema(raw); err != nil {
		t.Fatal(err)
	}
	if string(raw) != before {
		t.Error("ParseInputSchema modified its input")
	}
}
