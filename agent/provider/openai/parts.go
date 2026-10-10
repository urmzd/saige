package openai

import (
	"encoding/base64"
	"fmt"
	"mime"
	"strings"

	"github.com/urmzd/saige/agent/types"
)

// Surface names used in rejection errors.
const (
	surfaceChat      = "chat completions"
	surfaceResponses = "responses"
)

// rejectPart builds the error for a part a surface cannot send natively.
// It names the part's path, kind and media type and matches
// types.ErrModalityUnsupported (and so types.ErrInvalidModelConfig).
func rejectPart(surface string, path types.PartPath, p types.Part, reason string) error {
	return partError(types.ErrModalityUnsupported, surface, path, p, reason)
}

// unavailablePart builds the error for a media part with no locator the
// adapter can use. It matches types.ErrMediaUnavailable.
func unavailablePart(surface string, path types.PartPath, p types.Part, reason string) error {
	return partError(types.ErrMediaUnavailable, surface, path, p, reason)
}

func partError(sentinel error, surface string, path types.PartPath, p types.Part, reason string) error {
	desc := string(p.Kind())
	if src, ok := types.SourceOf(p); ok && src.MediaType != "" {
		desc += " " + string(src.MediaType)
	}
	return fmt.Errorf("%w: openai %s: message %d part %s (%s): %s", sentinel, surface, path.Message, path, desc, reason)
}

// wrapPartError turns a mapping error into a permanent provider error, the
// form every other request validation failure takes.
func wrapPartError(caps types.ModelCapabilities, err error) error {
	if err == nil {
		return nil
	}
	return &types.ProviderError{Provider: caps.Provider, Model: caps.Model, Kind: types.ErrorKindPermanent, Err: err}
}

// topLevel is the path of a part directly in a message.
func topLevel(msg, part int) types.PartPath {
	return types.PartPath{Message: msg, Part: part, Nested: -1}
}

