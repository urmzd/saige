package mcp

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestMCPNullableTypesSurviveImport(t *testing.T) {
	for _, value := range []any{[]any{"string", "null"}, []any{"null", "string"}, []string{"string", "null"}} {
		p := propertyFromMCP(map[string]any{"type": value, "enum": []any{"AG-1", nil}})
		if p.Type != "string" || !p.Nullable {
			t.Fatalf("%v lost null: %+v", value, p)
		}
		if !reflect.DeepEqual(p.JSONSchema()["enum"], []any{"AG-1", nil}) {
			t.Fatal("nullable enum changed")
		}
	}
	raw := json.RawMessage(`{"type":"object","required":["owner"],"properties":{"owner":{"type":["string","null"]},"values":{"type":"array","items":{"type":["integer","null"]}}}}`)
	schema := schemaFromMCP(raw)
	if !reflect.DeepEqual(schema.Required, []string{"owner"}) {
		t.Fatal("required changed")
	}
	if !schema.Properties["owner"].Nullable || !schema.Properties["values"].Items.Nullable {
		t.Fatal("nested nullable schema lost")
	}
}
