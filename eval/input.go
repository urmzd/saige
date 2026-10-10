package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// Inputs made of parts.
//
// A dataset case whose input carries media stores it in the parts form:
//
//	{"parts":[{"type":"text","text":"What is in this picture?"},
//	          {"type":"image","source":{"media_type":"image/png","uri":"file:images/cat.png"}}]}
//
// Each part uses the shared part codec. Media is referenced by a locator
// (a uri such as file:, https: or gs:, or a saige-artifact:// ref), never by
// bytes, so a dataset stays small, diffable and safe to review. Inline bytes
// are accepted only when the row says so with "inline": true, and only up to
// [MaxInlineInputBytes] per part:
//
//	{"inline":true,"parts":[{"type":"image","source":{"media_type":"image/png","data":"iVBORw0..."}}]}
//
// An input that is a JSON string is the text form and reads as one text
// part, so datasets written before parts existed keep working. Any other
// JSON value is a subject-defined input: it has no parts, and judges see it
// as JSON text, as before.

// MaxInlineInputBytes is the most bytes a dataset input may inline in one
// part, when the row allows inline bytes at all.
const MaxInlineInputBytes = 64 << 10

// Errors of the parts input form.
var (
	// ErrInlineInput reports media bytes in a dataset input that did not
	// allow them, or more of them than [MaxInlineInputBytes].
	ErrInlineInput = errors.New("dataset input inlines media bytes")
	// ErrUnlocatedMedia reports a media part whose only locator is inline
	// bytes beyond what the encoding allows.
	ErrUnlocatedMedia = errors.New("dataset input media has no locator")
)

// InputKind says which form an observation's input takes.
type InputKind string

const (
	// InputText is a JSON string.
	InputText InputKind = "text"
	// InputParts is the {"parts": [...]} form.
	InputParts InputKind = "parts"
	// InputOther is any other JSON value, read by its subject.
	InputOther InputKind = "other"
)

// Input is a decoded observation input.
type Input struct {
	Kind InputKind
	// Parts are the input parts: one text part for the text form, none for
	// a subject-defined input.
	Parts []types.UserPart
	// Raw is the input as stored.
	Raw json.RawMessage
}