// baseType is mt without parameters, lower-cased.
func baseType(mt types.MediaType) types.MediaType {
	s := strings.ToLower(strings.TrimSpace(string(mt)))
	if t, _, err := mime.ParseMediaType(s); err == nil {
		return types.MediaType(t)
	}
	if i := strings.IndexByte(s, ';'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return types.MediaType(s)
}

// imageTypes are the image formats both surfaces accept.
var imageTypes = map[types.MediaType]bool{
	types.MediaJPEG: true, types.MediaPNG: true, types.MediaGIF: true, types.MediaWebP: true,
}

func isImageType(mt types.MediaType) bool { return imageTypes[baseType(mt)] }

// audioInputFormat returns the input_audio format for a media type. Chat
// Completions takes wav and mp3 only.
func audioInputFormat(mt types.MediaType, declared string) (string, bool) {
	switch strings.ToLower(declared) {
	case "wav", "mp3":
		return strings.ToLower(declared), true
	}
	switch baseType(mt) {
	case types.MediaWAV, "audio/x-wav", "audio/wave", "audio/vnd.wave":
		return "wav", true
	case types.MediaMP3, "audio/mp3":
		return "mp3", true
	}
	return "", false
}

// responsesFileTypes are the input_file types the Responses API accepts,
// from its file input guide: PDF, rich documents, presentations,
// spreadsheets, and text and code. text/* is accepted as a family.
var responsesFileTypes = map[types.MediaType]bool{
	types.MediaPDF:                            true,
	types.MediaDOCX:                           true,
	types.MediaXLSX:                           true,
	types.MediaPPTX:                           true,
	types.MediaJSON:                           true,
	"application/msword":                      true,
	"application/rtf":                         true,
	"application/vnd.ms-excel":                true,
	"application/vnd.ms-powerpoint":           true,
	"application/vnd.oasis.opendocument.text": true,
	"application/xml":                         true,
	"application/x-yaml":                      true,
	"application/yaml":                        true,
	"application/javascript":                  true,
	"application/x-sh":                        true,
	"application/sql":                         true,
}

func isResponsesFileType(mt types.MediaType) bool {
	b := baseType(mt)
	return responsesFileTypes[b] || strings.HasPrefix(string(b), "text/")
}

// locator is how one media part is reached on the wire.
type locator struct {
	kind   types.SourceKind
	fileID string // kind file
	uri    string // kind uri: an https URL or a data: URI
	data   []byte // kind inline
}

// pickSource chooses how to send src, in the adapter's preference order:
// an OpenAI file ID, then a URL, then the inline bytes. Only the kinds in
// allowed are considered, and a URI is usable only when it is https, http
// or a data: URI. It returns ErrMediaUnavailable when src carries nothing
// a provider could read (an unresolved or elided source, or only a workspace
// reference), and ErrModalityUnsupported when its locators exist but none is
// one this surface accepts.
func pickSource(surface string, path types.PartPath, p types.Part, src types.Source, allowed ...types.SourceKind) (locator, error) {
	if src.Unresolved != "" {
		return locator{}, unavailablePart(surface, path, p, src.Unresolved)
	}
	ok := func(k types.SourceKind) bool {
		for _, a := range allowed {
			if a == k {
				return true
			}
		}
		return false
	}
	usable := false
	if f, has := src.VendorFile(providerName, ""); has {
		usable = true
		if ok(types.SourceFile) {
			return locator{kind: types.SourceFile, fileID: f.ID}, nil
		}
	}
	if src.URI != "" && fetchableURI(src.URI) {
		usable = true
		if ok(types.SourceURI) {
			return locator{kind: types.SourceURI, uri: src.URI}, nil
		}
	}
	if len(src.Inline) > 0 {
		usable = true
		if ok(types.SourceInline) {
			return locator{kind: types.SourceInline, data: src.Inline}, nil
		}
	}
	switch {
	case usable:
		return locator{}, rejectPart(surface, path, p, "source "+describeKinds(src)+" is not accepted here; accepted: "+joinKinds(allowed))
	case src.URI != "":
		return locator{}, rejectPart(surface, path, p, fmt.Sprintf("URI scheme of %q is not fetchable by OpenAI", redactURI(src.URI)))
	case src.Ref != "":
		return locator{}, unavailablePart(surface, path, p, "a workspace reference must be resolved to bytes or a URL first")
	case len(src.Files) > 0:
		return locator{}, rejectPart(surface, path, p, "its vendor files belong to another provider")
	default:
		return locator{}, unavailablePart(surface, path, p, "no locator: the bytes were not kept and nothing else names the media")
	}
}

func fetchableURI(u string) bool {
	l := strings.ToLower(u)
	return strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "data:")
}

// redactURI keeps a URI's scheme and host for an error message.
func redactURI(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			rest = rest[:j]
		}
		return u[:i+3] + rest
	}
	if i := strings.IndexByte(u, ':'); i >= 0 {
		return u[:i+1]
	}
	return u
}

func describeKinds(src types.Source) string {
	return joinKinds(src.Kinds())
}

func joinKinds(ks []types.SourceKind) string {
	s := make([]string, len(ks))
	for i, k := range ks {
		s[i] = string(k)
	}
	return strings.Join(s, ", ")
}

// dataURI encodes bytes as a data: URI.
func dataURI(mt types.MediaType, data []byte) string {
	return fmt.Sprintf("data:%s;base64,%s", baseType(mt), base64.StdEncoding.EncodeToString(data))
}

// filename returns the filename sent with inline file data, which OpenAI
// requires.
func filename(src types.Source, fallback string) string {
	if src.Filename != "" {
		return src.Filename
	}
	return fallback
}

// defaultFilename names inline file data that came without a name.
func defaultFilename(mt types.MediaType) string {
	if baseType(mt) == types.MediaPDF {
		return defaultPDFName
	}
	if exts, _ := mime.ExtensionsByType(string(baseType(mt))); len(exts) > 0 {
		return "document" + exts[0]
	}
	return "document"
}

