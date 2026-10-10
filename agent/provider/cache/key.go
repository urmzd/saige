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

// KeyVersion is the version of the key derivation. It is hashed into every
// key, so a change to the derivation misses old entries instead of
// matching them by accident. Version 2 hashes parts: media by its digest
// rather than by the locators that reach it, and the conversion report.
// Entries written under version 1 miss once and are written again.
const KeyVersion = 2

// Key returns a deterministic sha256 hex digest of the request inputs
// (model, messages, tools, schema). It is KeyWithConversions with no
// conversion report.
func Key(model string, msgs []types.Message, tools []types.ToolDef, schema *types.ParameterSchema) string {
	return KeyWithConversions(model, msgs, tools, schema, "")
}

// KeyWithConversions returns a deterministic sha256 hex digest of the
// request inputs and the hash of the conversion report planned for them
// (types.ConversionReport.Hash), so a converted view and the original
// never share an entry. It is stable across process runs: map keys are
// sorted, every field is length-prefixed and type-tagged so distinct inputs
// cannot collide via concatenation, and media is hashed by what it is, not
// by how it is reached:
//
//   - its digest, else the digest of its inline bytes, else its URI, else
//     its workspace reference, else its first vendor upload
//     (provider:endpoint:id), so a re-upload or a resolved URI does not miss;
//   - its kind, media type, file name and metadata (detail, pages, clip,
//     frame rate), which change what the provider receives.
//
// Metadata parts are excluded because the agent loop strips them before
// they reach the provider. Thinking is hashed with its signature.
func KeyWithConversions(model string, msgs []types.Message, tools []types.ToolDef, schema *types.ParameterSchema, conversions string) string {
	h := sha256.New()
	writeField(h, "key", []byte{KeyVersion})
	writeField(h, "model", []byte(model))
	for _, m := range msgs {
		writeField(h, "role", []byte(m.Role()))
		for _, p := range types.PartsOf(m) {
			hashPart(h, p)
		}
	}
	hashTools(h, tools)
	if schema != nil {
		writeField(h, "schema", nil)
		hashParameterSchema(h, *schema)
	}
	if conversions != "" {
		writeField(h, "conversions", []byte(conversions))
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

// hashPart hashes one part in the shared part codec, with media sources
// reduced to their identity and a tool result's parts hashed one by one.
func hashPart(h hash.Hash, p types.Part) {
	if types.IsMetadata(p) {
		return
	}
	writeField(h, "part", []byte(p.Kind()))
	if src, ok := types.SourceOf(p); ok {
		writeField(h, "media", []byte(mediaIdentity(src)))
		writeField(h, "body", marshalPart(withSource(p, types.Source{MediaType: src.MediaType, Filename: src.Filename})))
		return
	}
	if tr, ok := p.(types.ToolResultPart); ok {
		nested := tr.Parts
		tr.Parts = nil
		writeField(h, "body", marshalPart(tr))
		for _, n := range nested {
			hashPart(h, n)
		}
		writeField(h, "end", nil)
		return
	}
	writeField(h, "body", marshalPart(p))
}

func marshalPart(p types.Part) []byte {
	b, err := types.MarshalPart(p)
	if err != nil {
		return []byte("!" + err.Error())
	}
	return b
}

// mediaIdentity names the media a source reaches, independent of which
// locators it carries.
func mediaIdentity(src types.Source) string {
	switch {
	case src.Digest != "":
		return "sha256:" + src.Digest
	case len(src.Inline) > 0:
		sum := sha256.Sum256(src.Inline)
		return "sha256:" + hex.EncodeToString(sum[:])
	case src.URI != "":
		return "uri:" + src.URI
	case src.Ref != "":
		return "ref:" + src.Ref
	case len(src.Files) > 0:
		f := src.Files[0]
		return "file:" + f.Provider + ":" + f.Endpoint + ":" + f.ID
	}
	return "elided"
}

// withSource returns the media part p with its source replaced.
func withSource(p types.Part, src types.Source) types.Part {
	switch v := p.(type) {
	case types.ImagePart:
		v.Source = src
		return v
	case types.AudioPart:
		v.Source = src
		return v
	case types.VideoPart:
		v.Source = src
		return v
	case types.DocumentPart:
		v.Source = src
		return v
	case types.FilePart:
		v.Source = src
		return v
	case types.AudioOutPart:
		v.Source = src
		return v
	case types.ImageOutPart:
		v.Source = src
		return v
	case types.VideoOutPart:
		v.Source = src
		return v
	}
	return p
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
