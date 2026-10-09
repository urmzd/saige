package mcp

import (
	"encoding/json"

	"github.com/urmzd/saige/agent/types"
)

// CoerceScalarsToArrays is an ArgTransform that wraps a scalar argument in a
// one-element array when the tool's own schema declares that property as an
// array. Models often send "tags": "go" for a "tags" array, and servers
// reject it. Arguments the schema does not describe are left alone. It
// modifies args in place and returns it.
func CoerceScalarsToArrays(_ string, schema json.RawMessage, args map[string]any) map[string]any {
	if len(schema) == 0 || len(args) == 0 {
		return args
	}
	var s struct {
		Properties map[string]struct {
			Type any `json:"type"`
		} `json:"properties"`
	}
	if json.Unmarshal(schema, &s) != nil {
		return args
	}
	for name, v := range args {
		prop, ok := s.Properties[name]
		if !ok || !declaresArray(prop.Type) {
			continue
		}
		switch v.(type) {
		case nil, []any, []string, []map[string]any:
			continue
		}
		args[name] = []any{v}
	}
	return args
}

// declaresArray reports whether a JSON Schema type keyword allows an array.
// A union that also allows the scalar's own type is left alone, because the
// scalar is then already valid.
func declaresArray(t any) bool {
	switch v := t.(type) {
	case string:
		return v == types.SchemaArray
	case []any:
		hasArray := false
		for _, m := range v {
			s, _ := m.(string)
			switch s {
			case types.SchemaArray:
				hasArray = true
			case types.SchemaNull:
			default:
				return false
			}
		}
		return hasArray
	}
	return false
}
