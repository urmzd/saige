package google

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/urmzd/saige/agent/types"
	"google.golang.org/genai"
)

// maxInlinePart is the largest media part sent as inline bytes. Gemini
// limits a whole inline request to 20 MB; larger media must be uploaded and
// referenced by URI.
const maxInlinePart = 20 << 20

// filesAPIBase is the Gemini API location of uploaded files. A vendor file
// recorded by name ("files/abc") is sent as this base plus the name.
const filesAPIBase = "https://generativelanguage.googleapis.com/v1beta/"

// Media types Gemini reads natively, by modality. The lists follow the
// Gemini API and Vertex AI documentation; a part of another type is rejected
// before the request instead of failing with an opaque 400.
var (
	imageTypes = setOf("image/png", "image/jpeg", "image/webp", "image/gif", "image/heic", "image/heif")
	audioTypes = setOf("audio/wav", "audio/x-wav", "audio/wave", "audio/mp3", "audio/mpeg", "audio/mpga",
		"audio/aiff", "audio/x-aiff", "audio/aac", "audio/ogg", "audio/flac", "audio/m4a", "audio/mp4",
		"audio/opus", "audio/webm", "audio/pcm")
	videoTypes = setOf("video/mp4", "video/mpeg", "video/mpg", "video/mov", "video/quicktime", "video/avi",
		"video/x-msvideo", "video/x-flv", "video/webm", "video/wmv", "video/x-ms-wmv", "video/3gpp")
	// toolMediaTypes are the types a function response may carry as parts.
	toolMediaTypes = setOf("image/png", "image/jpeg", "image/webp", "application/pdf", "text/plain")
)

func setOf(v ...string) map[string]bool {
	m := make(map[string]bool, len(v))
	for _, s := range v {
		m[s] = true
	}
	return m
}

// baseType returns the lower-case media type without parameters.
func baseType(mt types.MediaType) string {
	base, _, _ := strings.Cut(string(mt), ";")
	return strings.ToLower(strings.TrimSpace(base))
}

// mediaToolResults reports whether model takes media inside a function
// response. Gemini 3 models do; earlier ones accept only a JSON response.
func mediaToolResults(model string) bool {
	rest, ok := strings.CutPrefix(model, "gemini-")
	if !ok {
		return false
	}
	end := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
	if end < 0 {
		end = len(rest)
	}
	major, err := strconv.Atoi(rest[:end])
	return err == nil && major >= 3
}

// partPath locates a part in the request for an error message.
type partPath struct {
	types.PartPath
	kind types.PartKind
	mt   types.MediaType
}

// String names the message, the part, the tool output inside it, the kind
// and the media type.
func (p partPath) String() string {
	s := fmt.Sprintf("message %d part %d", p.Message, p.Part)
	if p.Nested >= 0 {
		s += fmt.Sprintf(" output %d", p.Nested)
	}
	s += " (" + string(p.kind)
	if p.mt != "" {
		s += ", " + string(p.mt)
	}
	return s + ")"
}

// reject builds the error for a part the request cannot carry. It matches
// cause, which is types.ErrModalityUnsupported or types.ErrMediaUnavailable
// (both match types.ErrInvalidModelConfig).
func (p partPath) reject(cause error, reason string) error {
	return fmt.Errorf("google: %s: %s: %w", p, reason, cause)
}

// mapper converts messages to Gemini contents for one backend and model.
type mapper struct {
	vertex bool
	model  string
	// base is the index of the first message, so a request built from a
	// suffix of the conversation still reports absolute paths.
	base int
	// names maps a tool call ID to its function name: a function response
	// names the function it answers.
	names map[string]string
}

// mapper returns a mapper for messages[base:]. Tool call names are learned
// from every message, so a function response after a cached prefix still
// names its function.
func (a *Adapter) mapper(messages []types.Message, base int) *mapper {
	m := &mapper{vertex: a.backend.kind == genai.BackendVertexAI, model: a.model, base: base, names: map[string]string{}}
	for _, msg := range messages {
		if am, ok := msg.(types.AssistantMessage); ok {
			for _, c := range types.Each[types.ToolCallPart](am) {
				m.names[c.ID] = c.Name
			}
		}
	}
	return m
}

