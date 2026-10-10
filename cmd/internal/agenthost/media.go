package agenthost

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/urmzd/saige/agent/types"
)

// Errors the media boundary returns. Each matches one client mistake, so a
// host can map it to a status code.
var (
	// ErrVendorFile is returned for a client part that names a vendor file
	// ID. File IDs belong to the host's provider accounts; a client that
	// could name one could read another tenant's upload.
	ErrVendorFile = errors.New("vendor file IDs are not accepted from clients; upload the bytes or use an https URI")
	// ErrPartKind is returned for a client part of a kind clients may not
	// send, such as a tool result or run metadata.
	ErrPartKind = errors.New("part kind not accepted from clients")
	// ErrURIScheme is returned for a client media URI with a scheme other
	// than https, or on a vendor's file store.
	ErrURIScheme = errors.New("media URIs from clients must be https")
	// ErrInlineTooLarge is returned for client inline bytes over the inline
	// limit; the client uploads them as an artifact instead.
	ErrInlineTooLarge = errors.New("inline media too large; upload it as an artifact and send its ref")
	// ErrArtifactNotFound is returned for a ref the session does not hold.
	ErrArtifactNotFound = errors.New("artifact not found in this session")
	// ErrArtifactsFull is returned when an upload would pass the session's
	// artifact budget.
	ErrArtifactsFull = errors.New("session artifact storage is full")
	// ErrEmptyMessage is returned for a message with no content.
	ErrEmptyMessage = errors.New("the message has no content")
)

// Artifact is one stored piece of media.
type Artifact struct {
	Digest    string
	MediaType types.MediaType
	Filename  string
	Data      []byte
}

// Ref is the artifact's saige-artifact:// reference.
func (a Artifact) Ref() string { return types.ArtifactScheme + a.Digest }

// Source returns a source for the artifact: its reference, digest and size,
// without the bytes.
func (a Artifact) Source() types.Source {
	src := types.Artifact(a.Ref(), a.MediaType)
	src.Filename = a.Filename
	src.Size = int64(len(a.Data))
	return src
}

// Artifacts is a session's content-addressed media store: what clients
// upload and what the host externalizes from a run's output. It is safe
// for concurrent use.
type Artifacts struct {
	mu    sync.Mutex
	limit int64
	used  int64
	byID  map[string]Artifact
}

// DefaultArtifactBudget is the bytes a session's artifacts may hold.
const DefaultArtifactBudget = 256 << 20

// NewArtifacts returns an empty store holding at most limit bytes; zero
// means DefaultArtifactBudget and a negative limit means no limit.
func NewArtifacts(limit int64) *Artifacts {
	if limit == 0 {
		limit = DefaultArtifactBudget
	}
	return &Artifacts{limit: limit, byID: map[string]Artifact{}}
}

