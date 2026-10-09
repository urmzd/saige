package types

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// VersionedTool is an OPTIONAL interface a tool implements to report a
// content version: a value that changes whenever what the model is told
// about the tool, or what the tool does, changes. A revisioned registry uses
// it to add a revision only when the version changes, and the agent loop
// records it next to each result so a transcript names the version that ran.
type VersionedTool interface {
	Tool
	Version() string
}

// IdempotentTool is an OPTIONAL interface a tool implements to declare that
// repeating a call with the same arguments has the same effect as making it
// once. A durable engine may then run the call again after a crash left its
// outcome unknown, instead of waiting for the host to reconcile it.
type IdempotentTool interface {
	Tool
	Idempotent() bool
}

// ToolVersion returns the version a tool reports, looking through markers
// and decorators that implement Unwrap. It is empty for a tool that reports
// none.
func ToolVersion(t Tool) string {
	if v, ok := findTool[VersionedTool](t); ok {
		return v.Version()
	}
	return ""
}

// IsIdempotent reports whether a tool, or a tool it wraps, declares itself
// idempotent.
func IsIdempotent(t Tool) bool {
	if v, ok := findTool[IdempotentTool](t); ok {
		return v.Idempotent()
	}
	return false
}

// findTool is As that also steps into a MarkedTool, which wraps through a
// field rather than an Unwrap method.
func findTool[T any](t Tool) (T, bool) {
	for t != nil {
		if v, ok := As[T](t); ok {
			return v, true
		}
		m, ok := As[*MarkedTool](t)
		if !ok {
			break
		}
		t = m.Inner
	}
	var zero T
	return zero, false
}

// DefinitionHash returns a short, stable hash of a tool definition: its
// name, description, parameter schema and capability class. Two definitions
// hash alike exactly when the model and the policies see the same tool.
func DefinitionHash(def ToolDef) string {
	// encoding/json sorts map keys, so the encoding is canonical.
	raw, err := json.Marshal(struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  ParameterSchema `json:"parameters"`
		Capability  ToolCapability  `json:"capability"`
	}{def.Name, def.Description, def.Parameters, def.Capability.Effective()})
	if err != nil {
		// A Default JSON cannot encode. %v prints maps in key order, so
		// this fallback is stable too.
		raw = fmt.Appendf(nil, "%v", def)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}
