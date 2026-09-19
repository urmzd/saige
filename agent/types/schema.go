package types

import (
	"reflect"
	"strings"
)

// SchemaFrom derives a ParameterSchema from a Go struct type using reflection.
//
// Field mapping rules:
//   - json tag → field name (json:"-" skips the field)
//   - json:"name,omitempty" → field is optional (not in required); no omitempty → required
//   - description tag → PropertyDef.Description
//   - enum tag → PropertyDef.Enum (comma-separated values)
//   - Type mapping: string→"string", int*/uint*→"integer", float*→"number",
//     bool→"boolean", slice→"array", struct→"object"
//   - Pointer types permit null; omitempty still controls field presence
//   - Nested structs and slice element types are recursed into
func SchemaFrom[T any]() ParameterSchema {
	var zero T
	t := reflect.TypeOf(zero)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	props, required := structToProperties(t)
	return ParameterSchema{
		Type:       "object",
		Required:   required,
		Properties: props,
	}
}

func structToProperties(t reflect.Type) (map[string]PropertyDef, []string) {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	props := make(map[string]PropertyDef)
	var required []string

	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}

		jsonTag := f.Tag.Get("json")
		if jsonTag == "-" {
			continue
		}

		name, opts := parseTag(jsonTag)
		if name == "" {
			name = f.Name
		}

		omitempty := strings.Contains(opts, "omitempty")
		if !omitempty {
			required = append(required, name)
		}

		pd := typeToPropertyDef(f.Type)

		if desc := f.Tag.Get("description"); desc != "" {
			pd.Description = desc
		}
		if enum := f.Tag.Get("enum"); enum != "" {
			pd.Enum = strings.Split(enum, ",")
		}

		props[name] = pd
	}

	return props, required
}

func typeToPropertyDef(t reflect.Type) PropertyDef {
	if t.Kind() == reflect.Ptr {
		property := typeToPropertyDef(t.Elem())
		property.Nullable = true
		return property
	}

	switch t.Kind() {
	case reflect.String:
		return PropertyDef{Type: "string"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return PropertyDef{Type: "integer"}
	case reflect.Float32, reflect.Float64:
		return PropertyDef{Type: "number"}
	case reflect.Bool:
		return PropertyDef{Type: "boolean"}
	case reflect.Slice, reflect.Array:
		items := typeToPropertyDef(t.Elem())
		return PropertyDef{Type: "array", Items: &items}
	case reflect.Struct:
		props, req := structToProperties(t)
		return PropertyDef{Type: "object", Properties: props, Required: req}
	default:
		return PropertyDef{Type: "string"}
	}
}

func parseTag(tag string) (string, string) {
	name, opts, _ := strings.Cut(tag, ",")
	return name, opts
}
