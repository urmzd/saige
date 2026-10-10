package anthropic

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/urmzd/saige/agent/types"
)

// providerName is the vendor recorded on Anthropic Files API uploads.
const providerName = "anthropic"

// serverToolIDPrefix starts the ID of every server tool call Anthropic runs.
// Only calls with it are replayed: another vendor's server tool call cannot
// be sent back to this API.
const serverToolIDPrefix = "srvtoolu_"

// WithEndpoint names the endpoint (the Anthropic workspace) the adapter's
// key belongs to. Files API IDs are scoped to a workspace, so a media
// source's Anthropic file is sent only when it was recorded for this
// endpoint; with no endpoint set, only files recorded without one are sent.
// A file ID recorded for another endpoint is never sent: the part falls back
// to its URL or bytes, or the request is rejected. Files that code execution
// produces are recorded under this endpoint.
func WithEndpoint(name string) Option {
	return func(a *Adapter) { a.endpoint = name }
}

// partError reports a part the request cannot carry. It names the part's
// position, kind and media type, and wraps types.ErrModalityUnsupported or
// types.ErrMediaUnavailable, which both match types.ErrInvalidModelConfig.
func partError(path types.PartPath, p types.Part, sentinel error, reason string) error {
	desc := string(p.Kind())
	if src, ok := types.SourceOf(p); ok && src.MediaType != "" {
		desc += " " + string(src.MediaType)
	}
	return fmt.Errorf("anthropic: part %s (%s): %s: %w", path, desc, reason, sentinel)
}

// requestMapper converts typed parts to Messages API content blocks.
type requestMapper struct {
	endpoint string
	// codeExec is set when the request offers code execution, the only tool
	// that reads an opaque file (as a container upload).
	codeExec bool
	// servers are the server tool kinds the request offers; a past server
	// tool call is replayed only when its tool is offered again.
	servers map[types.ServerToolKind]bool
}

func (a *Adapter) mapper() requestMapper {
	m := requestMapper{endpoint: a.endpoint, servers: map[types.ServerToolKind]bool{}}
	for _, st := range a.serverTools {
		m.servers[st.Kind] = true
		if st.Kind == types.ServerToolCodeExecution {
			m.codeExec = true
		}
	}
	return m
}

// toAnthropicParams converts the conversation to system blocks and
// messages. A part the API cannot take, such as audio or video, fails the
// whole request before anything is sent; nothing is dropped or replaced
// with a placeholder.
func (a *Adapter) toAnthropicParams(msgs []types.Message) ([]anthropic.TextBlockParam, []anthropic.MessageParam, error) {
	return a.mapper().messages(msgs)
}

func (m requestMapper) messages(msgs []types.Message) ([]anthropic.TextBlockParam, []anthropic.MessageParam, error) {
	var system []anthropic.TextBlockParam
	var out []anthropic.MessageParam

	for mi, msg := range msgs {
		switch v := msg.(type) {
		case types.SystemMessage:
			for pi, c := range v.Parts {
				path := types.PartPath{Message: mi, Part: pi, Nested: -1}
				switch bc := c.(type) {
				case types.TextPart:
					// The API rejects an empty text block, so a blank system
					// prompt is dropped; with none left, no system is sent.
					if strings.TrimSpace(bc.Text) != "" {
						system = append(system, anthropic.TextBlockParam{Text: bc.Text})
					}
				case types.ToolResultPart:
					b, err := m.toolResult(path, bc)
					if err != nil {
						return nil, nil, err
					}
					out = appendMsg(out, "user", b)
				default:
					if !types.IsMetadata(c) {
						return nil, nil, partError(path, c, types.ErrModalityUnsupported, "not accepted in a system message")
					}
				}
			}

		case types.UserMessage:
			for pi, c := range v.Parts {
				path := types.PartPath{Message: mi, Part: pi, Nested: -1}
				b, ok, err := m.userBlock(path, c)
				if err != nil {
					return nil, nil, err
				}
				if ok {
					out = appendMsg(out, "user", b)
				}
			}

		case types.AssistantMessage:
			blocks, err := m.assistantBlocks(mi, v)
			if err != nil {
				return nil, nil, err
			}
			for _, b := range blocks {
				out = appendMsg(out, "assistant", b)
			}
		}
	}

	return system, trimPrefill(out), nil
}

