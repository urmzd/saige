package mcp

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"

	"github.com/urmzd/saige/agent/types"
)

// JSON Schema keywords ParseInputSchema reads.
const (
	kwType                 = "type"
	kwProperties           = "properties"
	kwRef                  = "$ref"
	kwAnyOf                = "anyOf"
	kwOneOf                = "oneOf"
	kwAllOf                = "allOf"
	kwEnum                 = "enum"
	kwNullable             = "nullable"
	kwItems                = "items"
	kwAdditionalProperties = "additionalProperties"
)

// MaxSchemaDepth bounds how deeply ParseInputSchema follows nested
// properties and array items.
const MaxSchemaDepth = 32

// annotationKeywords carry no validation meaning, so ParseInputSchema may
// drop them. "format" is an annotation by default since draft 2019-09.
var annotationKeywords = map[string]bool{
	"title": true, "description": true, "default": true, "examples": true,
	"$comment": true, "$schema": true, "$id": true, "deprecated": true,
	"readOnly": true, "writeOnly": true, "format": true,
}

// propertyKeywords are the validation keywords PropertyDef models, after
// unions and type lists are folded.
var propertyKeywords = map[string]bool{
	kwType: true, kwNullable: true, kwEnum: true, kwItems: true,
	kwProperties: true, "required": true, kwAdditionalProperties: true,
}

// rootKeywords are the validation keywords ParameterSchema models.
var rootKeywords = map[string]bool{
	kwType: true, kwProperties: true, "required": true, kwAdditionalProperties: true,
}

var schemaTypes = map[string]bool{
	types.SchemaString: true, types.SchemaInteger: true, types.SchemaNumber: true,
	types.SchemaBoolean: true, types.SchemaArray: true, types.SchemaObject: true,
	types.SchemaNull: true,
}

// ParseInputSchema converts an MCP tool input schema into a ParameterSchema
// that accepts the same arguments, or refuses it. Use it where a tool must
// never reach a model with a schema that means something else. The tool
// import in this package uses a lenient conversion that approximates
// instead.
//
// The accepted subset:
//   - Local "#/$defs/<name>" references, inlined first (see InlineDefs).
//     Only annotations may sit beside a $ref.
//   - A nullable property: anyOf or oneOf of one schema and
//     {"type":"null"}, a type list of one type and "null", or
//     "nullable": true. Beside such a union, only annotations and keywords
//     the member does not set (or sets to the same value) may appear, and
//     never type, enum or nullable.
//   - Exactly one type per property, from the JSON Schema type names.
//   - String enums. A null member counts only where the type allows null.
//   - additionalProperties as true or false. False is read as true: the
//     model may send a key the server then rejects. This is the one
//     loosening, kept because zod writes it on every object.
//   - Annotations (title, description, default, examples, format, $comment,
//     $schema, $id, deprecated, readOnly, writeOnly), which are dropped or
//     kept as PropertyDef allows.
//
// Everything else is refused: other unions, allOf, any other keyword
// (const, pattern, minimum, additionalProperties as a schema, and so on), a
// property with no type, nesting deeper than MaxSchemaDepth, and a root that
// is not a non-nullable object. Every refusal wraps ErrUnsupportedSchema and
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
	if doc == nil {
		return types.ParameterSchema{Type: types.SchemaObject}, stats, nil
	}
	// InlineDefs lets keywords beside a $ref override the definition's.
	// That is exact only for annotations, so check before inlining.
	if err := checkRefSiblings(doc, "$"); err != nil {
		return types.ParameterSchema{}, stats, err
	}
	doc, stats, err := InlineDefs(doc)
	if err != nil {
		return types.ParameterSchema{}, stats, err
	}
	if err := normalizeRoot(doc); err != nil {
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
	return s, stats, nil
}

// checkRefSiblings refuses a $ref with a validation keyword beside it.
func checkRefSiblings(v any, path string) error {
	switch n := v.(type) {
	case []any:
		for i, e := range n {
			if err := checkRefSiblings(e, path+"["+strconv.Itoa(i)+"]"); err != nil {
				return err
			}
		}
	case map[string]any:
		if _, ok := n[kwRef]; ok {
			for _, k := range sortedKeys(n) {
				if k != kwRef && !annotationKeywords[k] {
					return fmt.Errorf("%w: schema at %s uses %s beside $ref, which is not supported", ErrUnsupportedSchema, path, k)
				}
			}
		}
		for _, k := range sortedKeys(n) {
			if err := checkRefSiblings(n[k], childPath(path, k)); err != nil {
				return err
			}
		}
	}
	return nil
}

