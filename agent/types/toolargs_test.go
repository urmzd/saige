package types

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestValidateToolArgs(t *testing.T) {
	schema := ParameterSchema{
		Type:     "object",
		Required: []string{"path", "limit"},
		Properties: map[string]PropertyDef{
			"path":    {Type: "string"},
			"limit":   {Type: "integer"},
			"ratio":   {Type: "number"},
			"force":   {Type: "boolean"},
			"tags":    {Type: "array"},
			"opts":    {Type: "object"},
			"comment": {Type: "string", Nullable: true},
			"note":    {Type: "string"},
		},
	}
	tests := []struct {
		name    string
		schema  ParameterSchema
		args    map[string]any
		wantErr string // substring; empty means valid
	}{
		{"valid decoded JSON", schema, map[string]any{"path": "a", "limit": float64(3), "ratio": 0.5, "force": true, "tags": []any{"x"}, "opts": map[string]any{}}, ""},
		{"valid json.Number", schema, map[string]any{"path": "a", "limit": json.Number("7")}, ""},
		{"valid Go native values", schema, map[string]any{"path": "a", "limit": 2, "tags": []string{"x"}}, ""},
		{"undeclared property accepted", schema, map[string]any{"path": "a", "limit": 1, "extra": 1}, ""},
		{"nullable accepts null", schema, map[string]any{"path": "a", "limit": 1, "comment": nil}, ""},
		{"optional null treated as absent", schema, map[string]any{"path": "a", "limit": 1, "note": nil}, ""},
		{"missing required", schema, map[string]any{"path": "a"}, `missing required property "limit"`},
		{"nil arguments", schema, nil, `missing required property "path"`},
		{"wrong type", schema, map[string]any{"path": 5, "limit": 1}, `property "path" must be string, got number`},
		{"fractional integer", schema, map[string]any{"path": "a", "limit": 1.5}, `property "limit" must be integer`},
		{"required null", schema, map[string]any{"path": nil, "limit": 1}, `property "path" must be string, got null`},
		{"all problems reported", schema, map[string]any{"force": "yes"}, `missing required property "path"; missing required property "limit"; property "force" must be boolean, got string`},
		{"empty schema accepts anything", ParameterSchema{}, map[string]any{"x": 1}, ""},
		{"untyped property accepts anything", ParameterSchema{Type: "object", Properties: map[string]PropertyDef{"x": {}}}, map[string]any{"x": []any{1}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateToolArgs(tt.schema, tt.args)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q", tt.wantErr)
			}
			if !errors.Is(err, ErrInvalidToolArguments) {
				t.Errorf("error %v does not match ErrInvalidToolArguments", err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
