package google

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestNullableGeminiToolSchemaOnWire(t *testing.T) {
	defs := []types.ToolDef{{Name: "update_ticket", Parameters: types.ParameterSchema{
		Type: "object", Required: []string{"assignee_id"}, Properties: map[string]types.PropertyDef{
			"assignee_id": {Type: "string", Nullable: true, Enum: []string{"AG-1"}},
			"label":       {Type: "string"},
			"nested": {Type: "object", Nullable: true, Properties: map[string]types.PropertyDef{
				"values": {Type: "array", Items: &types.PropertyDef{Type: "integer", Nullable: true}},
			}},
		},
	}}}
	raw, err := json.Marshal(toGeminiTools(defs))
	if err != nil {
		t.Fatal(err)
	}
	var wire []any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	schema := wire[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)["parameters"].(map[string]any)
	properties := schema["properties"].(map[string]any)
	owner := properties["assignee_id"].(map[string]any)
	if owner["type"] != "STRING" || owner["nullable"] != true {
		t.Fatalf("null lost: %s", raw)
	}
	if !reflect.DeepEqual(schema["required"], []any{"assignee_id"}) {
		t.Fatalf("presence changed: %s", raw)
	}
	if _, exists := properties["label"].(map[string]any)["nullable"]; exists {
		t.Fatal("non-nullable field changed")
	}
	nested := properties["nested"].(map[string]any)
	if nested["nullable"] != true {
		t.Fatal("nullable object lost")
	}
	items := nested["properties"].(map[string]any)["values"].(map[string]any)["items"].(map[string]any)
	if items["nullable"] != true || items["type"] != "INTEGER" {
		t.Fatal("nullable item lost")
	}
}
