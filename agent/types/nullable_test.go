package types

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

func TestNullableSchemaValidatesNullAndPresenceIndependently(t *testing.T) {
	for _, required := range []bool{false, true} {
		for _, nullable := range []bool{false, true} {
			definition := PropertyDef{Type: "object", Properties: map[string]PropertyDef{
				"assignee_id": {Type: "string", Nullable: nullable, Enum: []string{"AG-1"}},
			}}
			if required {
				definition.Required = []string{"assignee_id"}
			}
			raw, err := json.Marshal(definition.JSONSchema())
			if err != nil {
				t.Fatal(err)
			}
			var schema jsonschema.Schema
			if err := json.Unmarshal(raw, &schema); err != nil {
				t.Fatal(err)
			}
			resolved, err := schema.Resolve(nil)
			if err != nil {
				t.Fatal(err)
			}
			cases := []struct {
				name  string
				value map[string]any
				valid bool
			}{
				{"absent", map[string]any{}, !required},
				{"null", map[string]any{"assignee_id": nil}, nullable},
				{"allowed string", map[string]any{"assignee_id": "AG-1"}, true},
				{"other string", map[string]any{"assignee_id": "AG-2"}, false},
				{"wrong type", map[string]any{"assignee_id": 7}, false},
			}
			for _, c := range cases {
				if err := resolved.Validate(c.value); (err == nil) != c.valid {
					t.Errorf("required=%v nullable=%v %s: valid=%v, error=%v; schema=%s", required, nullable, c.name, c.valid, err, raw)
				}
			}
		}
	}
}

func TestSchemaFromPointerNullabilityDoesNotChangePresence(t *testing.T) {
	type Request struct {
		Owner  *string `json:"owner"`
		Filter *string `json:"filter,omitempty"`
		Label  string  `json:"label,omitempty"`
		Items  []*int  `json:"items"`
		Nested *struct {
			Active *bool `json:"active"`
		} `json:"nested"`
	}
	schema := SchemaFrom[Request]()
	if !reflect.DeepEqual(schema.Required, []string{"owner", "items", "nested"}) {
		t.Fatalf("required = %v", schema.Required)
	}
	for _, name := range []string{"owner", "filter", "nested"} {
		if !schema.Properties[name].Nullable {
			t.Errorf("%s lost pointer nullability", name)
		}
	}
	if schema.Properties["label"].Nullable || schema.Properties["items"].Nullable {
		t.Fatal("non-pointer fields gained nullability")
	}
	if !schema.Properties["items"].Items.Nullable || !schema.Properties["nested"].Properties["active"].Nullable {
		t.Fatal("nested pointer lost nullability")
	}
}

func TestNullableJSONSchemaCopiesEnumAndRequired(t *testing.T) {
	p := PropertyDef{Type: "string", Nullable: true, Enum: []string{"open"}, Required: []string{"x"}}
	schema := p.JSONSchema()
	schema["enum"].([]any)[0] = "closed"
	schema["required"].([]string)[0] = "y"
	if p.Enum[0] != "open" || p.Required[0] != "x" {
		t.Fatal("schema output changed the definition")
	}
	if got := (PropertyDef{Type: "null", Nullable: true}).JSONSchema()["type"]; got != "null" {
		t.Fatalf("null-only type = %v", got)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var restored PropertyDef
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, p) {
		t.Fatalf("definition round trip: %+v", restored)
	}
}
