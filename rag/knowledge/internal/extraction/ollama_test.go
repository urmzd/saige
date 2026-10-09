package extraction

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestJSONFormatSchema(t *testing.T) {
	var schema struct {
		Type       string `json:"type"`
		Required   []string
		Properties map[string]struct {
			Type  string `json:"type"`
			Items struct {
				Type     string   `json:"type"`
				Required []string `json:"required"`
			} `json:"items"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(jsonFormat, &schema); err != nil {
		t.Fatalf("format is not valid JSON: %v", err)
	}
	tests := []struct {
		property string
		required []string
	}{
		{"entities", []string{"name", "type", "summary"}},
		{"relations", []string{"source", "target", "type", "fact"}},
	}
	for _, tt := range tests {
		t.Run(tt.property, func(t *testing.T) {
			p, ok := schema.Properties[tt.property]
			if !ok || p.Type != "array" || p.Items.Type != "object" {
				t.Fatalf("%s = %+v, want an array of objects", tt.property, p)
			}
			if !slices.Equal(p.Items.Required, tt.required) {
				t.Fatalf("%s required = %v, want %v", tt.property, p.Items.Required, tt.required)
			}
		})
	}
	if schema.Type != "object" || !slices.Equal(schema.Required, []string{"entities", "relations"}) {
		t.Fatalf("root = %s %v", schema.Type, schema.Required)
	}
}
