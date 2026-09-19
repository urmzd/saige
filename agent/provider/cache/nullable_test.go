package cache

import (
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestNullabilityChangesToolAndResponseCacheKeys(t *testing.T) {
	schema := types.ParameterSchema{Type: "object", Properties: map[string]types.PropertyDef{
		"rows": {Type: "array", Items: &types.PropertyDef{Type: "string"}},
	}}
	defs := []types.ToolDef{{Name: "list_tickets", Parameters: schema}}
	beforeTool := Key("model", nil, defs, nil)
	beforeResponse := Key("model", nil, nil, &schema)
	schema.Properties["rows"].Items.Nullable = true
	if beforeTool == Key("model", nil, defs, nil) {
		t.Fatal("nullable tool reuses non-nullable response")
	}
	if beforeResponse == Key("model", nil, nil, &schema) {
		t.Fatal("nullable response schema reuses non-nullable response")
	}
}