// contents converts msgs into a system instruction and the turn contents.
// A part Gemini cannot take is rejected with an error naming its path, never
// dropped or replaced by a placeholder. Metadata parts are not sent.
func (m *mapper) contents(msgs []types.Message) (*genai.Content, []*genai.Content, error) {
	var system []*genai.Part
	var contents []*genai.Content
	for i, msg := range msgs {
		at := types.PartPath{Message: m.base + i, Nested: -1}
		switch v := msg.(type) {
		case types.SystemMessage:
			var responses []*genai.Part
			for j, p := range v.Parts {
				at.Part = j
				switch pt := p.(type) {
				case types.TextPart:
					// An empty part has no data, which Vertex reads as a
					// non-text system part and rejects.
					if pt.Text != "" {
						system = append(system, &genai.Part{Text: pt.Text})
					}
				case types.ToolResultPart:
					r, err := m.toolResult(at, pt)
					if err != nil {
						return nil, nil, err
					}
					responses = append(responses, r)
				default:
					if err := m.skip(at, p); err != nil {
						return nil, nil, err
					}
				}
			}
			if len(responses) > 0 {
				contents = append(contents, &genai.Content{Role: genai.RoleUser, Parts: responses})
			}
		case types.UserMessage:
			parts, err := m.userParts(at, v.Parts)
			if err != nil {
				return nil, nil, err
			}
			if len(parts) > 0 {
				contents = append(contents, &genai.Content{Role: genai.RoleUser, Parts: parts})
			}
		case types.AssistantMessage:
			parts, err := m.modelParts(at, v.Parts)
			if err != nil {
				return nil, nil, err
			}
			if len(parts) > 0 {
				contents = append(contents, &genai.Content{Role: genai.RoleModel, Parts: parts})
			}
		}
	}
	var inst *genai.Content
	if len(system) > 0 {
		inst = &genai.Content{Parts: system}
	}
	return inst, contents, nil
}

// skip accepts a metadata part, which is never sent, and rejects anything
// else.
func (m *mapper) skip(at types.PartPath, p types.Part) error {
	if types.IsMetadata(p) {
		return nil
	}
	path := partPath{PartPath: at, kind: p.Kind()}
	if src, ok := types.SourceOf(p); ok {
		path.mt = src.MediaType
	}
	return path.reject(types.ErrModalityUnsupported, "not accepted in this message")
}

// userParts converts a user turn. Function responses go first, as Gemini
// expects them right after the model turn that called the functions.
func (m *mapper) userParts(at types.PartPath, ps []types.UserPart) ([]*genai.Part, error) {
	var responses, parts []*genai.Part
	for j, p := range ps {
		at.Part = j
		switch pt := p.(type) {
		case types.TextPart:
			parts = append(parts, &genai.Part{Text: pt.Text})
		case types.ToolResultPart:
			r, err := m.toolResult(at, pt)
			if err != nil {
				return nil, err
			}
			responses = append(responses, r)
		case types.ImagePart, types.AudioPart, types.VideoPart, types.DocumentPart, types.FilePart:
			gp, err := m.media(at, p)
			if err != nil {
				return nil, err
			}
			parts = append(parts, gp)
		default:
			if err := m.skip(at, p); err != nil {
				return nil, err
			}
		}
	}
	return append(responses, parts...), nil
}

