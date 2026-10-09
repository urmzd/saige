package types

import (
	"encoding/json"
	"fmt"
)

// jsonNull is the JSON null literal, as it appears in raw encoded values.
const jsonNull = "null"

// JSONSchema returns the property's JSON Schema representation. Nullable adds
// null to both the type and any enum without changing field presence rules.
// Type remains a string in PropertyDef so existing Go definitions keep working.
func (p PropertyDef) JSONSchema() map[string]any {
	schema := map[string]any{"type": p.Type}
	if p.Nullable && p.Type != SchemaNull {
		schema["type"] = []string{p.Type, SchemaNull}
	}
	if p.Description != "" {
		schema["description"] = p.Description
	}
	if len(p.Enum) > 0 {
		if p.Nullable {
			values := make([]any, 0, len(p.Enum)+1)
			for _, value := range p.Enum {
				values = append(values, value)
			}
			schema["enum"] = append(values, nil)
		} else {
			schema["enum"] = append([]string(nil), p.Enum...)
		}
	}
	if p.Default != nil {
		schema["default"] = p.Default
	}
	if p.Items != nil {
		schema["items"] = p.Items.JSONSchema()
	}
	if len(p.Properties) > 0 {
		properties := make(map[string]any, len(p.Properties))
		for name, property := range p.Properties {
			properties[name] = property.JSONSchema()
		}
		schema["properties"] = properties
	}
	if len(p.Required) > 0 {
		schema["required"] = append([]string(nil), p.Required...)
	}
	return schema
}

// UnmarshalJSON accepts standard JSON Schema as well as the PropertyDef
// encoding. A type list containing "null" (for example ["string","null"]) and
// a null enum value both set Nullable. Non-string enum values are kept in
// their JSON text form. Keywords PropertyDef does not model are ignored.
func (p *PropertyDef) UnmarshalJSON(data []byte) error {
	var raw struct {
		Nullable    bool                   `json:"nullable"`
		Type        json.RawMessage        `json:"type"`
		Description string                 `json:"description"`
		Enum        []json.RawMessage      `json:"enum"`
		Items       *PropertyDef           `json:"items"`
		Properties  map[string]PropertyDef `json:"properties"`
		Required    []string               `json:"required"`
		Default     any                    `json:"default"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	out := PropertyDef{
		Nullable:    raw.Nullable,
		Description: raw.Description,
		Items:       raw.Items,
		Properties:  raw.Properties,
		Required:    raw.Required,
		Default:     raw.Default,
	}
	if len(raw.Type) > 0 && string(raw.Type) != jsonNull {
		var single string
		if err := json.Unmarshal(raw.Type, &single); err == nil {
			out.Type = single
		} else {
			var list []string
			if err := json.Unmarshal(raw.Type, &list); err != nil {
				return fmt.Errorf("property type must be a string or a list of strings: %w", err)
			}
			for _, t := range list {
				if t == SchemaNull {
					out.Nullable = true
				} else if out.Type == "" {
					out.Type = t
				}
			}
			if out.Type == "" && out.Nullable {
				out.Type = SchemaNull
				out.Nullable = false
			}
		}
	}
	for _, v := range raw.Enum {
		if string(v) == jsonNull {
			out.Nullable = true
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			out.Enum = append(out.Enum, s)
		} else {
			out.Enum = append(out.Enum, string(v))
		}
	}
	*p = out
	return nil
}
