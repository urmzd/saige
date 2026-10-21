package mcp

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// pydanticSchema is the shape pydantic emits for a tool taking a nested
// model: shared shapes in $defs, used by plain, list, Optional and
// described references, one definition referring to another, and one unused.
var pydanticSchema = map[string]any{
	"type":     "object",
	"title":    "CreateOrderArgs",
	"required": []any{"customer", "lines"},
	"$defs": map[string]any{
		"Address": map[string]any{"type": "object", "title": "Address", "required": []any{"city"}, "properties": map[string]any{
			"city": map[string]any{"type": "string"},
			"zip":  map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "null"}}},
		}},
		"Customer": map[string]any{"type": "object", "description": "Who orders", "properties": map[string]any{
			"name":    map[string]any{"type": "string"},
			"address": map[string]any{"$ref": "#/$defs/Address"},
		}},
		"Line": map[string]any{"type": "object", "properties": map[string]any{
			"sku": map[string]any{"type": "string"},
			"qty": map[string]any{"type": "integer"},
		}},
		"Kind":   map[string]any{"type": "string", "enum": []any{"retail", "wholesale"}},
		"Unused": map[string]any{"allOf": []any{}},
	},
	"properties": map[string]any{
		"customer": map[string]any{"$ref": "#/$defs/Customer"},
		"lines":    map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/Line"}},
		"ship_to":  map[string]any{"anyOf": []any{map[string]any{"$ref": "#/$defs/Address"}, map[string]any{"type": "null"}}},
		"kind":     map[string]any{"$ref": "#/$defs/Kind", "description": "Order kind", "default": "retail"},
	},
}

var recursiveSchema = map[string]any{
	"type": "object",
	"$defs": map[string]any{"node": map[string]any{"type": "object", "properties": map[string]any{
		"children": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/node"}},
	}}},
	"properties": map[string]any{"root": map[string]any{"$ref": "#/$defs/node"}},
}

func TestLocalDefsReferencesAreInlinedWithTheSchemaTheyName(t *testing.T) {
	before, _ := json.Marshal(pydanticSchema)
	doc, stats, err := InlineDefs(pydanticSchema)
	if err != nil {
		t.Fatalf("a pydantic-style $defs schema was refused: %v", err)
	}
	if stats != (InlineStats{Defs: 5, Inlined: 5}) {
		t.Errorf("stats = %+v, want 5 definitions and 5 references", stats)
	}
	if _, ok := doc["$defs"]; ok {
		t.Error("$defs was kept")
	}
	if after, _ := json.Marshal(pydanticSchema); string(after) != string(before) {
		t.Error("InlineDefs modified its input")
	}
	if raw, _ := json.Marshal(doc); strings.Contains(string(raw), "$ref") {
		t.Errorf("a reference survived: %s", raw)
	}

	params := schemaFromMCP(pydanticSchema)
	p := params.Properties
	cust := p["customer"]
	if cust.Type != "object" || cust.Description != "Who orders" || cust.Properties["name"].Type != "string" {
		t.Errorf("customer = %+v, want the Customer definition", cust)
	}
	addr := cust.Properties["address"]
	if addr.Type != "object" || addr.Properties["city"].Type != "string" || !slices.Equal(addr.Required, []string{"city"}) {
		t.Errorf("customer.address = %+v, want the Address definition, reached through Customer", addr)
	}
	if zip := addr.Properties["zip"]; zip.Type != "string" || !zip.Nullable {
		t.Errorf("customer.address.zip = %+v, want a nullable string", zip)
	}
	if it := p["lines"].Items; it == nil || it.Type != "object" || it.Properties["qty"].Type != "integer" {
		t.Errorf("lines items = %+v, want the Line definition", it)
	}
	if ship := p["ship_to"]; ship.Type != "object" || !ship.Nullable || ship.Properties["city"].Type != "string" {
		t.Errorf("ship_to = %+v, want a nullable Address", ship)
	}
	kind := p["kind"]
	if kind.Type != "string" || !slices.Equal(kind.Enum, []string{"retail", "wholesale"}) {
		t.Errorf("kind = %+v, want the Kind enum", kind)
	}
	if kind.Description != "Order kind" || kind.Default != "retail" {
		t.Errorf("kind = %+v, want the keywords beside $ref kept", kind)
	}
	if !slices.Equal(params.Required, []string{"customer", "lines"}) {
		t.Errorf("required = %v", params.Required)
	}
}

