package eval

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// JSONSchema is a compiled JSON Schema used by [JSONSchemaScorer] to check
// an output contract.
//
// It supports the validation keywords that describe the shape of a
// structured reply: type (a name or a list of names, including "integer" and
// "null"), nullable, enum, const, required, properties,
// additionalProperties (a boolean or a schema), items, minItems, maxItems,
// minLength, maxLength, minimum, maximum, exclusiveMinimum,
// exclusiveMaximum, pattern, allOf, anyOf, oneOf, and not. The annotation
// keywords $schema, $id, $comment, $defs, definitions, title, description,
// examples, default, deprecated, readOnly, and writeOnly are accepted and
// ignored. [CompileJSONSchema] rejects every other keyword, including $ref
// and a misspelling such as "requird", rather than silently accepting a
// schema that would check less than it appears to.
type JSONSchema struct {
	// always holds the verdict of a boolean schema: true accepts any value,
	// false rejects every value. Nil for an object schema.
	always *bool

	types    []string
	nullable bool
	enum     []any
	hasConst bool
	constVal any

	required      []string
	properties    map[string]*JSONSchema
	additional    *JSONSchema
	hasAdditional bool
	items         *JSONSchema

	minItems, maxItems   *int
	minLength, maxLength *int
	minimum, maximum     *float64
	exclusiveMin         *float64
	exclusiveMax         *float64
	pattern              *regexp.Regexp

	allOf, anyOf, oneOf []*JSONSchema
	not                 *JSONSchema
}

// annotationSchemaKeywords do not constrain the document, so a schema may
// carry them. Subschemas under $defs and definitions are reachable only
// through $ref, which is not supported, so they are not compiled.
var annotationSchemaKeywords = []string{
	"$schema", "$id", "$comment", "$defs", "definitions", "title",
	"description", "examples", "default", "deprecated", "readOnly",
	"writeOnly",
}

// CompileJSONSchema parses and checks a JSON Schema document.
func CompileJSONSchema(raw json.RawMessage) (*JSONSchema, error) {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("json schema: %w", err)
	}
	return compileSchema(doc, "#")
}

func compileSchema(doc any, at string) (*JSONSchema, error) {
	switch v := doc.(type) {
	case bool:
		return &JSONSchema{always: &v}, nil
	case map[string]any:
		return compileObjectSchema(v, at)
	default:
		return nil, fmt.Errorf("json schema %s: must be an object or a boolean", at)
	}
}

//nolint:gocyclo // one branch per JSON Schema keyword
func compileObjectSchema(m map[string]any, at string) (*JSONSchema, error) {
	s := &JSONSchema{}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		value := m[key]
		where := at + "/" + key
		var err error
		switch key {
		case "type":
			s.types, err = schemaTypes(value, where)
		case "nullable":
			b, ok := value.(bool)
			if !ok {
				return nil, fmt.Errorf("json schema %s: must be a boolean", where)
			}
			s.nullable = b
		case "enum":
			list, ok := value.([]any)
			if !ok {
				return nil, fmt.Errorf("json schema %s: must be an array", where)
			}
			s.enum = list
		case "const":
			s.hasConst, s.constVal = true, value
		case "required":
			s.required, err = schemaStrings(value, where)
		case "properties":
			props, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("json schema %s: must be an object", where)
			}
			s.properties = make(map[string]*JSONSchema, len(props))
			for name, sub := range props {
				if s.properties[name], err = compileSchema(sub, where+"/"+name); err != nil {
					return nil, err
				}
			}
		case "additionalProperties":
			s.hasAdditional = true
			s.additional, err = compileSchema(value, where)
		case "items":
			s.items, err = compileSchema(value, where)
		case "minItems":
			s.minItems, err = schemaCount(value, where)
		case "maxItems":
			s.maxItems, err = schemaCount(value, where)
		case "minLength":
			s.minLength, err = schemaCount(value, where)
		case "maxLength":
			s.maxLength, err = schemaCount(value, where)
		case "minimum":
			s.minimum, err = schemaNumber(value, where)
		case "maximum":
			s.maximum, err = schemaNumber(value, where)
		case "exclusiveMinimum":
			s.exclusiveMin, err = schemaNumber(value, where)
		case "exclusiveMaximum":
			s.exclusiveMax, err = schemaNumber(value, where)
		case "pattern":
			p, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("json schema %s: must be a string", where)
			}
			if s.pattern, err = regexp.Compile(p); err != nil {
				return nil, fmt.Errorf("json schema %s: %w", where, err)
			}
		case "allOf":
			s.allOf, err = schemaList(value, where)
		case "anyOf":
			s.anyOf, err = schemaList(value, where)
		case "oneOf":
			s.oneOf, err = schemaList(value, where)
		case "not":
			s.not, err = compileSchema(value, where)
		default:
			if !slices.Contains(annotationSchemaKeywords, key) {
				return nil, fmt.Errorf("json schema %s: keyword %q is not supported", where, key)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return s, nil
}

