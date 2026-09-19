# Nullable tool properties

Set `Nullable: true` when a property accepts JSON null.
Keep `Type` as the non-null type. Existing string assignments remain valid.

```go
parameters := types.ParameterSchema{
    Type: "object",
    Required: []string{"id", "assignee_id"},
    Properties: map[string]types.PropertyDef{
        "id":          {Type: "string"},
        "assignee_id": {Type: "string", Nullable: true},
    },
}
```

This definition permits an explicit null assignee, which can mean "remove the assignment" in the tool's contract.
It does not permit omission of `assignee_id`, because the parent schema lists that field as required.

For an optional list filter, omit `assignee_id` from `Required` and retain `Nullable: true`.
The tool can then distinguish an omitted filter from an explicit null filter.

```go
value, present := args["assignee_id"]
switch {
case !present:
    // No assignee filter was supplied.
case value == nil:
    // The caller explicitly requested the null filter.
default:
    // Validate and use the supplied assignee ID.
}
```

## Why a separate flag

Nullability describes a permitted value. Required fields describe presence.
Combining these rules would make it impossible to request a required field whose value can be null.
A separate flag also preserves the existing `Type: "string"` Go API.
This change supports one base type plus null, not arbitrary unions of non-null types.

## Provider encoding

| Boundary | Encoding |
|---|---|
| OpenAI, Anthropic, Ollama | `"type": ["string", "null"]` |
| Google Gemini | `"type": "STRING", "nullable": true` |
| MCP export | Standard JSON Schema type union |
| MCP import | Preserve null in a type array, regardless of its position |
| Response cache | Include nullability in the request identity |

Nested properties, nullable objects, and nullable array elements follow the same rule.
OpenAI strict response schemas still close nullable objects and check their required fields.
Anthropic tool schemas preserve the parent required-field list.

A nullable enum permits its listed strings plus null.
For example, `Enum: []string{"open", "closed"}` with `Nullable: true` accepts `"open"`, `"closed"`, and `null`.
Other strings remain invalid.
JSON Schema needs null in the enum as well as the type union. See the [enum reference](https://json-schema.org/understanding-json-schema/reference/enum).

Use `PropertyDef.JSONSchema()` to obtain the provider-neutral JSON Schema map.
Ordinary `json.Marshal(PropertyDef)` retains the Go definition format, including the nullable flag.
It is not a substitute for `JSONSchema()` at a standard JSON Schema boundary.

## Struct-derived schemas

`SchemaFrom` marks pointer fields as nullable.
The `omitempty` tag still determines whether the field is required.
Pointer elements inside arrays and nested objects also retain nullability.

```go
type Request struct {
    Owner  *string `json:"owner"`            // Required; string or null.
    Filter *string `json:"filter,omitempty"` // Optional; string or null.
    Label  string  `json:"label,omitempty"`  // Optional; string only.
}
```

Existing pointer-derived schemas now advertise null explicitly.
Non-pointer fields keep their previous type behavior.
A plain pointer cannot distinguish absent from null after Go struct decoding; use the argument map when that distinction matters.

## Verification and limits

Tests validate missing, null, valid string, invalid string, and wrong-type values against the generated JSON Schema.
Provider tests inspect serialized tool definitions. Other tests cover nested fields, strict responses, MCP conversion, and cache identity.
These checks need no API key.

A schema describes permitted input. The tool must still validate the actual arguments and apply its own domain rules.
The tests do not claim that every live model or server follows the schema correctly.
Consumers pinned to an older saige release need an upgrade and must set the nullable flag in their contract conversion.