// media converts an input media part. The part's kind and media type must
// be one Gemini reads; the locator is the first the backend can use: this
// adapter's own upload, then a URI the backend can fetch, then inline bytes.
func (m *mapper) media(at types.PartPath, p types.Part) (*genai.Part, error) {
	src, _ := types.SourceOf(p)
	path := partPath{PartPath: at, kind: p.Kind(), mt: src.MediaType}
	mt := baseType(src.MediaType)
	youTube := isYouTube(src.URI)
	var accepted bool
	switch p.(type) {
	case types.ImagePart:
		accepted = imageTypes[mt]
	case types.AudioPart:
		accepted = audioTypes[mt]
	case types.VideoPart:
		// A YouTube link needs no media type.
		accepted = videoTypes[mt] || (mt == "" && youTube)
	case types.DocumentPart:
		accepted = mt == string(types.MediaPDF) || strings.HasPrefix(mt, "text/")
	case types.FilePart:
		return nil, path.reject(types.ErrModalityUnsupported, "Gemini has no opaque file input")
	}
	switch {
	case mt == "" && !accepted:
		return nil, path.reject(types.ErrModalityUnsupported, "a media type is required")
	case !accepted:
		return nil, path.reject(types.ErrModalityUnsupported, "media type not supported for this part kind")
	}
	gp, err := m.locate(path, src, youTube)
	if err != nil {
		return nil, err
	}
	if v, ok := p.(types.VideoPart); ok {
		gp.VideoMetadata = videoMetadata(v.VideoMeta)
	}
	return gp, nil
}

// locate picks the locator for src.
func (m *mapper) locate(path partPath, src types.Source, youTube bool) (*genai.Part, error) {
	if src.Unresolved != "" {
		return nil, path.reject(types.ErrMediaUnavailable, src.Unresolved)
	}
	mt := string(src.MediaType)
	for _, f := range src.Files {
		if f.Provider != providerName {
			continue
		}
		if uri, ok := m.fileURI(f.ID); ok {
			return &genai.Part{FileData: &genai.FileData{FileURI: uri, MIMEType: mt}}, nil
		}
	}
	if src.URI != "" && (youTube || m.fetchable(src.URI)) {
		if mt == "" && youTube {
			// Vertex AI requires a media type on file data.
			mt = "video/*"
		}
		return &genai.Part{FileData: &genai.FileData{FileURI: src.URI, MIMEType: mt}}, nil
	}
	if len(src.Inline) > 0 {
		if len(src.Inline) > maxInlinePart {
			return nil, path.reject(types.ErrModalityUnsupported, "inline media over 20 MB; upload it and pass the URI")
		}
		return &genai.Part{InlineData: &genai.Blob{Data: src.Inline, MIMEType: mt}}, nil
	}
	if src.URI == "" && len(src.Files) == 0 {
		return nil, path.reject(types.ErrMediaUnavailable, "no locator the provider can read")
	}
	return nil, path.reject(types.ErrModalityUnsupported, m.locatorReason(src))
}

// locatorReason explains why none of src's locators fit this backend.
func (m *mapper) locatorReason(src types.Source) string {
	if strings.HasPrefix(src.URI, "gs://") && !m.vertex {
		return "a gs:// URI is read only on Vertex AI"
	}
	if isFilesAPI(src.URI) && m.vertex {
		return "a Gemini API file URI is not readable on Vertex AI"
	}
	if src.URI != "" {
		return "URI scheme not readable by the provider"
	}
	return "no upload for this backend"
}

// fileURI returns the fileData URI of an upload recorded for this adapter:
// on the Gemini API a Files API name or URI, on Vertex a Cloud Storage URI.
func (m *mapper) fileURI(id string) (string, bool) {
	switch {
	case m.vertex && strings.HasPrefix(id, "gs://"):
		return id, true
	case !m.vertex && strings.HasPrefix(id, "files/"):
		return filesAPIBase + id, true
	case !m.vertex && isFilesAPI(id):
		return id, true
	}
	return "", false
}

// fetchable reports whether the backend reads uri itself.
func (m *mapper) fetchable(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "gs":
		return m.vertex
	case "https", "http":
		return !m.vertex || !isFilesAPI(uri)
	}
	return false
}

func isFilesAPI(uri string) bool {
	u, err := url.Parse(uri)
	return err == nil && u.Host == "generativelanguage.googleapis.com" && strings.Contains(u.Path, "/files/")
}

func isYouTube(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return false
	}
	switch strings.TrimPrefix(u.Host, "www.") {
	case "youtube.com", "m.youtube.com", "youtu.be":
		return true
	}
	return false
}