// JSON Schema type names.
const (
	typeNull    = "null"
	typeBoolean = "boolean"
	typeObject  = "object"
	typeArray   = "array"
	typeNumber  = "number"
	typeInteger = "integer"
	typeString  = "string"
)

var schemaTypeNames = []string{typeNull, typeBoolean, typeObject, typeArray, typeNumber, typeInteger, typeString}

func schemaTypes(v any, at string) ([]string, error) {
	var names []string
	switch t := v.(type) {
	case string:
		names = []string{t}
	case []any:
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("json schema %s: type list must hold strings", at)
			}
			names = append(names, s)
		}
	default:
		return nil, fmt.Errorf("json schema %s: must be a string or an array of strings", at)
	}
	for _, n := range names {
		if !slices.Contains(schemaTypeNames, n) {
			return nil, fmt.Errorf("json schema %s: unknown type %q", at, n)
		}
	}
	return names, nil
}

func schemaStrings(v any, at string) ([]string, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("json schema %s: must be an array of strings", at)
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("json schema %s: must be an array of strings", at)
		}
		out = append(out, s)
	}
	return out, nil
}

func schemaCount(v any, at string) (*int, error) {
	f, ok := v.(float64)
	if !ok || f < 0 || f != math.Trunc(f) {
		return nil, fmt.Errorf("json schema %s: must be a non-negative integer", at)
	}
	n := int(f)
	return &n, nil
}

func schemaNumber(v any, at string) (*float64, error) {
	f, ok := v.(float64)
	if !ok {
		return nil, fmt.Errorf("json schema %s: must be a number", at)
	}
	return &f, nil
}

func schemaList(v any, at string) ([]*JSONSchema, error) {
	list, ok := v.([]any)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("json schema %s: must be a non-empty array of schemas", at)
	}
	out := make([]*JSONSchema, len(list))
	for i, e := range list {
		s, err := compileSchema(e, at+"/"+strconv.Itoa(i))
		if err != nil {
			return nil, err
		}
		out[i] = s
	}
	return out, nil
}

// Validate checks a decoded JSON value (as produced by [json.Unmarshal] into
// an any) and returns one problem per violation, each prefixed with the JSON
// pointer of the offending value. An empty result means the value conforms.
func (s *JSONSchema) Validate(value any) []string {
	var problems []string
	s.validate(normalizeJSONValue(value), "", &problems)
	return problems
}