func TestADefinitionUsedTwiceIsCopiedNotShared(t *testing.T) {
	schema := map[string]any{
		"type":  "object",
		"$defs": map[string]any{"N": map[string]any{"anyOf": []any{map[string]any{"type": "number"}, map[string]any{"type": "null"}}}},
		"properties": map[string]any{
			"a": map[string]any{"$ref": "#/$defs/N", "description": "first"},
			"b": map[string]any{"$ref": "#/$defs/N", "description": "second"},
		},
	}
	doc, _, err := InlineDefs(schema)
	if err != nil {
		t.Fatal(err)
	}
	props := doc["properties"].(map[string]any)
	a, b := props["a"].(map[string]any), props["b"].(map[string]any)
	a["anyOf"].([]any)[0].(map[string]any)["type"] = "string"
	if b["anyOf"].([]any)[0].(map[string]any)["type"] != "number" {
		t.Fatal("the two uses share one copy of the definition")
	}

	params := schemaFromMCP(schema)
	pa, pb := params.Properties["a"], params.Properties["b"]
	if pa.Description != "first" || pb.Description != "second" || pa.Type != "number" || pb.Type != "number" || !pa.Nullable || !pb.Nullable {
		t.Errorf("a = %+v, b = %+v, want two independent nullable numbers", pa, pb)
	}
}