// userBlock converts one user part. Metadata parts are not sent.
func (m requestMapper) userBlock(path types.PartPath, c types.UserPart) (anthropic.ContentBlockParamUnion, bool, error) {
	switch bc := c.(type) {
	case types.TextPart:
		return anthropic.NewTextBlock(bc.Text), true, nil
	case types.ToolResultPart:
		b, err := m.toolResult(path, bc)
		return b, err == nil, err
	case types.ImagePart:
		img, err := m.image(path, bc)
		if err != nil {
			return anthropic.ContentBlockParamUnion{}, false, err
		}
		return anthropic.ContentBlockParamUnion{OfImage: img}, true, nil
	case types.DocumentPart:
		doc, err := m.document(path, bc)
		if err != nil {
			return anthropic.ContentBlockParamUnion{}, false, err
		}
		return anthropic.ContentBlockParamUnion{OfDocument: doc}, true, nil
	case types.FilePart:
		up, err := m.containerUpload(path, bc)
		if err != nil {
			return anthropic.ContentBlockParamUnion{}, false, err
		}
		return anthropic.ContentBlockParamUnion{OfContainerUpload: up}, true, nil
	case types.AudioPart, types.VideoPart:
		return anthropic.ContentBlockParamUnion{}, false, partError(path, c, types.ErrModalityUnsupported, "Anthropic accepts no audio or video input")
	}
	if types.IsMetadata(c) {
		return anthropic.ContentBlockParamUnion{}, false, nil
	}
	return anthropic.ContentBlockParamUnion{}, false, partError(path, c, types.ErrModalityUnsupported, "not accepted in a user message")
}

// assistantBlocks converts an assistant turn for replay. Thinking keeps its
// signature (redacted thinking its data), and a server tool call and its
// result are replayed natively when the call is Anthropic's and the request
// offers the tool again. Citations and refusals annotate the turn and are
// not content blocks.
func (m requestMapper) assistantBlocks(mi int, v types.AssistantMessage) ([]anthropic.ContentBlockParamUnion, error) {
	replay := m.replayableServerCalls(v.Parts)
	var out []anthropic.ContentBlockParamUnion
	for pi, c := range v.Parts {
		path := types.PartPath{Message: mi, Part: pi, Nested: -1}
		switch bc := c.(type) {
		case types.ThinkingPart:
			if bc.Redacted {
				out = append(out, anthropic.ContentBlockParamUnion{OfRedactedThinking: &anthropic.RedactedThinkingBlockParam{Data: bc.Signature}})
				continue
			}
			out = append(out, anthropic.NewThinkingBlock(bc.Signature, bc.Text))
		case types.TextPart:
			out = append(out, anthropic.NewTextBlock(bc.Text))
		case types.ToolCallPart:
			out = append(out, anthropic.NewToolUseBlock(bc.ID, bc.Arguments, bc.Name))
		case types.ServerToolCallPart:
			if name, ok := replay[bc.ID]; ok {
				input := any(bc.Input)
				if bc.Input == nil {
					input = map[string]any{}
				}
				out = append(out, anthropic.ContentBlockParamUnion{OfServerToolUse: &anthropic.ServerToolUseBlockParam{
					ID: bc.ID, Name: anthropic.ServerToolUseBlockParamName(name), Input: input}})
			}
		case types.ServerToolResultPart:
			if name, ok := replay[bc.CallID]; ok {
				raw, err := json.Marshal(struct {
					Type      string          `json:"type"`
					ToolUseID string          `json:"tool_use_id"`
					Content   json.RawMessage `json:"content"`
				}{name + "_tool_result", bc.CallID, bc.Result})
				if err != nil {
					return nil, partError(path, c, types.ErrModalityUnsupported, "result payload: "+err.Error())
				}
				out = append(out, param.Override[anthropic.ContentBlockParamUnion](json.RawMessage(raw)))
			}
		case types.CitationPart, types.RefusalPart:
		case types.AudioOutPart, types.ImageOutPart, types.VideoOutPart:
			return nil, partError(path, c, types.ErrModalityUnsupported, "Anthropic accepts no generated media in an assistant turn")
		default:
			if !types.IsMetadata(c) {
				return nil, partError(path, c, types.ErrModalityUnsupported, "not accepted in an assistant message")
			}
		}
	}
	return out, nil
}