// Put stores data and returns its artifact. Storing the same bytes again
// returns the stored artifact and uses no more of the budget.
func (s *Artifacts) Put(mt types.MediaType, filename string, data []byte) (Artifact, error) {
	sum := sha256.Sum256(data)
	id := hex.EncodeToString(sum[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.byID[id]; ok {
		return a, nil
	}
	if s.limit >= 0 && s.used+int64(len(data)) > s.limit {
		return Artifact{}, fmt.Errorf("%w: %d of %d bytes used", ErrArtifactsFull, s.used, s.limit)
	}
	if mt == "" {
		mt = "application/octet-stream"
	}
	a := Artifact{Digest: id, MediaType: mt, Filename: filename, Data: append([]byte(nil), data...)}
	s.byID[id] = a
	s.used += int64(len(data))
	return a, nil
}

// Get returns the artifact with digest id.
func (s *Artifacts) Get(id string) (Artifact, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byID[id]
	return a, ok
}

// Lookup returns the artifact a saige-artifact:// reference names.
func (s *Artifacts) Lookup(ref string) (Artifact, bool) {
	id, ok := strings.CutPrefix(ref, types.ArtifactScheme)
	if !ok {
		return Artifact{}, false
	}
	return s.Get(id)
}

// ClientParts are the limits a host applies to the parts an untrusted
// client sends.
type ClientParts struct {
	// MaxInline caps the inline bytes of one part. Zero means
	// types.DefaultMaxInlineBytes; a negative value means no limit.
	MaxInline int
	// Artifacts resolves the client's saige-artifact:// refs and stores
	// its inline bytes, so the run's record keeps a ref. Nil refuses refs.
	Artifacts *Artifacts
}

// clientKinds are the part kinds a client may send in a user message.
var clientKinds = []types.PartKind{
	types.KindText, types.KindImage, types.KindAudio, types.KindVideo, types.KindDocument, types.KindFile,
}

// UserMessage checks the parts an untrusted client sent and returns the
// user message to run. Only text and media parts are accepted. A media part
// may carry inline bytes (up to MaxInline), an https URI, or a ref to an
// artifact the session holds, whose bytes are attached. A vendor file ID is
// always refused (ErrVendorFile): the client cannot know which provider
// account serves the run. types.CheckClientParts runs first, so a URI on a
// vendor file store is refused too (ErrURIScheme, matching
// types.ErrUntrustedLocator). A client-set unresolved reason is dropped.
func (c ClientParts) UserMessage(parts []types.UserPart) (types.UserMessage, error) {
	limit := c.MaxInline
	if limit == 0 {
		limit = types.DefaultMaxInlineBytes
	}
	// The shared untrusted-locator rules first (vendor files, foreign
	// schemes, vendor file-store URLs); a vendor file keeps its own error,
	// so a host can name the case.
	if err := types.CheckClientParts(parts); err != nil {
		for _, p := range parts {
			if src, ok := types.SourceOf(p); ok && len(src.Files) > 0 {
				return types.UserMessage{}, fmt.Errorf("%w: %w", ErrVendorFile, err)
			}
		}
		return types.UserMessage{}, fmt.Errorf("%w: %w", ErrURIScheme, err)
	}
	out := make([]types.UserPart, 0, len(parts))
	var content bool
	for i, p := range parts {
		if p == nil || !slices.Contains(clientKinds, p.Kind()) {
			kind := "nil"
			if p != nil {
				kind = string(p.Kind())
			}
			return types.UserMessage{}, fmt.Errorf("part %d: %w: %s", i, ErrPartKind, kind)
		}
		if t, ok := p.(types.TextPart); ok {
			if strings.TrimSpace(t.Text) != "" {
				content = true
			}
			out = append(out, t)
			continue
		}
		src, _ := types.SourceOf(p)
		src, err := c.source(src, limit)
		if err != nil {
			return types.UserMessage{}, fmt.Errorf("part %d (%s): %w", i, p.Kind(), err)
		}
		content = true
		out = append(out, WithSource(p, src).(types.UserPart))
	}
	if !content {
		return types.UserMessage{}, ErrEmptyMessage
	}
	return types.UserMsg(out...), nil
}

func (c ClientParts) source(src types.Source, limit int) (types.Source, error) {
	src.Unresolved = ""
	if len(src.Files) > 0 {
		return types.Source{}, ErrVendorFile
	}
	if src.URI != "" && !strings.HasPrefix(strings.ToLower(src.URI), "https://") {
		return types.Source{}, fmt.Errorf("%w: %s", ErrURIScheme, schemeOf(src.URI))
	}
	if src.Ref != "" {
		if c.Artifacts == nil {
			return types.Source{}, ErrArtifactNotFound
		}
		a, ok := c.Artifacts.Lookup(src.Ref)
		if !ok {
			return types.Source{}, fmt.Errorf("%w: %s", ErrArtifactNotFound, src.Ref)
		}
		if src.Digest != "" && src.Digest != a.Digest {
			return types.Source{}, fmt.Errorf("sha256 %s does not match %s", src.Digest, src.Ref)
		}
		if src.MediaType == "" {
			src.MediaType = a.MediaType
		}
		if src.Filename == "" {
			src.Filename = a.Filename
		}
		src.Digest, src.Size, src.Inline = a.Digest, int64(len(a.Data)), a.Data
		return src, nil
	}
	if len(src.Inline) == 0 {
		if src.URI == "" {
			return types.Source{}, errors.New("media needs data, an https uri, or an artifact ref")
		}
		return src, nil
	}
	if limit >= 0 && len(src.Inline) > limit {
		return types.Source{}, fmt.Errorf("%w (%d bytes, limit %d)", ErrInlineTooLarge, len(src.Inline), limit)
	}
	sum := sha256.Sum256(src.Inline)
	digest := hex.EncodeToString(sum[:])
	if src.Digest != "" && src.Digest != digest {
		return types.Source{}, errors.New("sha256 does not match the data")
	}
	src.Digest, src.Size = digest, int64(len(src.Inline))
	if c.Artifacts != nil {
		a, err := c.Artifacts.Put(src.MediaType, src.Filename, src.Inline)
		if err != nil {
			return types.Source{}, err
		}
		src.Ref = a.Ref()
	}
	return src, nil
}

func schemeOf(uri string) string {
	if i := strings.Index(uri, ":"); i > 0 {
		return uri[:i]
	}
	return "no scheme"
}

// WithSource returns the media part p with its source replaced. Any other
// part is returned unchanged.
func WithSource(p types.Part, src types.Source) types.Part {
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

// MapSources returns d with every media source in it passed through f: the
// part a PartEnd closes (and a server tool result's outputs), a tool
// result's parts, and the same inside a sub-agent's deltas. Other deltas
// are returned unchanged.
func MapSources(d types.Delta, f func(types.Source) (types.Source, error)) (types.Delta, error) {
	switch v := d.(type) {
	case types.PartEnd:
		if v.Part == nil {
			return v, nil
		}
		p, err := mapPart(v.Part, f)
		if err != nil {
			return nil, err
		}
		v.Part = p.(types.AssistantPart)
		return v, nil
	case types.ToolExecEndDelta:
		if len(v.Parts) == 0 {
			return v, nil
		}
		parts := make([]types.ToolOutputPart, len(v.Parts))
		for i, p := range v.Parts {
			q, err := mapPart(p, f)
			if err != nil {
				return nil, err
			}
			parts[i] = q.(types.ToolOutputPart)
		}
		v.Parts = parts
		return v, nil
	case types.ToolExecDelta:
		if v.Inner == nil {
			return v, nil
		}
		inner, err := MapSources(v.Inner, f)
		if err != nil {
			return nil, err
		}
		v.Inner = inner
		return v, nil
	}
	return d, nil
}

func mapPart(p types.Part, f func(types.Source) (types.Source, error)) (types.Part, error) {
	if sr, ok := p.(types.ServerToolResultPart); ok && len(sr.Outputs) > 0 {
		outs := make([]types.Part, len(sr.Outputs))
		for i, o := range sr.Outputs {
			q, err := mapPart(o, f)
			if err != nil {
				return nil, err
			}
			outs[i] = q
		}
		sr.Outputs = outs
		return sr, nil
	}
	src, ok := types.SourceOf(p)
	if !ok {
		return p, nil
	}
	src, err := f(src)
	if err != nil {
		return nil, err
	}
	return WithSource(p, src), nil
}

// Externalize prepares d for a wire whose inline fields hold at most limit
// bytes: a media part whose bytes are over the limit is stored in store and
// sent as its ref, and a streamed data chunk over the limit is split into
// chunks that fit. Bytes at or under the limit stay inline. Media it cannot
// store keeps its metadata and is marked unresolved with the reason, so a
// client sees that the bytes were not kept instead of losing them silently.
func Externalize(d types.Delta, store *Artifacts, limit int) []types.Delta {
	if limit <= 0 {
		limit = types.DefaultMaxInlineBytes
	}
	if pd, ok := d.(types.PartDelta); ok && len(pd.Data) > limit {
		var out []types.Delta
		for data := pd.Data; len(data) > 0; {
			n := min(limit, len(data))
			chunk := types.PartDelta{Index: pd.Index, Data: data[:n]}
			if len(out) == 0 {
				chunk = pd
				chunk.Data = data[:n]
			}
			out = append(out, chunk)
			data = data[n:]
		}
		return out
	}
	if ex, ok := d.(types.ToolExecDelta); ok && ex.Inner != nil {
		inner := Externalize(ex.Inner, store, limit)
		out := make([]types.Delta, len(inner))
		for i, in := range inner {
			out[i] = types.ToolExecDelta{ToolCallID: ex.ToolCallID, Inner: in}
		}
		return out
	}
	x, _ := MapSources(d, func(src types.Source) (types.Source, error) {
		if len(src.Inline) <= limit {
			return src, nil
		}
		if src.Digest == "" {
			sum := sha256.Sum256(src.Inline)
			src.Digest = hex.EncodeToString(sum[:])
		}
		size := int64(len(src.Inline))
		if store == nil {
			src.Size, src.Inline = size, nil
			return src.Unavailable(fmt.Sprintf("%d bytes over the %d byte inline limit and no artifact store", size, limit)), nil
		}
		a, err := store.Put(src.MediaType, src.Filename, src.Inline)
		if err != nil {
			src.Size, src.Inline = size, nil
			return src.Unavailable("not kept: " + err.Error()), nil
		}
		src.Ref, src.Digest, src.Size, src.Inline = a.Ref(), a.Digest, int64(len(a.Data)), nil
		return src, nil
	})
	return []types.Delta{x}
}
