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
// concatenation. ConfigPart and FeedbackPart are excluded because the
// agent loop strips them before they reach the provider.
func Key(model string, msgs []types.Message, tools []types.ToolDef, schema *types.ParameterSchema) string {
	h := sha256.New()
	writeField(h, "model", []byte(model))

	for _, m := range msgs {
		writeField(h, "role", []byte(m.Role()))
		switch v := m.(type) {
		case types.SystemMessage:
			hashSystemContent(h, v.Parts)
		case types.UserMessage:
			hashUserContent(h, v.Parts)
		case types.AssistantMessage:
			hashAssistantContent(h, v.Parts)
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

func hashSystemContent(h hash.Hash, content []types.SystemPart) {
	for _, c := range content {
		switch v := c.(type) {
		case types.TextPart:
			writeField(h, "text", []byte(v.Text))
		case types.ToolResultPart:
			hashToolResult(h, v)
			// ConfigPart / HandoffPart excluded: stripped before the provider.
		}
	}
}

func hashUserContent(h hash.Hash, content []types.UserPart) {
	for _, c := range content {
		switch v := c.(type) {
		case types.TextPart:
			writeField(h, "text", []byte(v.Text))
		case types.ToolResultPart:
			hashToolResult(h, v)
		case types.ImagePart, types.AudioPart, types.VideoPart, types.DocumentPart, types.FilePart:
			src, _ := types.SourceOf(v)
			writeField(h, "file", []byte(src.MediaType))
			writeField(h, "filename", []byte(src.Filename))
			writeField(h, "uri", []byte(src.URI))
			// Hash the raw bytes (json:"-" would silently drop them).
			sum := sha256.Sum256(src.Inline)
			writeField(h, "data", sum[:])
			// ConfigPart / FeedbackPart / HandoffPart excluded.
		}
	}
}

func hashAssistantContent(h hash.Hash, content []types.AssistantPart) {
	for _, c := range content {
		switch v := c.(type) {
		case types.TextPart:
			writeField(h, "text", []byte(v.Text))
		case types.ThinkingPart:
			writeField(h, "thinking", []byte(v.Text))
			writeField(h, "signature", []byte(v.Signature))
		case types.ToolCallPart:
			writeField(h, "tooluse", []byte(v.ID))
			writeField(h, "toolname", []byte(v.Name))
			hashArgs(h, v.Arguments)
		}
	}
}

func hashToolResult(h hash.Hash, v types.ToolResultPart) {
	writeField(h, "toolresult", []byte(v.CallID))
	if v.IsError {
		writeField(h, "iserror", []byte{1})
	} else {
		writeField(h, "iserror", []byte{0})
	}
	writeField(h, "text", []byte(v.Text()))
	if len(v.Parts) == 1 {
		if _, plain := v.Parts[0].(types.TextPart); plain {
			return // a plain text result is its text
		}
	}
	for _, p := range v.Parts {
		var kind, text, uri string
		var media types.MediaType
		var data []byte
		var raw []byte
		switch x := p.(type) {
		case types.TextPart:
			kind, text = "text", x.Text
		case types.JSONPart:
			kind, raw = "json", x.JSON
		default:
			src, _ := types.SourceOf(p)
			kind, media, uri, data = "file", src.MediaType, src.URI, src.Inline
			if _, img := p.(types.ImagePart); img {
				kind = "image"
			}
		}
		writeField(h, "block", []byte(kind))
		writeField(h, "blocktext", []byte(text))
		writeField(h, "blockmedia", []byte(media))
		writeField(h, "blockuri", []byte(uri))
		if len(data) > 0 {
			sum := sha256.Sum256(data)
			writeField(h, "blockdata", sum[:])
		}
		writeField(h, "blockjson", raw)
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
	// Tool order is part of the actual provider prompt. Registries already
	// expose a stable order; direct callers can deliberately choose another one.
	for _, t := range tools {
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
	if p.Nullable {
		writeField(h, "pnullable", []byte("true"))
	}
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