// replayableServerCalls returns the native tool name of each server tool
// call in parts that can be sent back: an Anthropic call whose result, with
// its native payload, is in the same turn, for a tool the request offers.
// Other server tool parts are records of provider-side work, not content,
// and are left out as before.
func (m requestMapper) replayableServerCalls(parts []types.AssistantPart) map[string]string {
	var replay map[string]string
	for _, pair := range types.PairServerTools(parts) {
		c, r := pair.Call, pair.Result
		if r == nil || len(r.Result) == 0 || !strings.HasPrefix(c.ID, serverToolIDPrefix) || !m.servers[c.ToolKind] {
			continue
		}
		name := c.Name
		if name == "" {
			name = string(c.ToolKind)
		}
		if replay == nil {
			replay = map[string]string{}
		}
		replay[c.ID] = name
	}
	return replay
}

// toToolResultBlock converts a tool result. Text-only output is sent as a
// string; output with media is sent as content blocks of text, images and
// documents. Audio, video and opaque files fail the request.
func (m requestMapper) toolResult(path types.PartPath, c types.ToolResultPart) (anthropic.ContentBlockParamUnion, error) {
	if !hasRichOutput(c.Parts) {
		return anthropic.NewToolResultBlock(c.CallID, c.Text(), c.IsError), nil
	}
	content := make([]anthropic.ToolResultBlockParamContentUnion, 0, len(c.Parts))
	for ni, p := range c.Parts {
		nested := types.PartPath{Message: path.Message, Part: path.Part, Nested: ni}
		switch v := p.(type) {
		case types.TextPart:
			if v.Text != "" {
				content = append(content, anthropic.ToolResultBlockParamContentUnion{OfText: &anthropic.TextBlockParam{Text: v.Text}})
			}
		case types.JSONPart:
			content = append(content, anthropic.ToolResultBlockParamContentUnion{OfText: &anthropic.TextBlockParam{Text: string(v.JSON)}})
		case types.ImagePart:
			img, err := m.image(nested, v)
			if err != nil {
				return anthropic.ContentBlockParamUnion{}, err
			}
			content = append(content, anthropic.ToolResultBlockParamContentUnion{OfImage: img})
		case types.DocumentPart:
			doc, err := m.document(nested, v)
			if err != nil {
				return anthropic.ContentBlockParamUnion{}, err
			}
			content = append(content, anthropic.ToolResultBlockParamContentUnion{OfDocument: doc})
		default:
			return anthropic.ContentBlockParamUnion{}, partError(nested, p, types.ErrModalityUnsupported, "a tool result carries only text, images and documents")
		}
	}
	return anthropic.ContentBlockParamUnion{OfToolResult: &anthropic.ToolResultBlockParam{
		ToolUseID: c.CallID, IsError: anthropic.Bool(c.IsError), Content: content}}, nil
}

// hasRichOutput reports tool output that is not text alone.
func hasRichOutput(parts []types.ToolOutputPart) bool {
	for _, p := range parts {
		if _, ok := p.(types.TextPart); !ok {
			return true
		}
	}
	return false
}