func normalizeRoot(doc map[string]any) error {
	switch t := doc[kwType].(type) {
	case nil:
		if _, set := doc[kwType]; set {
			return fmt.Errorf("%w: schema at $ has type null, want object", ErrUnsupportedSchema)
		}
	case string:
		if t != types.SchemaObject {
			return fmt.Errorf("%w: schema at $ has type %q, want object", ErrUnsupportedSchema, t)
		}
	default:
		return fmt.Errorf("%w: schema at $ must have the single type object", ErrUnsupportedSchema)
	}
	if err := checkKeywords(doc, "$", rootKeywords); err != nil {
		return err
	}
	return normalizeChildren(doc, "$", 0)
}

// normalizeProperty rewrites node in place into the form PropertyDef reads,
// or returns the reason it cannot. See ParseInputSchema.
func normalizeProperty(node map[string]any, path string, depth int) error {
	if depth > MaxSchemaDepth {
		return fmt.Errorf("%w: schema at %s nests deeper than %d levels", ErrUnsupportedSchema, path, MaxSchemaDepth)
	}
	if _, ok := node[kwRef]; ok {
		// InlineDefs resolves or refuses every $ref first; this guards the
		// invariant.
		return fmt.Errorf("%w: schema at %s uses $ref, which is not supported", ErrUnsupportedSchema, path)
	}
	if _, ok := node[kwAllOf]; ok {
		return fmt.Errorf("%w: schema at %s uses allOf, which is not supported", ErrUnsupportedSchema, path)
	}
	_, anyOf := node[kwAnyOf]
	_, oneOf := node[kwOneOf]
	if anyOf && oneOf {
		return fmt.Errorf("%w: schema at %s uses both anyOf and oneOf, which is not supported", ErrUnsupportedSchema, path)
	}
	unionNull := false
	for _, key := range []string{kwAnyOf, kwOneOf} {
		if _, ok := node[key]; ok {
			if err := foldNullableUnion(node, key, path); err != nil {
				return err
			}
			unionNull = true
		}
	}

	typeNull, err := foldType(node, path)
	if err != nil {
		return err
	}
	explicitNull := false
	if v, ok := node[kwNullable]; ok {
		b, ok := v.(bool)
		if !ok {
			return fmt.Errorf("%w: schema at %s has nullable that is not a boolean", ErrUnsupportedSchema, path)
		}
		explicitNull = b
	}
	nullable := unionNull || typeNull || explicitNull
	if _, ok := node[kwEnum]; ok {
		// The type and the enum must both allow null; a union's null branch
		// stands apart from both.
		enumNull, err := foldEnum(node, path)
		if err != nil {
			return err
		}
		nullable = unionNull || (typeNull || explicitNull) && enumNull
	}
	if nullable {
		node[kwNullable] = true
	} else {
		delete(node, kwNullable)
	}

	if err := checkKeywords(node, path, propertyKeywords); err != nil {
		return err
	}
	return normalizeChildren(node, path, depth)
}

// foldType reduces node's type to one name, and reports whether a type list
// also allowed null.
func foldType(node map[string]any, path string) (bool, error) {
	switch t := node[kwType].(type) {
	case nil:
		return false, fmt.Errorf("%w: schema at %s has no type, which is not supported", ErrUnsupportedSchema, path)
	case string:
		if !schemaTypes[t] {
			return false, fmt.Errorf("%w: schema at %s has type %q, which is not supported", ErrUnsupportedSchema, path, t)
		}
		return false, nil
	case []any:
		only, nulls := "", 0
		for _, e := range t {
			s, _ := e.(string)
			switch {
			case s == types.SchemaNull:
				nulls++
			case schemaTypes[s] && only == "":
				only = s
			default:
				return false, fmt.Errorf("%w: schema at %s has several types, which is not supported", ErrUnsupportedSchema, path)
			}
		}
		if only == "" || nulls > 1 {
			return false, fmt.Errorf("%w: schema at %s has no single type", ErrUnsupportedSchema, path)
		}
		node[kwType] = only
		return nulls == 1, nil
	default:
		return false, fmt.Errorf("%w: schema at %s has a type that is not a string or a list of strings", ErrUnsupportedSchema, path)
	}
}