// videoMetadata maps the clip bounds and frame rate, or nil when none is
// set. The SDK sends offsets in whole seconds, rounded.
func videoMetadata(v types.VideoMeta) *genai.VideoMetadata {
	if v.ClipStart == 0 && v.ClipEnd == 0 && v.FPS == 0 {
		return nil
	}
	md := &genai.VideoMetadata{StartOffset: v.ClipStart, EndOffset: v.ClipEnd}
	if v.FPS > 0 {
		fps := v.FPS
		md.FPS = &fps
	}
	return md
}

// toolResult converts a tool result to a function response. Text and JSON
// outputs form the response object; media outputs become the response's
// parts, which Gemini 3 models read natively. Earlier models, and media
// types a function response cannot carry, are rejected.
func (m *mapper) toolResult(at types.PartPath, r types.ToolResultPart) (*genai.Part, error) {
	key := "result"
	if r.IsError {
		key = "error"
	}
	resp := map[string]any{key: r.Text()}
	var media []*genai.FunctionResponsePart
	for k, out := range r.Parts {
		switch o := out.(type) {
		case types.TextPart:
		case types.JSONPart:
			var v any
			if len(o.JSON) > 0 && json.Unmarshal(o.JSON, &v) == nil {
				resp["data"] = v
			}
		default:
			src, _ := types.SourceOf(out)
			path := partPath{PartPath: types.PartPath{Message: at.Message, Part: at.Part, Nested: k}, kind: out.Kind(), mt: src.MediaType}
			fp, err := m.toolMedia(path, src)
			if err != nil {
				return nil, err
			}
			media = append(media, fp)
		}
	}
	name := m.names[r.CallID]
	if name == "" {
		name = r.CallID
	}
	return &genai.Part{FunctionResponse: &genai.FunctionResponse{Name: name, Response: resp, Parts: media}}, nil
}

// toolMedia converts one media output of a tool.
func (m *mapper) toolMedia(path partPath, src types.Source) (*genai.FunctionResponsePart, error) {
	if !mediaToolResults(m.model) {
		return nil, path.reject(types.ErrModalityUnsupported, "this model takes no media in a function response")
	}
	if path.kind == types.KindFile || !toolMediaTypes[baseType(src.MediaType)] {
		return nil, path.reject(types.ErrModalityUnsupported, "media type not supported in a function response")
	}
	gp, err := m.locate(path, src, false)
	if err != nil {
		return nil, err
	}
	if gp.FileData != nil {
		if !m.vertex {
			return nil, path.reject(types.ErrModalityUnsupported, "the Gemini API takes only inline bytes in a function response")
		}
		return &genai.FunctionResponsePart{FileData: &genai.FunctionResponseFileData{FileURI: gp.FileData.FileURI, MIMEType: gp.FileData.MIMEType}}, nil
	}
	return &genai.FunctionResponsePart{InlineData: &genai.FunctionResponseBlob{Data: gp.InlineData.Data, MIMEType: gp.InlineData.MIMEType}}, nil
}

// modelEntry is one converted assistant part, or a bare thought signature
// still looking for the part it belongs to.
type modelEntry struct {
	part *genai.Part
	sig  []byte
}

