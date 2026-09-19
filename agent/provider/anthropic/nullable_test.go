package anthropic

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestNullableToolSchemaOnWire(t *testing.T) {
	defs := []types.ToolDef{{Name: "update_ticket", Parameters: types.ParameterSchema{
		Type: "object", Required: []string{"assignee_id"}, Properties: map[string]types.PropertyDef{
			"assignee_id": {Type: "string", Nullable: true, Enum: []string{"AG-1"}},
			"filter":      {Type: "string", Nullable: true},
			"label":       {Type: "string"},
			"nested": {Type: "object", Nullable: true, Required: []string{"values"}, Properties: map[string]types.PropertyDef{
				"values": {Type: "array", Items: &types.PropertyDef{Type: "integer", Nullable: true}},
			}},
		},
	}}}
	raw, err := json.Marshal(toAnthropicTools(defs))
	if err != nil {
		t.Fatal(err)
	}
	var wire []any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	tool := wire[0].(map[string]any)
	schema := tool["input_schema"].(map[string]any)
	properties := schema["properties"].(map[string]any)
	owner := properties["assignee_id"].(map[string]any)
	if !reflect.DeepEqual(owner["type"], []any{"string", "null"}) {
		t.Fatalf("nullable type lost: %s", raw)
	}
	if !reflect.DeepEqual(owner["enum"], []any{"AG-1", nil}) {
		t.Fatalf("enum excludes null: %s", raw)
	}
	if !reflect.DeepEqual(schema["required"], []any{"assignee_id"}) {
		t.Fatalf("presence rules changed: %s", raw)
	}
	if properties["label"].(map[string]any)["type"] != "string" {
		t.Fatalf("plain string changed: %s", raw)
	}
	if _, exists := owner["nullable"]; exists {
		t.Fatal("JSON Schema must express null in the type union")
	}
	nested := properties["nested"].(map[string]any)
	if !reflect.DeepEqual(nested["type"], []any{"object", "null"}) {
		t.Fatal("nullable object lost")
	}
	items := nested["properties"].(map[string]any)["values"].(map[string]any)["items"].(map[string]any)
	if !reflect.DeepEqual(items["type"], []any{"integer", "null"}) {
		t.Fatal("nullable array item lost")
	}
}
