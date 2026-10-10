package definition

import _ "embed"

// schemaJSON is the JSON Schema of the frontmatter, for editors and for
// validating definitions outside Go. A test keeps it in step with the Go
// types.
//
//go:embed definition.schema.json
var schemaJSON []byte

// Schema returns the JSON Schema of a definition's frontmatter.
func Schema() []byte { return append([]byte(nil), schemaJSON...) }
