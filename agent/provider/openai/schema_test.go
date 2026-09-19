package openai

import (
	"reflect"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func TestResponseSchema(t *testing.T) {
	tests := []struct {
		name       string
		schema     types.ParameterSchema
		wantStrict bool
		wantClosed []string // dotted paths of objects that must be closed
	}{
		{
			name: "every property required: strict",
			schema: types.ParameterSchema{Type: "object", Required: []string{"verdict", "reason"},
				Properties: map[string]types.PropertyDef{"verdict": {Type: "string", Enum: []string{"pass", "fail"}}, "reason": {Type: "string"}}},
			wantStrict: true, wantClosed: []string{""},
		},
		{
			name: "an optional property: closed but not strict",
			schema: types.ParameterSchema{Type: "object", Required: []string{"verdict"},
				Properties: map[string]types.PropertyDef{"verdict": {Type: "string"}, "reason": {Type: "string"}}},
			wantStrict: false, wantClosed: []string{""},
		},
		{
			name: "nested objects and array items are closed too",
			schema: types.ParameterSchema{Type: "object", Required: []string{"meta", "rows"},
				Properties: map[string]types.PropertyDef{
					"meta": {Type: "object", Required: []string{"id"}, Properties: map[string]types.PropertyDef{"id": {Type: "string"}}},
					"rows": {Type: "array", Items: &types.PropertyDef{Type: "object", Required: []string{"n"}, Properties: map[string]types.PropertyDef{"n": {Type: "integer"}}}},
				}},
			wantStrict: true, wantClosed: []string{"", "meta", "rows[]"},
		},
		{
			name: "an optional property deep inside still prevents strict",
			schema: types.ParameterSchema{Type: "object", Required: []string{"meta"},
				Properties: map[string]types.PropertyDef{
					"meta": {Type: "object", Properties: map[string]types.PropertyDef{"id": {Type: "string"}}},
				}},
			wantStrict: false, wantClosed: []string{"", "meta"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, strict := responseSchema(tt.schema)
			if strict != tt.wantStrict {
				t.Errorf("strict = %v, want %v", strict, tt.wantStrict)
			}
			for _, path := range tt.wantClosed {
				node := got
				switch path {
				case "meta":
					node = got["properties"].(map[string]any)["meta"].(map[string]any)
				case "rows[]":
					node = got["properties"].(map[string]any)["rows"].(map[string]any)["items"].(map[string]any)
				}
				if node["additionalProperties"] != false {
					t.Errorf("object at %q is not closed: %v", path, node)
				}
			}
		})
	}
}

// Tool parameter schemas are not response formats and must be left alone.
func TestToolSchemasAreNotClosed(t *testing.T) {
	got := parameterSchemaToMap(types.ParameterSchema{Type: "object", Properties: map[string]types.PropertyDef{"q": {Type: "string"}}})
	want := map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tool schema changed: %v", got)
	}
}