func TestARootReferenceIsInlined(t *testing.T) {
	doc, stats, err := InlineDefs(map[string]any{
		"$ref":  "#/$defs/Args",
		"$defs": map[string]any{"Args": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}
	if !reflect.DeepEqual(doc, want) || stats.Inlined != 1 {
		t.Errorf("doc = %v (%+v), want %v", doc, stats, want)
	}
}

func TestAPointerEscapedDefinitionNameResolves(t *testing.T) {
	doc, _, err := InlineDefs(map[string]any{
		"type":       "object",
		"$defs":      map[string]any{"a/b~c": map[string]any{"type": "integer"}},
		"properties": map[string]any{"v": map[string]any{"$ref": "#/$defs/a~1b~0c"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := doc["properties"].(map[string]any)["v"]; !reflect.DeepEqual(got, map[string]any{"type": "integer"}) {
		t.Errorf("v = %v", got)
	}
}

func TestAnUnresolvableReferenceIsRefusedWithItsReason(t *testing.T) {
	str := map[string]any{"type": "string"}
	for name, tc := range map[string]struct {
		schema map[string]any
		want   string
	}{
		"not_local": {map[string]any{"type": "object", "properties": map[string]any{
			"v": map[string]any{"$ref": "https://example.com/s.json"},
		}}, `$.v uses $ref "https://example.com/s.json"`},
		"definitions_keyword": {map[string]any{"type": "object", "definitions": map[string]any{"s": str}, "properties": map[string]any{
			"v": map[string]any{"$ref": "#/definitions/s"},
		}}, "only local #/$defs/<name> references are resolved"},
		"missing": {map[string]any{"type": "object", "$defs": map[string]any{"a": str}, "properties": map[string]any{
			"v": map[string]any{"$ref": "#/$defs/b"},
		}}, "names no definition in $defs (have a)"},
		"no_defs": {map[string]any{"type": "object", "properties": map[string]any{
			"v": map[string]any{"$ref": "#/$defs/b"},
		}}, "names no definition in $defs (have none)"},
		"into_a_definition": {map[string]any{"type": "object", "$defs": map[string]any{"a": map[string]any{"type": "object", "properties": map[string]any{"x": str}}}, "properties": map[string]any{
			"v": map[string]any{"$ref": "#/$defs/a/properties/x"},
		}}, "names no definition"},
		"self": {recursiveSchema, "recursive (node -> node)"},
		"mutual": {map[string]any{"type": "object", "$defs": map[string]any{
			"A": map[string]any{"type": "object", "properties": map[string]any{"b": map[string]any{"$ref": "#/$defs/B"}}},
			"B": map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"$ref": "#/$defs/A"}}},
		}, "properties": map[string]any{"v": map[string]any{"$ref": "#/$defs/A"}}}, "recursive (A -> B -> A)"},
		"not_a_string": {map[string]any{"type": "object", "properties": map[string]any{
			"v": map[string]any{"$ref": 7},
		}}, "not a string"},
		"definition_not_an_object": {map[string]any{"type": "object", "$defs": map[string]any{"a": "string"}, "properties": map[string]any{
			"v": map[string]any{"$ref": "#/$defs/a"},
		}}, "$.$defs.a is not an object"},
		"defs_not_an_object": {map[string]any{"type": "object", "$defs": []any{}}, "$.$defs is not an object"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := InlineDefs(tc.schema)
			if err == nil {
				t.Fatal("an unresolvable reference was accepted")
			}
			if !errors.Is(err, ErrUnsupportedSchema) {
				t.Errorf("err = %v, want ErrUnsupportedSchema", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to say %q", err, tc.want)
			}
		})
	}
}

func TestADefinitionThatExpandsWithoutBoundIsRefused(t *testing.T) {
	// Each level references the one below twice: 2^12 expansions from a
	// dozen definitions.
	defs := map[string]any{"L12": map[string]any{"type": "string"}}
	for i := 11; i >= 0; i-- {
		next := "#/$defs/L" + strconv.Itoa(i+1)
		defs["L"+strconv.Itoa(i)] = map[string]any{"type": "object", "properties": map[string]any{
			"a": map[string]any{"$ref": next}, "b": map[string]any{"$ref": next},
		}}
	}
	_, _, err := InlineDefs(map[string]any{"type": "object", "$defs": defs, "properties": map[string]any{"v": map[string]any{"$ref": "#/$defs/L0"}}})
	if !errors.Is(err, ErrUnsupportedSchema) || !strings.Contains(err.Error(), "expands more than") {
		t.Fatalf("err = %v, want the expansion refused as unbounded", err)
	}
}

// A schema InlineDefs refuses still imports, approximately: one odd tool
// must not break the server's import.
func TestARefusedSchemaStillImportsApproximately(t *testing.T) {
	params := schemaFromMCP(recursiveSchema)
	if params.Type != "object" {
		t.Fatalf("type = %q, want object", params.Type)
	}
	if _, ok := params.Properties["root"]; !ok {
		t.Errorf("properties = %+v, want root kept", params.Properties)
	}
	if doc, _, err := InlineDefs(nil); doc != nil || err != nil {
		t.Errorf("InlineDefs(nil) = %v, %v", doc, err)
	}
}

func TestOnlyANullableUnionCollapses(t *testing.T) {
	for name, tc := range map[string]struct {
		in   map[string]any
		want string
		null bool
	}{
		"oneOf_nullable":  {map[string]any{"oneOf": []any{map[string]any{"type": "null"}, map[string]any{"type": "integer"}}}, "integer", true},
		"two_types":       {map[string]any{"anyOf": []any{map[string]any{"type": "integer"}, map[string]any{"type": "string"}}}, "string", false},
		"two_nulls":       {map[string]any{"anyOf": []any{map[string]any{"type": "integer"}, map[string]any{"type": "null"}, map[string]any{"type": "null"}}}, "string", false},
		"no_null":         {map[string]any{"anyOf": []any{map[string]any{"type": "integer"}}}, "string", false},
		"not_an_object":   {map[string]any{"anyOf": []any{"integer", map[string]any{"type": "null"}}}, "string", false},
		"outer_type_wins": {map[string]any{"type": "number", "anyOf": []any{map[string]any{"type": "integer"}, map[string]any{"type": "null"}}}, "number", true},
	} {
		t.Run(name, func(t *testing.T) {
			p := propertyFromMCP(tc.in)
			if p.Type != tc.want || p.Nullable != tc.null {
				t.Errorf("got %s nullable=%v, want %s nullable=%v", p.Type, p.Nullable, tc.want, tc.null)
			}
		})
	}
}
