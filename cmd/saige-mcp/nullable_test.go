package main

import (
	"github.com/urmzd/saige/agent/types"
	"reflect"
	"testing"
)

func TestMCPExportPreservesNullableAndRequired(t *testing.T) {
	got := parameterSchemaToJSON(types.ParameterSchema{Type: "object", Required: []string{"owner"}, Properties: map[string]types.PropertyDef{
		"owner":  {Type: "string", Nullable: true},
		"filter": {Type: "string", Nullable: true},
	}})
	if !reflect.DeepEqual(got["required"], []string{"owner"}) {
		t.Fatal("required changed")
	}
	for _, name := range []string{"owner", "filter"} {
		p := got["properties"].(map[string]any)[name].(map[string]any)
		if !reflect.DeepEqual(p["type"], []string{"string", "null"}) {
			t.Fatalf("%s lost null: %v", name, p)
		}
	}
}
