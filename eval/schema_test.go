package eval

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCompileJSONSchemaRejects(t *testing.T) {
	tests := []struct {
		name   string
		schema string
		want   string
	}{
		{"not json", `{`, "json schema"},
		{"not an object", `3`, "must be an object or a boolean"},
		{"unknown type", `{"type": "date"}`, `unknown type "date"`},
		{"ref unsupported", `{"$ref": "#/defs/x"}`, `keyword "$ref" is not supported`},
		{"bad pattern", `{"pattern": "("}`, "pattern"},
		{"negative count", `{"minItems": -1}`, "non-negative integer"},
		{"empty anyOf", `{"anyOf": []}`, "non-empty array"},
		{"misspelled required", `{"type": "object", "requird": ["a"]}`, `keyword "requird" is not supported`},
		{"misspelled nested keyword", `{"properties": {"a": {"propertes": {}}}}`, `#/properties/a/propertes: keyword "propertes" is not supported`},
		{"format unsupported", `{"type": "string", "format": "email"}`, `keyword "format" is not supported`},
		{"extension keyword", `{"x-order": 1}`, `keyword "x-order" is not supported`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := CompileJSONSchema(json.RawMessage(tt.schema))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestCompileJSONSchemaAcceptsAnnotations(t *testing.T) {
	const doc = `{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$id": "urn:example",
		"$comment": "c",
		"$defs": {"x": {"type": "string"}},
		"definitions": {"y": {"type": "number"}},
		"title": "t",
		"description": "d",
		"examples": [{}],
		"default": {},
		"deprecated": false,
		"readOnly": false,
		"writeOnly": false,
		"type": "object"
	}`
	schema, err := CompileJSONSchema(json.RawMessage(doc))
	if err != nil {
		t.Fatal(err)
	}
	if p := schema.Validate(map[string]any{}); len(p) != 0 {
		t.Fatalf("problems = %v", p)
	}
}

func TestJSONSchemaValidate(t *testing.T) {
	const person = `{
		"$schema": "http://json-schema.org/draft-07/schema#",
		"title": "person",
		"type": "object",
		"required": ["name", "age"],
		"additionalProperties": false,
		"properties": {
			"name": {"type": "string", "minLength": 1, "maxLength": 10, "pattern": "^[A-Z]"},
			"age": {"type": "integer", "minimum": 0, "exclusiveMaximum": 150},
			"email": {"type": ["string", "null"]},
			"role": {"enum": ["admin", "user", 3]},
			"tags": {"type": "array", "items": {"type": "string"}, "minItems": 1, "maxItems": 2},
			"kind": {"const": "person"},
			"nick": {"type": "string", "nullable": true},
			"id": {"oneOf": [{"type": "integer"}, {"type": "string", "pattern": "^x"}]},
			"score": {"anyOf": [{"type": "number", "maximum": 1}, {"type": "null"}]},
			"note": {"not": {"type": "number"}},
			"both": {"allOf": [{"type": "number"}, {"minimum": 5}]}
		}
	}`
	schema, err := CompileJSONSchema(json.RawMessage(person))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		doc  string
		want []string // substrings, one per expected problem
	}{
		{"valid minimal", `{"name": "Ada", "age": 36}`, nil},
		{"valid full", `{"name": "Ada", "age": 36, "email": null, "role": 3, "tags": ["a"], "kind": "person", "nick": null, "id": "x1", "score": null, "note": "n", "both": 6}`, nil},
		{"integer as float", `{"name": "Ada", "age": 36.0}`, nil},
		{"wrong root type", `[1]`, []string{"/: got array, want object"}},
		{"missing required", `{"name": "Ada"}`, []string{`missing required property "age"`}},
		{"extra property", `{"name": "Ada", "age": 1, "x": 1}`, []string{`property "x" is not allowed`}},
		{"non integer", `{"name": "Ada", "age": 1.5}`, []string{"/age: got number, want integer"}},
		{"bounds", `{"name": "Ada", "age": 150}`, []string{"/age: 150 is not below 150"}},
		{"string rules", `{"name": "", "age": 1}`, []string{"length 0", "does not match pattern"}},
		{"enum", `{"name": "Ada", "age": 1, "role": "root"}`, []string{"/role: value \"root\" is not one of"}},
		{"const", `{"name": "Ada", "age": 1, "kind": "cat"}`, []string{"/kind: value \"cat\" is not \"person\""}},
		{"items", `{"name": "Ada", "age": 1, "tags": [1, 2, 3]}`, []string{"3 items", "/tags/0: got integer", "/tags/1", "/tags/2"}},
		{"oneOf none", `{"name": "Ada", "age": 1, "id": "y"}`, []string{"matches 0 of the oneOf"}},
		{"anyOf none", `{"name": "Ada", "age": 1, "score": 2}`, []string{"none of the anyOf"}},
		{"not", `{"name": "Ada", "age": 1, "note": 1}`, []string{"must not match"}},
		{"allOf", `{"name": "Ada", "age": 1, "both": 1}`, []string{"below the minimum 5"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var doc any
			if err := json.Unmarshal([]byte(tt.doc), &doc); err != nil {
				t.Fatal(err)
			}
			problems := schema.Validate(doc)
			if len(problems) != len(tt.want) {
				t.Fatalf("problems = %q, want %d", problems, len(tt.want))
			}
			for i, want := range tt.want {
				if !strings.Contains(problems[i], want) {
					t.Errorf("problem %d = %q, want it to contain %q", i, problems[i], want)
				}
			}
		})
	}
}

func TestJSONSchemaBooleanAndGoValues(t *testing.T) {
	never, _ := CompileJSONSchema(json.RawMessage(`false`))
	if len(never.Validate("x")) != 1 {
		t.Fatal("false schema accepted a value")
	}
	schema, _ := CompileJSONSchema(json.RawMessage(`{"type": "object", "properties": {"n": {"type": "integer"}}}`))
	if problems := schema.Validate(map[string]any{"n": 3}); len(problems) != 0 {
		t.Fatalf("Go map with an int rejected: %v", problems)
	}
}
