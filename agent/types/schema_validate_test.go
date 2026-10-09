package types

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestValidateJSON(t *testing.T) {
	schema := ParameterSchema{
		Type:     "object",
		Required: []string{"name", "tags", "address"},
		Properties: map[string]PropertyDef{
			"name":  {Type: "string"},
			"level": {Type: "string", Enum: []string{"low", "high"}},
			"count": {Type: "integer", Enum: []string{"1", "2"}},
			"tags":  {Type: "array", Items: &PropertyDef{Type: "string"}},
			"note":  {Type: "string", Nullable: true},
			"address": {Type: "object", Required: []string{"city"}, Properties: map[string]PropertyDef{
				"city": {Type: "string"},
				"zip":  {Type: "string"},
			}},
		},
	}
	tests := []struct {
		name string
		json string
		want []string // substrings of the error; nil means valid
	}{
		{"valid", `{"name":"a","tags":["x"],"address":{"city":"c"},"level":"low","count":2,"note":null}`, nil},
		{"undeclared properties are accepted", `{"name":"a","tags":[],"address":{"city":"c"},"extra":1}`, nil},
		{"optional null means absent", `{"name":"a","tags":[],"address":{"city":"c","zip":null}}`, nil},
		{"missing required", `{"tags":[],"address":{"city":"c"}}`, []string{`$ missing required property "name"`}},
		{"nested required", `{"name":"a","tags":[],"address":{}}`, []string{`$.address missing required property "city"`}},
		{"array item type", `{"name":"a","tags":["x",3],"address":{"city":"c"}}`, []string{"$.tags[1] must be string, got number"}},
		{"string enum", `{"name":"a","tags":[],"address":{"city":"c"},"level":"mid"}`, []string{"$.level must be one of"}},
		{"number enum", `{"name":"a","tags":[],"address":{"city":"c"},"count":3}`, []string{"$.count must be one of"}},
		{"required null", `{"name":null,"tags":[],"address":{"city":"c"}}`, []string{"$.name must be string, got null"}},
		{"every problem is listed", `{"name":1,"tags":"x","address":{"city":"c"}}`, []string{"$.name must be string", "$.tags must be array"}},
		{"root type", `[1]`, []string{"$ must be object, got array"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dec := json.NewDecoder(strings.NewReader(tt.json))
			dec.UseNumber()
			var v any
			if err := dec.Decode(&v); err != nil {
				t.Fatal(err)
			}
			err := ValidateJSON(schema, v)
			if tt.want == nil {
				if err != nil {
					t.Fatalf("ValidateJSON = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrSchemaMismatch) {
				t.Fatalf("ValidateJSON = %v, want ErrSchemaMismatch", err)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
		})
	}
}

func TestValidateJSONUntypedRoot(t *testing.T) {
	schema := ParameterSchema{Properties: map[string]PropertyDef{"a": {Type: "string"}}}
	if err := ValidateJSON(schema, map[string]any{"a": 1.0}); err == nil {
		t.Fatal("a schema with properties and no type should still check them")
	}
	if err := ValidateJSON(ParameterSchema{}, "anything"); err != nil {
		t.Fatalf("an empty schema accepts any value, got %v", err)
	}
}