// foldEnum checks node's enum is a list of strings on a string type, removes
// a null member, and reports whether there was one.
func foldEnum(node map[string]any, path string) (bool, error) {
	list, ok := node[kwEnum].([]any)
	if !ok || len(list) == 0 {
		return false, fmt.Errorf("%w: schema at %s has an enum that is not a non-empty list", ErrUnsupportedSchema, path)
	}
	if node[kwType] != types.SchemaString {
		return false, fmt.Errorf("%w: schema at %s uses enum on type %v, which is not supported: only string enums are", ErrUnsupportedSchema, path, node[kwType])
	}
	values := make([]any, 0, len(list))
	enumNull := false
	for _, e := range list {
		switch s := e.(type) {
		case string:
			values = append(values, s)
		case nil:
			enumNull = true
		default:
			return false, fmt.Errorf("%w: schema at %s has enum member %v, which is not a string", ErrUnsupportedSchema, path, e)
		}
	}
	if len(values) == 0 {
		return false, fmt.Errorf("%w: schema at %s has an enum with no string member", ErrUnsupportedSchema, path)
	}
	node[kwEnum] = values
	return enumNull, nil
}

// checkKeywords refuses any keyword that is neither modelled nor an
// annotation, and additionalProperties that is not a boolean.
func checkKeywords(node map[string]any, path string, modelled map[string]bool) error {
	for _, k := range sortedKeys(node) {
		if !modelled[k] && !annotationKeywords[k] {
			return fmt.Errorf("%w: schema at %s uses %s, which is not supported", ErrUnsupportedSchema, path, k)
		}
	}
	if v, ok := node[kwAdditionalProperties]; ok {
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%w: schema at %s uses additionalProperties as a schema, which is not supported", ErrUnsupportedSchema, path)
		}
	}
	return nil
}

func normalizeChildren(node map[string]any, path string, depth int) error {
	if v, ok := node[kwProperties]; ok {
		props, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: schema at %s.properties is not an object", ErrUnsupportedSchema, path)
		}
		for _, name := range sortedKeys(props) {
			p, ok := props[name].(map[string]any)
			if !ok {
				return fmt.Errorf("%w: schema at %s is not an object", ErrUnsupportedSchema, path+"."+name)
			}
			if err := normalizeProperty(p, path+"."+name, depth+1); err != nil {
				return err
			}
		}
	}
	if v, ok := node[kwItems]; ok {
		items, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: schema at %s[] is not a single schema, which is not supported", ErrUnsupportedSchema, path)
		}
		if err := normalizeProperty(items, path+"[]", depth+1); err != nil {
			return err
		}
	}
	return nil
}

// foldNullableUnion replaces node[key], an anyOf or oneOf of one schema and
// {"type":"null"}, by that schema. Keywords beside the union combine with
// the member's as JSON Schema does (both apply), so only an annotation may
// differ, and the node wins it. type, enum and nullable beside the union
// would exclude its null branch, so they are refused.
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
		if t, _ := m[kwType].(string); t == types.SchemaNull && len(m) == 1 {
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
	for _, k := range []string{kwAllOf, kwAnyOf, kwOneOf, kwRef} {
		if _, ok := only[k]; ok {
			return fmt.Errorf("%w: schema at %s uses %s inside %s, which is not supported", ErrUnsupportedSchema, path, k, key)
		}
	}
	for _, k := range []string{kwType, kwEnum, kwNullable} {
		if _, ok := node[k]; ok {
			return fmt.Errorf("%w: schema at %s sets %s beside %s, which is not supported", ErrUnsupportedSchema, path, k, key)
		}
	}
	delete(node, key)
	for _, k := range sortedKeys(only) {
		v := only[k]
		if cur, set := node[k]; set {
			if annotationKeywords[k] || reflect.DeepEqual(cur, v) {
				continue
			}
			return fmt.Errorf("%w: schema at %s sets %s both beside %s and inside it, which is not supported", ErrUnsupportedSchema, path, k, key)
		}
		node[k] = v
	}
	return nil
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