//nolint:gocyclo // one branch per JSON Schema keyword
func (s *JSONSchema) validate(v any, at string, problems *[]string) {
	report := func(format string, args ...any) {
		where := at
		if where == "" {
			where = "/"
		}
		*problems = append(*problems, where+": "+fmt.Sprintf(format, args...))
	}

	if s.always != nil {
		if !*s.always {
			report("no value is allowed here")
		}
		return
	}
	if v == nil && s.nullable {
		return
	}
	if len(s.types) > 0 && !slices.ContainsFunc(s.types, func(t string) bool { return jsonTypeIs(v, t) }) {
		report("got %s, want %s", jsonTypeName(v), strings.Join(s.types, " or "))
		return
	}
	if s.enum != nil && !slices.ContainsFunc(s.enum, func(e any) bool { return reflect.DeepEqual(e, v) }) {
		report("value %s is not one of the allowed values", compactJSON(v))
	}
	if s.hasConst && !reflect.DeepEqual(s.constVal, v) {
		report("value %s is not %s", compactJSON(v), compactJSON(s.constVal))
	}

	switch val := v.(type) {
	case map[string]any:
		for _, name := range s.required {
			if _, ok := val[name]; !ok {
				report("missing required property %q", name)
			}
		}
		names := make([]string, 0, len(val))
		for name := range val {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			child := at + "/" + escapePointer(name)
			if sub, ok := s.properties[name]; ok {
				sub.validate(val[name], child, problems)
			} else if s.hasAdditional {
				if s.additional.always != nil && !*s.additional.always {
					report("property %q is not allowed", name)
				} else {
					s.additional.validate(val[name], child, problems)
				}
			}
		}
	case []any:
		if s.minItems != nil && len(val) < *s.minItems {
			report("has %d items, want at least %d", len(val), *s.minItems)
		}
		if s.maxItems != nil && len(val) > *s.maxItems {
			report("has %d items, want at most %d", len(val), *s.maxItems)
		}
		if s.items != nil {
			for i, item := range val {
				s.items.validate(item, at+"/"+strconv.Itoa(i), problems)
			}
		}
	case string:
		n := utf8.RuneCountInString(val)
		if s.minLength != nil && n < *s.minLength {
			report("has length %d, want at least %d", n, *s.minLength)
		}
		if s.maxLength != nil && n > *s.maxLength {
			report("has length %d, want at most %d", n, *s.maxLength)
		}
		if s.pattern != nil && !s.pattern.MatchString(val) {
			report("%q does not match pattern %q", excerpt(val, 60), s.pattern.String())
		}
	case float64:
		if s.minimum != nil && val < *s.minimum {
			report("%g is below the minimum %g", val, *s.minimum)
		}
		if s.maximum != nil && val > *s.maximum {
			report("%g is above the maximum %g", val, *s.maximum)
		}
		if s.exclusiveMin != nil && val <= *s.exclusiveMin {
			report("%g is not above %g", val, *s.exclusiveMin)
		}
		if s.exclusiveMax != nil && val >= *s.exclusiveMax {
			report("%g is not below %g", val, *s.exclusiveMax)
		}
	}

	for _, sub := range s.allOf {
		sub.validate(v, at, problems)
	}
	if len(s.anyOf) > 0 && !slices.ContainsFunc(s.anyOf, func(sub *JSONSchema) bool { return len(sub.Validate(v)) == 0 }) {
		report("matches none of the anyOf schemas")
	}
	if len(s.oneOf) > 0 {
		matched := 0
		for _, sub := range s.oneOf {
			if len(sub.Validate(v)) == 0 {
				matched++
			}
		}
		if matched != 1 {
			report("matches %d of the oneOf schemas, want exactly 1", matched)
		}
	}
	if s.not != nil && len(s.not.Validate(v)) == 0 {
		report("matches a schema it must not match")
	}
}

func jsonTypeIs(v any, t string) bool {
	switch t {
	case typeNull:
		return v == nil
	case typeBoolean:
		_, ok := v.(bool)
		return ok
	case typeObject:
		_, ok := v.(map[string]any)
		return ok
	case typeArray:
		_, ok := v.([]any)
		return ok
	case typeNumber:
		_, ok := v.(float64)
		return ok
	case typeInteger:
		f, ok := v.(float64)
		return ok && f == math.Trunc(f) && !math.IsInf(f, 0)
	case typeString:
		_, ok := v.(string)
		return ok
	}
	return false
}

func jsonTypeName(v any) string {
	switch val := v.(type) {
	case nil:
		return typeNull
	case bool:
		return typeBoolean
	case map[string]any:
		return typeObject
	case []any:
		return typeArray
	case float64:
		if val == math.Trunc(val) {
			return typeInteger
		}
		return typeNumber
	case string:
		return typeString
	}
	return fmt.Sprintf("%T", v)
}

// normalizeJSONValue round-trips v through JSON so Go values such as int, a
// struct, or a map holding either compare the way their JSON form would.
func normalizeJSONValue(v any) any {
	switch v.(type) {
	case nil, bool, float64, string:
		return v
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return v
	}
	return out
}

func compactJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return excerpt(string(raw), 80)
}

func escapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}
