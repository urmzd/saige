package types

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// ErrSchemaMismatch identifies a value that does not satisfy a schema.
var ErrSchemaMismatch = errors.New("value does not match schema")

// ValidateJSON checks a decoded JSON value against schema, recursively.
// Unlike ValidateToolArgs it descends into nested objects and array items and
// enforces enums, so it suits a final structured answer that callers decode
// into typed values. Properties the schema does not declare are accepted, and
// an empty type accepts any value. Numbers may be float64 or json.Number.
//
// The error wraps ErrSchemaMismatch and lists every problem with its JSON
// path in a stable order, so a model can fix all of them in one retry.
func ValidateJSON(schema ParameterSchema, value any) error {
	root := PropertyDef{Type: schema.Type, Properties: schema.Properties, Required: schema.Required}
	if root.Type == "" && len(root.Properties) > 0 {
		root.Type = SchemaObject
	}
	var problems []string
	validateValue(root, value, "$", &problems)
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrSchemaMismatch, strings.Join(problems, "; "))
}

func validateValue(p PropertyDef, value any, path string, problems *[]string) {
	if value == nil {
		if p.Nullable || p.Type == "" || p.Type == SchemaNull || p.Type == "any" {
			return
		}
		*problems = append(*problems, fmt.Sprintf("%s must be %s, got null", path, p.Type))
		return
	}
	if !matchesJSONType(p.Type, value) {
		*problems = append(*problems, fmt.Sprintf("%s must be %s, got %s", path, p.Type, jsonTypeName(value)))
		return
	}
	if len(p.Enum) > 0 && !slices.Contains(p.Enum, enumKey(value)) {
		*problems = append(*problems, fmt.Sprintf("%s must be one of %q", path, p.Enum))
	}
	switch v := value.(type) {
	case map[string]any:
		for _, name := range p.Required {
			if _, ok := v[name]; !ok {
				*problems = append(*problems, fmt.Sprintf("%s missing required property %q", path, name))
			}
		}
		names := make([]string, 0, len(v))
		for name := range v {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			prop, declared := p.Properties[name]
			if !declared {
				continue
			}
			// An optional property sent as null means absent, as in
			// ValidateToolArgs.
			if v[name] == nil && !slices.Contains(p.Required, name) {
				continue
			}
			validateValue(prop, v[name], path+"."+name, problems)
		}
	case []any:
		if p.Items == nil {
			return
		}
		for i, item := range v {
			validateValue(*p.Items, item, fmt.Sprintf("%s[%d]", path, i), problems)
		}
	}
}

// enumKey renders a scalar the way PropertyDef.Enum stores it.
func enumKey(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	if f, ok := numberValue(value); ok {
		return fmt.Sprint(f)
	}
	return fmt.Sprint(value)
}