// runeSpanToBytes converts a [start, end) span counted in characters of
// text to byte offsets, clamped to the text.
func runeSpanToBytes(text string, start, end int) (int, int) {
	return runeToByte(text, start), runeToByte(text, end)
}

func runeToByte(text string, n int) int {
	if n <= 0 {
		return 0
	}
	i := 0
	for pos := range text {
		if i == n {
			return pos
		}
		i++
	}
	return len(text)
}

// citationsAcross builds citation parts from annotations whose offsets
// count characters of the message text, the concatenation of spans. Each
// citation is anchored to the text part its span starts in, with byte
// offsets into that part; Citation.Start and End stay byte offsets into
// the whole text.
func citationsAcross(spans []textSpan, anns []annotation) []types.CitationPart {
	if len(anns) == 0 || len(spans) == 0 {
		return nil
	}
	var full strings.Builder
	for _, sp := range spans {
		full.WriteString(sp.text)
	}
	text := full.String()
	out := make([]types.CitationPart, 0, len(anns))
	for _, a := range anns {
		c, ok := a.citation(text)
		if !ok {
			continue
		}
		cp := types.CitationPart{Citation: c}
		if c.Start >= 0 {
			base := 0
			for i, sp := range spans {
				last := i == len(spans)-1
				if c.Start < base+len(sp.text) || last {
					cp.Anchor = &types.Anchor{PartIndex: sp.index,
						Start: min(c.Start-base, len(sp.text)), End: min(max(c.End-base, c.Start-base), len(sp.text))}
					break
				}
				base += len(sp.text)
			}
		}
		out = append(out, cp)
	}
	return out
}

// Annotation types, shared by both surfaces.
const (
	annURLCitation           = "url_citation"
	annFileCitation          = "file_citation"
	annContainerFileCitation = "container_file_citation"
	annFilePath              = "file_path"
)

// annotation is a text annotation on either surface. Chat Completions nests
// the url_citation fields under "url_citation"; Responses puts them at the
// top level.
type annotation struct {
	Type        string `json:"type"`
	URL         string `json:"url"`
	Title       string `json:"title"`
	StartIndex  *int   `json:"start_index"`
	EndIndex    *int   `json:"end_index"`
	FileID      string `json:"file_id"`
	Filename    string `json:"filename"`
	Index       *int   `json:"index"`
	ContainerID string `json:"container_id"`
	URLCitation *struct {
		URL        string `json:"url"`
		Title      string `json:"title"`
		StartIndex *int   `json:"start_index"`
		EndIndex   *int   `json:"end_index"`
	} `json:"url_citation"`
}

// citation maps an annotation to a citation with byte offsets into text.
// Annotations that are not citations (file_path) report false.
func (a annotation) citation(text string) (types.Citation, bool) {
	if u := a.URLCitation; u != nil {
		a.URL, a.Title, a.StartIndex, a.EndIndex = u.URL, u.Title, u.StartIndex, u.EndIndex
	}
	span := func(c types.Citation) types.Citation {
		if a.StartIndex != nil && a.EndIndex != nil {
			c.Start, c.End = runeSpanToBytes(text, *a.StartIndex, *a.EndIndex)
		}
		return c
	}
	switch a.Type {
	case annURLCitation:
		c := span(types.NewCitation(types.CitationWeb, a.URL, a.Title))
		c.Producer = providerName
		return c, true
	case annFileCitation, annContainerFileCitation:
		c := types.NewCitation(types.CitationDocument, "", a.Filename)
		c.Producer = providerName
		c.Meta = map[string]any{"file_id": a.FileID}
		if a.ContainerID != "" {
			c.Meta["container_id"] = a.ContainerID
		}
		if a.Index != nil {
			pos := runeToByte(text, *a.Index)
			c.Start, c.End = pos, pos
		}
		c = span(c)
		return c, true
	}
	return types.Citation{}, false
}