// Text is the input's text: the text form's string, the parts form's text
// parts joined by newlines, or the raw JSON of a subject-defined input.
func (in Input) Text() string {
	if in.Kind == InputOther {
		return string(in.Raw)
	}
	var texts []string
	for _, p := range in.Parts {
		if t, ok := p.(types.TextPart); ok {
			texts = append(texts, t.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// Media returns the input's media parts, in order.
func (in Input) Media() []types.UserPart {
	var out []types.UserPart
	for _, p := range in.Parts {
		if types.IsMedia(p) {
			out = append(out, p)
		}
	}
	return out
}

// HasMedia reports whether the input carries a media part.
func (in Input) HasMedia() bool { return len(in.Media()) > 0 }

// partsInput is the JSON form of [InputParts].
type partsInput struct {
	Inline bool              `json:"inline,omitempty"`
	Parts  []json.RawMessage `json:"parts"`
}

// DecodeInput reads an observation input in any of its forms. A parts input
// whose parts do not decode as user parts, or that inlines bytes it did not
// allow (see [MaxInlineInputBytes]), is an error.
func DecodeInput(raw json.RawMessage) (Input, error) {
	in := Input{Kind: InputOther, Raw: raw}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return in, nil
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return in, fmt.Errorf("input: %w", err)
		}
		in.Kind, in.Parts = InputText, []types.UserPart{types.Text(s)}
		return in, nil
	case '{':
	default:
		return in, nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return in, fmt.Errorf("input: %w", err)
	}
	if _, ok := probe["parts"]; !ok {
		return in, nil
	}
	var pi partsInput
	if err := json.Unmarshal(trimmed, &pi); err != nil {
		return in, fmt.Errorf("input parts: %w", err)
	}
	in.Kind = InputParts
	in.Parts = make([]types.UserPart, 0, len(pi.Parts))
	for i, rp := range pi.Parts {
		p, err := types.UnmarshalRolePart[types.UserPart](rp)
		if err != nil {
			return in, fmt.Errorf("input part %d: %w", i, err)
		}
		if err := checkInline(i, p, pi.Inline, MaxInlineInputBytes); err != nil {
			return in, err
		}
		in.Parts = append(in.Parts, p)
	}
	return in, nil
}

// checkInline enforces the inline rule on one decoded or encoded part.
func checkInline(i int, p types.Part, allowed bool, limit int) error {
	src, ok := types.SourceOf(p)
	if !ok || len(src.Inline) == 0 {
		return nil
	}
	if !allowed {
		return fmt.Errorf("%w: part %d (%s) holds %d bytes; reference it by uri or ref, or set \"inline\": true",
			ErrInlineInput, i, p.Kind(), len(src.Inline))
	}
	if len(src.Inline) > limit {
		return fmt.Errorf("%w: part %d (%s) holds %d bytes, limit %d", ErrInlineInput, i, p.Kind(), len(src.Inline), limit)
	}
	return nil
}

// InputOptions configures [EncodeInput].
type InputOptions struct {
	// InlineMax allows a media part with no other locator to keep up to
	// this many bytes inline, at most [MaxInlineInputBytes]. Zero never
	// inlines.
	InlineMax int
}

// EncodeInput writes parts in the dataset form. A single text part is
// written as the text form, a JSON string, so text datasets read as they
// always have. Media bytes are never written when the part has another
// locator (a uri, a ref or a vendor file); a part with bytes and no other
// locator is written inline only within opts.InlineMax, and is otherwise an
// error wrapping [ErrUnlocatedMedia].
func EncodeInput(parts []types.UserPart, opts InputOptions) (json.RawMessage, error) {
	if len(parts) == 1 {
		if t, ok := parts[0].(types.TextPart); ok {
			return json.Marshal(t.Text)
		}
	}
	limit := min(max(opts.InlineMax, 0), MaxInlineInputBytes)
	pi := partsInput{Parts: make([]json.RawMessage, 0, len(parts))}
	for i, p := range parts {
		src, media := types.SourceOf(p)
		inline := media && len(src.Inline) > 0 && src.URI == "" && src.Ref == "" && len(src.Files) == 0
		var (
			raw []byte
			err error
		)
		switch {
		case !inline:
			// A part with another locator drops its bytes; one already
			// elided keeps its digest and media type as a record.
			raw, err = types.MarshalPart(p)
		case len(src.Inline) <= limit:
			pi.Inline = true
			raw, err = types.MarshalPartInline(p)
		default:
			return nil, fmt.Errorf("%w: part %d (%s) has only %d inline bytes; store it and reference it by uri or ref",
				ErrUnlocatedMedia, i, p.Kind(), len(src.Inline))
		}
		if err != nil {
			return nil, fmt.Errorf("input part %d: %w", i, err)
		}
		pi.Parts = append(pi.Parts, raw)
	}
	return json.Marshal(pi)
}

// ResolveInput fills the inline bytes of each media part whose source has a
// uri, or else a workspace ref, with a scheme in resolvers, keyed like
// agent.WithResolvers ("file", "https", "saige-artifact"). Parts that
// already hold bytes, or whose locators have no resolver, are returned as
// they are. Resolved bytes must match a digest the source records. The
// input parts are not modified.
func ResolveInput(ctx context.Context, parts []types.UserPart, resolvers map[string]types.Resolver) ([]types.UserPart, error) {
	out := make([]types.UserPart, len(parts))
	for i, p := range parts {
		out[i] = p
		src, ok := types.SourceOf(p)
		if !ok || len(src.Inline) > 0 {
			continue
		}
		loc, r := resolverFor(src, resolvers)
		if r == nil {
			continue
		}
		f, err := r.Resolve(ctx, loc)
		if err != nil {
			return nil, fmt.Errorf("resolve input part %d (%s): %w", i, loc, err)
		}
		got := types.Bytes(src.MediaType, f.Data)
		if got.MediaType == "" {
			got.MediaType = f.MediaType
		}
		if src.Digest != "" && src.Digest != got.Digest {
			return nil, fmt.Errorf("resolve input part %d (%s): sha256 %s does not match the dataset's %s",
				i, loc, got.Digest, src.Digest)
		}
		up, ok := withSource(p, got.With(src)).(types.UserPart)
		if !ok {
			continue
		}
		out[i] = up
	}
	return out, nil
}

// resolverFor picks the locator of src a resolver handles: the uri first,
// then the ref.
func resolverFor(src types.Source, resolvers map[string]types.Resolver) (string, types.Resolver) {
	for _, loc := range []string{src.URI, src.Ref} {
		scheme, _, found := strings.Cut(loc, ":")
		if !found {
			continue
		}
		if r, ok := resolvers[strings.ToLower(scheme)]; ok {
			return loc, r
		}
	}
	return "", nil
}

// withSource returns media part p with src as its source.
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
	}
	return p
}

// DirResolver returns a resolver for file: URIs relative to dir, the
// directory of a dataset, such as "file:images/cat.png". A path that leaves
// dir, through ".." or a symbolic link, is refused. The media type comes
// from the file extension when the dataset does not name one.
func DirResolver(dir string) types.Resolver {
	return types.ResolverFunc(func(_ context.Context, uri string) (types.ResolvedFile, error) {
		rel, ok := strings.CutPrefix(uri, "file:")
		if !ok {
			return types.ResolvedFile{}, fmt.Errorf("dataset resolver: %q is not a file: uri", uri)
		}
		rel = strings.TrimPrefix(rel, "//")
		root, err := os.OpenRoot(dir)
		if err != nil {
			return types.ResolvedFile{}, fmt.Errorf("dataset resolver: %w", err)
		}
		defer func() { _ = root.Close() }()
		f, err := root.Open(filepath.FromSlash(rel))
		if err != nil {
			return types.ResolvedFile{}, fmt.Errorf("dataset resolver: %w", err)
		}
		defer func() { _ = f.Close() }()
		data, err := io.ReadAll(f)
		if err != nil {
			return types.ResolvedFile{}, fmt.Errorf("dataset resolver: %w", err)
		}
		mt := types.MediaType(mime.TypeByExtension(filepath.Ext(rel)))
		return types.ResolvedFile{Data: data, MediaType: mt}, nil
	})
}
