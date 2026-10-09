package types

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
)

// ErrInvalidToolArguments identifies tool arguments that cannot be decoded or
// do not match the tool's declared parameters.
var ErrInvalidToolArguments = errors.New("invalid tool arguments")

// ValidateToolArgs checks args against a tool's parameter schema before the
// tool runs. The check is deliberately shallow: it reports missing required
// properties and top-level values whose JSON type differs from the declared
// one. It does not enforce enums, nested schemas, or formats, and it accepts
// properties the schema does not declare.
//
// A nullable property accepts null. An optional property may also be null,
// which is treated as absent, because models often send null for a field they
// mean to omit. An empty schema type or property type accepts any value.
//
// The error wraps ErrInvalidToolArguments and names every failing property in
// sorted order, so the model can correct all of them in one retry.
func ValidateToolArgs(schema ParameterSchema, args map[string]any) error {
	if schema.Type != "" && schema.Type != SchemaObject {
		return nil
	}
	var problems []string
	for _, name := range schema.Required {
		if _, ok := args[name]; !ok {
			problems = append(problems, fmt.Sprintf("missing required property %q", name))
		}
	}
	required := make(map[string]bool, len(schema.Required))
	for _, name := range schema.Required {
		required[name] = true
	}
	names := make([]string, 0, len(args))
	for name := range args {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		prop, declared := schema.Properties[name]
		if !declared {
			continue
		}
		value := args[name]
		if value == nil {
			if prop.Nullable || prop.Type == "" || prop.Type == SchemaNull || !required[name] {
				continue
			}
			problems = append(problems, fmt.Sprintf("property %q must be %s, got null", name, prop.Type))
			continue
		}
		if !matchesJSONType(prop.Type, value) {
			problems = append(problems, fmt.Sprintf("property %q must be %s, got %s", name, prop.Type, jsonTypeName(value)))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrInvalidToolArguments, strings.Join(problems, "; "))
}

// matchesJSONType reports whether a decoded value fits a JSON Schema type.
// It accepts the shapes encoding/json produces (float64, json.Number,
// map[string]any, []any) and the native Go values hand-written callers pass.
func matchesJSONType(typ string, value any) bool {
	switch typ {
	case "", "any":
		return true
	case SchemaString:
		_, ok := value.(string)
		return ok
	case SchemaBoolean:
		_, ok := value.(bool)
		return ok
	case SchemaNumber:
		_, ok := numberValue(value)
		return ok
	case SchemaInteger:
		f, ok := numberValue(value)
		return ok && f == math.Trunc(f) && !math.IsInf(f, 0)
	case SchemaNull:
		return value == nil
	}
	rv := reflect.ValueOf(value)
	switch typ {
	case SchemaObject:
		return rv.Kind() == reflect.Map || rv.Kind() == reflect.Struct
	case SchemaArray:
		return rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array
	default:
		// An unknown type keyword is not this check's to enforce.
		return true
	}
}

// numberValue converts any numeric representation to float64.
func numberValue(value any) (float64, bool) {
	switch v := value.(type) {
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case float64:
		return v, true
	case float32:
		return float64(v), true
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint()), true
	default:
		return 0, false
	}
}

// jsonTypeName names a value's JSON type for error messages.
func jsonTypeName(value any) string {
	switch value.(type) {
	case string:
		return SchemaString
	case bool:
		return SchemaBoolean
	}
	if _, ok := numberValue(value); ok {
		return SchemaNumber
	}
	switch reflect.ValueOf(value).Kind() {
	case reflect.Map, reflect.Struct:
		return SchemaObject
	case reflect.Slice, reflect.Array:
		return SchemaArray
	default:
		return fmt.Sprintf("%T", value)
	}
}
