package types

import (
	"encoding/json"
	"reflect"
	"testing"
)

// A schema in the form MCP servers and OpenAPI generators emit.
const realSchema = `{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "additionalProperties": false,
  "required": ["query", "filters"],
  "properties": {
    "query": {"type": "string", "description": "Search text", "minLength": 1},
    "limit": {"type": "integer", "default": 10, "minimum": 1},
    "mode": {"type": ["string", "null"], "enum": ["fast", "deep", null]},
    "level": {"type": "integer", "enum": [1, 2, 3]},
    "tags": {"type": "array", "items": {"type": "string"}},
    "filters": {
      "type": "object",
      "required": ["lang"],
      "properties": {"lang": {"type": "string"}, "since": {"type": "string", "nullable": true}}
    },
    "anything": {}
  }
}`

func TestParameterSchemaUnmarshalsJSONSchema(t *testing.T) {
	var got ParameterSchema
	if err := json.Unmarshal([]byte(realSchema), &got); err != nil {
		t.Fatal(err)
	}
	want := ParameterSchema{
		Type:     "object",
		Required: []string{"query", "filters"},
		Properties: map[string]PropertyDef{
			"query":    {Type: "string", Description: "Search text"},
			"limit":    {Type: "integer", Default: float64(10)},
			"mode":     {Type: "string", Nullable: true, Enum: []string{"fast", "deep"}},
			"level":    {Type: "integer", Enum: []string{"1", "2", "3"}},
			"tags":     {Type: "array", Items: &PropertyDef{Type: "string"}},
			"filters":  {Type: "object", Required: []string{"lang"}, Properties: map[string]PropertyDef{"lang": {Type: "string"}, "since": {Type: "string", Nullable: true}}},
			"anything": {},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %#v\nwant %#v", got, want)
	}
	// The decoded schema renders back to the nullable form providers expect.
	if mode := got.Properties["mode"].JSONSchema(); !reflect.DeepEqual(mode["type"], []string{"string", "null"}) {
		t.Errorf("mode schema = %v", mode)
	}
}

func TestPropertyDefTypeForms(t *testing.T) {
	tests := []struct {
		in   string
		want PropertyDef
	}{
		{`{"type":"null"}`, PropertyDef{Type: "null"}},
		{`{"type":["null"]}`, PropertyDef{Type: "null"}},
		{`{"type":["null","number"]}`, PropertyDef{Type: "number", Nullable: true}},
		{`{"type":null}`, PropertyDef{}},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			var got PropertyDef
			if err := json.Unmarshal([]byte(tt.in), &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %#v, want %#v", got, tt.want)
			}
		})
	}
	var bad PropertyDef
	if err := json.Unmarshal([]byte(`{"type":7}`), &bad); err == nil {
		t.Error("numeric type accepted")
	}
}

func TestToolDefJSONRoundTrip(t *testing.T) {
	def := ToolDef{
		Name: "rm", Description: "delete", Capability: ToolCapabilityDestructive,
		Parameters: ParameterSchema{Type: "object", Required: []string{"path"},
			Properties: map[string]PropertyDef{"path": {Type: "string", Nullable: true}}},
	}
	b, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	var back ToolDef
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, def) {
		t.Errorf("got %#v\nwant %#v\nwire %s", back, def, b)
	}
}

func TestToolCapabilityEffective(t *testing.T) {
	tests := map[ToolCapability]ToolCapability{
		"":                        ToolCapabilityUnknown,
		"bogus":                   ToolCapabilityUnknown,
		ToolCapabilityUnknown:     ToolCapabilityUnknown,
		ToolCapabilityRead:        ToolCapabilityRead,
		ToolCapabilityWrite:       ToolCapabilityWrite,
		ToolCapabilityDestructive: ToolCapabilityDestructive,
	}
	for in, want := range tests {
		if got := in.Effective(); got != want {
			t.Errorf("%q.Effective() = %q, want %q", in, got, want)
		}
	}
}