// located is the locator chosen for one media part.
type located struct {
	kind   types.SourceKind
	fileID string
	url    string
	data   []byte
}

// locate picks how to send src: an Anthropic file scoped to this endpoint,
// then an https URL (when the block accepts one), then the bytes. It fails
// when none is usable, naming why.
func (m requestMapper) locate(path types.PartPath, p types.Part, src types.Source, urlOK bool) (located, error) {
	if src.Unresolved != "" {
		return located{}, partError(path, p, types.ErrMediaUnavailable, src.Unresolved)
	}
	if id, ok := m.fileID(src); ok {
		return located{kind: types.SourceFile, fileID: id}, nil
	}
	foreign, mismatch, expired := "", false, false
	for _, f := range src.Files {
		switch {
		case f.Provider != providerName:
		case f.Endpoint == m.endpoint:
			expired = true
		default:
			foreign, mismatch = f.Endpoint, true
		}
	}
	if urlOK && isHTTPS(src.URI) {
		return located{kind: types.SourceURI, url: src.URI}, nil
	}
	if len(src.Inline) > 0 {
		return located{kind: types.SourceInline, data: src.Inline}, nil
	}
	switch {
	case expired:
		return located{}, partError(path, p, types.ErrMediaUnavailable, "its Anthropic file ID has expired, and it has no URL or bytes")
	case mismatch:
		return located{}, partError(path, p, types.ErrModalityUnsupported,
			fmt.Sprintf("its Anthropic file ID is scoped to endpoint %q, not this adapter's %q, and it has no URL or bytes", foreign, m.endpoint))
	case src.URI != "" && urlOK:
		return located{}, partError(path, p, types.ErrModalityUnsupported, "only an https URL can be sent, not "+uriScheme(src.URI)+": URIs")
	case src.URI != "":
		return located{}, partError(path, p, types.ErrModalityUnsupported, "this media type cannot be sent by URL; it needs bytes or an Anthropic file ID")
	case len(src.Files) > 0:
		return located{}, partError(path, p, types.ErrModalityUnsupported, "it has file IDs for other providers only")
	default:
		return located{}, partError(path, p, types.ErrMediaUnavailable, "no locator the provider can read; resolve the workspace reference or attach the bytes")
	}
}

// fileID returns the source's Anthropic file recorded for exactly this
// endpoint. Unlike Source.VendorFile, an unscoped ID does not match a scoped
// adapter, nor a scoped ID an unscoped one: a file ID is never sent to a
// workspace it was not recorded for. An expired upload is skipped.
func (m requestMapper) fileID(src types.Source) (string, bool) {
	for _, f := range src.Files {
		if f.Provider == providerName && f.Endpoint == m.endpoint && (f.ExpiresAt.IsZero() || time.Now().Before(f.ExpiresAt)) {
			return f.ID, true
		}
	}
	return "", false
}

func isHTTPS(u string) bool {
	parsed, err := url.Parse(u)
	return err == nil && parsed.Scheme == "https" && parsed.Host != ""
}

func uriScheme(u string) string {
	if parsed, err := url.Parse(u); err == nil && parsed.Scheme != "" {
		return parsed.Scheme
	}
	return "relative"
}

// baseMediaType lowercases mt and strips parameters such as charset.
func baseMediaType(mt types.MediaType) types.MediaType {
	base, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(string(mt))), ";")
	return types.MediaType(strings.TrimSpace(base))
}

func isImageType(mt types.MediaType) bool {
	switch mt {
	case types.MediaJPEG, types.MediaPNG, types.MediaGIF, types.MediaWebP:
		return true
	}
	return false
}

