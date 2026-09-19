package types

// JSONSchema returns the property's JSON Schema representation. Nullable adds
// null to both the type and any enum without changing field presence rules.
// Type remains a string in PropertyDef so existing Go definitions keep working.
func (p PropertyDef) JSONSchema() map[string]any {
	schema := map[string]any{"type": p.Type}
	if p.Nullable && p.Type != "null" {
		schema["type"] = []string{p.Type, "null"}
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