// modelParts converts an assistant turn for replay.
//
// Gemini attaches thought signatures to parts. A signature with no thought
// text streams as an empty, signed ThinkingPart: before a function call
// (the call carries it), or after the text or media part that carried it.
// On replay such a signature goes back on the function call that follows
// it, and otherwise on the part before it.
func (m *mapper) modelParts(at types.PartPath, ps []types.AssistantPart) ([]*genai.Part, error) {
	var entries []modelEntry
	add := func(p *genai.Part) { entries = append(entries, modelEntry{part: p}) }
	for j, p := range ps {
		at.Part = j
		path := partPath{PartPath: at, kind: p.Kind()}
		switch pt := p.(type) {
		case types.TextPart:
			add(&genai.Part{Text: pt.Text})
		case types.ThinkingPart:
			if pt.Redacted {
				// Redacted reasoning is another vendor's opaque form; Gemini
				// cannot read it and does not need it.
				continue
			}
			sig := decodeSignature(pt.Signature)
			if pt.Text == "" {
				if len(sig) > 0 {
					entries = append(entries, modelEntry{sig: sig})
				}
				continue
			}
			add(&genai.Part{Text: pt.Text, Thought: true, ThoughtSignature: sig})
		case types.ToolCallPart:
			add(&genai.Part{FunctionCall: &genai.FunctionCall{Name: pt.Name, Args: pt.Arguments}})
		case types.ServerToolCallPart:
			if pt.ToolKind == types.ServerToolCodeExecution {
				add(&genai.Part{ExecutableCode: replayCode(pt)})
			}
		case types.ServerToolResultPart:
			if pt.ToolKind == types.ServerToolCodeExecution {
				add(&genai.Part{CodeExecutionResult: replayCodeResult(pt)})
			}
		case types.ImageOutPart:
			path.mt = pt.Source.MediaType
			gp, err := m.locate(path, pt.Source, false)
			if err != nil {
				return nil, err
			}
			gp.ThoughtSignature = decodeSignature(pt.Signature)
			add(gp)
		case types.AudioOutPart:
			path.mt = pt.Source.MediaType
			gp, err := m.locate(path, pt.Source, false)
			switch {
			case err == nil:
				add(gp)
			case errors.Is(err, types.ErrMediaUnavailable) && pt.Transcript != "":
				add(&genai.Part{Text: pt.Transcript})
			default:
				return nil, err
			}
		case types.RefusalPart:
			if pt.Text != "" {
				add(&genai.Part{Text: pt.Text})
			}
		case types.CitationPart:
			// Grounding is not replayed; the cited text is.
		case types.VideoOutPart:
			path.mt = pt.Source.MediaType
			return nil, path.reject(types.ErrModalityUnsupported, "video output cannot be replayed")
		default:
			if err := m.skip(at, p); err != nil {
				return nil, err
			}
		}
	}
	return placeSignatures(entries), nil
}

// placeSignatures attaches each bare signature to the function call right
// after it, else to the part before it, else to the next part.
func placeSignatures(entries []modelEntry) []*genai.Part {
	var out []*genai.Part
	for i, e := range entries {
		if e.part != nil {
			out = append(out, e.part)
			continue
		}
		if i+1 < len(entries) && entries[i+1].part != nil && entries[i+1].part.FunctionCall != nil &&
			len(entries[i+1].part.ThoughtSignature) == 0 {
			entries[i+1].part.ThoughtSignature = e.sig
			continue
		}
		if n := len(out); n > 0 && len(out[n-1].ThoughtSignature) == 0 {
			out[n-1].ThoughtSignature = e.sig
			continue
		}
		for k := i + 1; k < len(entries); k++ {
			if p := entries[k].part; p != nil && len(p.ThoughtSignature) == 0 {
				p.ThoughtSignature = e.sig
				break
			}
		}
	}
	return out
}

// decodeSignature decodes a base64 thought signature. An unparseable one is
// dropped rather than failing the turn: the part is still sent.
func decodeSignature(s string) []byte {
	if s == "" {
		return nil
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}

func encodeSignature(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

// replayCode rebuilds the executable code part of a code execution call.
func replayCode(c types.ServerToolCallPart) *genai.ExecutableCode {
	code, _ := c.Input["code"].(string)
	lang, _ := c.Input["language"].(string)
	if lang == "" {
		lang = string(genai.LanguagePython)
	}
	return &genai.ExecutableCode{Code: code, Language: genai.Language(lang)}
}

// replayCodeResult rebuilds a code execution result, from the native
// payload when it was kept.
func replayCodeResult(r types.ServerToolResultPart) *genai.CodeExecutionResult {
	var out genai.CodeExecutionResult
	if len(r.Result) == 0 || json.Unmarshal(r.Result, &out) != nil {
		out = genai.CodeExecutionResult{Output: r.Text, Outcome: genai.OutcomeOK}
		if r.IsError {
			out.Outcome = genai.OutcomeFailed
		}
	}
	out.ID = ""
	return &out
}