// image maps an image part: base64, URL or file. The API reads JPEG, PNG,
// GIF and WebP.
func (m requestMapper) image(path types.PartPath, p types.ImagePart) (*anthropic.ImageBlockParam, error) {
	mt := baseMediaType(p.Source.MediaType)
	if mt != "" && !isImageType(mt) {
		return nil, partError(path, p, types.ErrModalityUnsupported, "Anthropic reads JPEG, PNG, GIF and WebP images only")
	}
	loc, err := m.locate(path, p, p.Source, true)
	if err != nil {
		return nil, err
	}
	var src anthropic.ImageBlockParamSourceUnion
	switch loc.kind {
	case types.SourceFile:
		src.OfFile = &anthropic.FileImageSourceParam{FileID: loc.fileID}
	case types.SourceURI:
		src.OfURL = &anthropic.URLImageSourceParam{URL: loc.url}
	default:
		if mt == "" {
			return nil, partError(path, p, types.ErrModalityUnsupported, "inline image bytes need a media type")
		}
		src.OfBase64 = &anthropic.Base64ImageSourceParam{Data: base64.StdEncoding.EncodeToString(loc.data),
			MediaType: anthropic.Base64ImageSourceMediaType(mt)}
	}
	return &anthropic.ImageBlockParam{Source: src}, nil
}

// document maps a document part: a PDF by base64, URL or file, or plain
// text by its bytes or a file. Title, context and citations pass through.
func (m requestMapper) document(path types.PartPath, p types.DocumentPart) (*anthropic.DocumentBlockParam, error) {
	mt := baseMediaType(p.Source.MediaType)
	var src anthropic.DocumentBlockParamSourceUnion
	switch mt {
	case types.MediaPDF:
		loc, err := m.locate(path, p, p.Source, true)
		if err != nil {
			return nil, err
		}
		switch loc.kind {
		case types.SourceFile:
			src.OfFile = &anthropic.FileDocumentSourceParam{FileID: loc.fileID}
		case types.SourceURI:
			src.OfURL = &anthropic.URLPDFSourceParam{URL: loc.url}
		default:
			src.OfBase64 = &anthropic.Base64PDFSourceParam{Data: base64.StdEncoding.EncodeToString(loc.data)}
		}
	case types.MediaText:
		loc, err := m.locate(path, p, p.Source, false)
		if err != nil {
			return nil, err
		}
		if loc.kind == types.SourceFile {
			src.OfFile = &anthropic.FileDocumentSourceParam{FileID: loc.fileID}
			break
		}
		if !utf8.Valid(loc.data) {
			return nil, partError(path, p, types.ErrModalityUnsupported, "plain text must be valid UTF-8")
		}
		src.OfText = &anthropic.PlainTextSourceParam{Data: string(loc.data)}
	default:
		return nil, partError(path, p, types.ErrModalityUnsupported, "Anthropic reads PDF and text/plain documents only")
	}
	doc := &anthropic.DocumentBlockParam{Source: src}
	if p.Title != "" {
		doc.Title = anthropic.String(p.Title)
	}
	if p.Context != "" {
		doc.Context = anthropic.String(p.Context)
	}
	if p.Citations {
		doc.Citations = anthropic.CitationsConfigParam{Enabled: anthropic.Bool(true)}
	}
	return doc, nil
}

// containerUpload maps an opaque file to a code execution upload, the only
// way the API takes one. It needs code execution in the request and an
// Anthropic file ID for this endpoint.
func (m requestMapper) containerUpload(path types.PartPath, p types.FilePart) (*anthropic.ContainerUploadBlockParam, error) {
	if !m.codeExec {
		return nil, partError(path, p, types.ErrModalityUnsupported, "an opaque file is sent only as a code execution upload, and the request does not enable code execution")
	}
	if id, ok := m.fileID(p.Source); ok {
		return &anthropic.ContainerUploadBlockParam{FileID: id}, nil
	}
	if _, err := m.locate(path, p, p.Source, false); err != nil {
		return nil, err
	}
	return nil, partError(path, p, types.ErrModalityUnsupported, "a code execution upload needs an Anthropic file ID for this endpoint")
}
