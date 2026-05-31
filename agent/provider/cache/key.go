package cache

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash"
	"io"
	"sort"

	"github.com/urmzd/saige/agent/types"
)

// Key returns a deterministic sha256 hex digest of the request inputs
// (model, messages, tools, schema). It is stable across process runs: map keys
// are sorted, byte payloads are hashed by content, and every field is
// length-prefixed and type-tagged so distinct inputs cannot collide via
// concatenation. ConfigContent and FeedbackContent are excluded because the
// agent loop strips them before they reach the provider.
func Key(model string, msgs []types.Message, tools []types.ToolDef, schema *types.ParameterSchema) string {
	h := sha256.New()
	writeField(h, "model", []byte(model))

	for _, m := range msgs {
		writeField(h, "role", []byte(m.Role()))
		switch v := m.(type) {
		case types.SystemMessage:
			hashSystemContent(h, v.Content)
		case types.UserMessage:
			hashUserContent(h, v.Content)
		case types.AssistantMessage:
			hashAssistantContent(h, v.Content)
		}
	}
	hashTools(h, tools)
	if schema != nil {
		writeField(h, "schema", nil)
		hashParameterSchema(h, *schema)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeField writes a length-prefixed, tagged field so inputs are unambiguous.
func writeField(h io.Writer, tag string, data []byte) {
	var n [8]byte
	_, _ = io.WriteString(h, tag)
	binary.BigEndian.PutUint64(n[:], uint64(len(data)))
	_, _ = h.Write(n[:])
	_, _ = h.Write(data)
}

func hashSystemContent(h hash.Hash, content []types.SystemContent) {
	for _, c := range content {
		switch v := c.(type) {
		case types.TextContent:
			writeField(h, "text", []byte(v.Text))
		case types.ToolResultContent:
			hashToolResult(h, v)
			// ConfigContent / HandoffContent excluded: stripped before the provider.
		}
	}
}

func hashUserContent(h hash.Hash, content []types.UserContent) {
	for _, c := range content {
		switch v := c.(type) {
		case types.TextContent:
			writeField(h, "text", []byte(v.Text))
		case types.ToolResultContent:
			hashToolResult(h, v)
		case types.FileContent:
			writeField(h, "file", []byte(v.MediaType))
			writeField(h, "filename", []byte(v.Filename))
			writeField(h, "uri", []byte(v.URI))
			// Hash the raw bytes (json:"-" would silently drop them).
			sum := sha256.Sum256(v.Data)
			writeField(h, "data", sum[:])
			// ConfigContent / FeedbackContent / HandoffContent excluded.
		}
	}
}

func hashAssistantContent(h hash.Hash, content []types.AssistantContent) {
	for _, c := range content {
		switch v := c.(type) {
		case types.TextContent:
			writeField(h, "text", []byte(v.Text))
		case types.ThinkingContent:
			writeField(h, "thinking", []byte(v.Thinking))
			writeField(h, "signature", []byte(v.Signature))
		case types.ToolUseContent:
			writeField(h, "tooluse", []byte(v.ID))
			writeField(h, "toolname", []byte(v.Name))
			hashArgs(h, v.Arguments)
		}
	}
}

func hashToolResult(h hash.Hash, v types.ToolResultContent) {
	writeField(h, "toolresult", []byte(v.ToolCallID))
	if v.IsError {
		writeField(h, "iserror", []byte{1})
	} else {
		writeField(h, "iserror", []byte{0})
	}
	writeField(h, "text", []byte(v.Text))
	for _, b := range v.Blocks {
		writeField(h, "block", []byte(b.Kind))
		writeField(h, "blocktext", []byte(b.Text))
		writeField(h, "blockmedia", []byte(b.MediaType))
		writeField(h, "blockuri", []byte(b.URI))
		if len(b.Data) > 0 {
			sum := sha256.Sum256(b.Data)
			writeField(h, "blockdata", sum[:])
		}
		writeField(h, "blockjson", b.JSON)
	}
}

// hashArgs hashes a map[string]any with deterministically sorted keys. Each
// value is canonicalized via json.Marshal; nested maps inside the value are
// likewise key-sorted by re-marshaling through a sorted intermediate.
func hashArgs(h hash.Hash, args map[string]any) {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		writeField(h, "argkey", []byte(k))
		writeField(h, "argval", canonicalJSON(args[k]))
	}
}

// canonicalJSON marshals v with map keys sorted recursively. Go's encoding/json
// already sorts map[string]T keys, but interface values (map[string]any) are
// normalized here to be explicit and stable.
func canonicalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func hashTools(h hash.Hash, tools []types.ToolDef) {
	// Sort tools by name so registry ordering (map iteration) doesn't perturb the key.
	sorted := make([]types.ToolDef, len(tools))
	copy(sorted, tools)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, t := range sorted {
		writeField(h, "tool", []byte(t.Name))
		writeField(h, "tooldesc", []byte(t.Description))
		hashParameterSchema(h, t.Parameters)
	}
}

func hashParameterSchema(h hash.Hash, s types.ParameterSchema) {
	writeField(h, "schematype", []byte(s.Type))
	req := make([]string, len(s.Required))
	copy(req, s.Required)
	sort.Strings(req)
	for _, r := range req {
		writeField(h, "required", []byte(r))
	}
	keys := make([]string, 0, len(s.Properties))
	for k := range s.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		writeField(h, "prop", []byte(k))
		hashProperty(h, s.Properties[k])
	}
}

func hashProperty(h hash.Hash, p types.PropertyDef) {
	writeField(h, "ptype", []byte(p.Type))
	writeField(h, "pdesc", []byte(p.Description))
	for _, e := range p.Enum {
		writeField(h, "penum", []byte(e))
	}
	if p.Default != nil {
		writeField(h, "pdefault", canonicalJSON(p.Default))
	}
	if p.Items != nil {
		writeField(h, "pitems", nil)
		hashProperty(h, *p.Items)
	}
	keys := make([]string, 0, len(p.Properties))
	for k := range p.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		writeField(h, "pprop", []byte(k))
		hashProperty(h, p.Properties[k])
	}
	req := make([]string, len(p.Required))
	copy(req, p.Required)
	sort.Strings(req)
	for _, r := range req {
		writeField(h, "preq", []byte(r))
	}
}
