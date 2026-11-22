package mcp

import (
	"encoding/json"
	"fmt"

	"github.com/urmzd/saige/agent/types"
)

// MaxSchemaDepth bounds how deeply ParseInputSchema follows nested
// properties and array items.
const MaxSchemaDepth = 32

// ParseInputSchema converts an MCP tool input schema into a ParameterSchema,
// and refuses any schema it cannot represent exactly. Use it where a tool
// must never reach a model with a schema that means something else. The
// tool import in this package uses a lenient conversion that approximates
// instead.
//
// Local "#/$defs/<name>" references are inlined first (see InlineDefs). A
// nullable property, written as anyOf or oneOf of one schema and
// {"type":"null"}, or as a type list of one type and "null", becomes that
// type with Nullable set. Keywords written beside such a union win over the
// member's. Any other anyOf or oneOf, any allOf, an unresolvable $ref, a type
// list of several types, nesting deeper than MaxSchemaDepth, and a root that
// is not an object are refused. Every refusal wraps ErrUnsupportedSchema and
// names the location in the schema and the reason.
//
// An empty raw schema is an object with no properties.
func ParseInputSchema(raw json.RawMessage) (types.ParameterSchema, InlineStats, error) {
	var stats InlineStats
	if len(raw) == 0 {
		return types.ParameterSchema{Type: types.SchemaObject}, stats, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return types.ParameterSchema{}, stats, fmt.Errorf("%w: schema is not a JSON object: %w", ErrUnsupportedSchema, err)
	}
	doc, stats, err := InlineDefs(doc)
	if err != nil {
		return types.ParameterSchema{}, stats, err
	}
	if doc == nil {
		return types.ParameterSchema{Type: types.SchemaObject}, stats, nil
	}
	if err := normalizeStrict(doc, "$", 0); err != nil {
		return types.ParameterSchema{}, stats, err
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return types.ParameterSchema{}, stats, fmt.Errorf("%w: schema is not JSON Schema: %w", ErrUnsupportedSchema, err)
	}
	var s types.ParameterSchema
	if err := json.Unmarshal(b, &s); err != nil {
		return types.ParameterSchema{}, stats, fmt.Errorf("%w: schema is not JSON Schema: %w", ErrUnsupportedSchema, err)
	}
	if s.Type == "" {
		s.Type = types.SchemaObject
	}
	if s.Type != types.SchemaObject {
		return types.ParameterSchema{}, stats, fmt.Errorf("%w: schema at $ has type %q, want object", ErrUnsupportedSchema, s.Type)
	}
	return s, stats, nil
}

// normalizeStrict rewrites node in place into the subset PropertyDef
// models, or returns the reason it cannot. See ParseInputSchema.
func normalizeStrict(node map[string]any, path string, depth int) error {
	if depth > MaxSchemaDepth {
		return fmt.Errorf("%w: schema at %s nests deeper than %d levels", ErrUnsupportedSchema, path, MaxSchemaDepth)
	}
	if _, ok := node["$ref"]; ok {
		// InlineDefs resolves or refuses every $ref first. This guards a
		// $ref that a definition body carries under a keyword it skips.
		return fmt.Errorf("%w: schema at %s uses $ref, which is not supported", ErrUnsupportedSchema, path)
	}
	if _, ok := node["allOf"]; ok {
		return fmt.Errorf("%w: schema at %s uses allOf, which is not supported", ErrUnsupportedSchema, path)
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		if _, ok := node[key]; ok {
			if err := foldNullableUnion(node, key, path); err != nil {
				return err
			}
		}
	}
	if list, ok := node["type"].([]any); ok {
		only, nulls := "", 0
		for _, t := range list {
			s, _ := t.(string)
			switch {
			case s == types.SchemaNull:
				nulls++
			case s != "" && only == "":
				only = s
			default:
				return fmt.Errorf("%w: schema at %s has several types, which is not supported", ErrUnsupportedSchema, path)
			}
		}
		if only == "" || nulls > 1 {
			return fmt.Errorf("%w: schema at %s has no single type", ErrUnsupportedSchema, path)
		}
		node["type"] = only
		if nulls == 1 {
			node["nullable"] = true
		}
	}
	if v, ok := node["properties"]; ok {
		props, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: schema at %s.properties is not an object", ErrUnsupportedSchema, path)
		}
		for name, pv := range props {
			p, ok := pv.(map[string]any)
			if !ok {
				return fmt.Errorf("%w: schema at %s is not an object", ErrUnsupportedSchema, path+"."+name)
			}
			if err := normalizeStrict(p, path+"."+name, depth+1); err != nil {
				return err
			}
		}
	}
	if v, ok := node["items"]; ok {
		items, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: schema at %s[] is not a single schema, which is not supported", ErrUnsupportedSchema, path)
		}
		if err := normalizeStrict(items, path+"[]", depth+1); err != nil {
			return err
		}
	}
	return nil
}

// foldNullableUnion replaces node[key], an anyOf or oneOf, by its one
// non-null member and sets nullable. Keywords already on node win.
func foldNullableUnion(node map[string]any, key, path string) error {
	list, ok := node[key].([]any)
	if !ok {
		return fmt.Errorf("%w: schema at %s: %s is not a list", ErrUnsupportedSchema, path, key)
	}
	var only map[string]any
	nulls := 0
	for _, a := range list {
		m, ok := a.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: schema at %s: %s entry is not an object", ErrUnsupportedSchema, path, key)
		}
		if t, _ := m["type"].(string); t == types.SchemaNull && len(m) == 1 {
			nulls++
			continue
		}
		if only != nil {
			return fmt.Errorf("%w: schema at %s uses %s of several types, which is not supported", ErrUnsupportedSchema, path, key)
		}
		only = m
	}
	if only == nil || nulls != 1 {
		return fmt.Errorf("%w: schema at %s uses %s other than one type or null, which is not supported", ErrUnsupportedSchema, path, key)
	}
	delete(node, key)
	for k, v := range only {
		if _, set := node[k]; !set {
			node[k] = v
		}
	}
	node["nullable"] = true
	return nil
}
