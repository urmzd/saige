package types

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Wire version 1 streamed model output as content-specific deltas without
// indices. They stay decodable (D-35): the unexported types below are what
// a v1 envelope decodes to, Decoder and NewV1Upgrader turn them into part
// deltas, and NewV1Downgrader produces them for a v1 encoder. The agent
// loop, the aggregator and every consumer in this module see only part
// deltas.

// ErrWireUnrepresentable reports output that has no form in the wire
// version a client asked for, such as an image or a refusal for a v1
// client. The encoder reports it as an error envelope instead of dropping
// the output.
var ErrWireUnrepresentable = errors.New("output has no form in this wire version")

// v1TextStart signals the beginning of a text block.
type v1TextStart struct{}

func (v1TextStart) isDelta() {}

// v1TextContent carries an incremental text fragment.
type v1TextContent struct {
	Content string
}

func (v1TextContent) isDelta() {}

// v1TextEnd signals the end of a text block.
type v1TextEnd struct{}

func (v1TextEnd) isDelta() {}

// v1ToolCallStart signals the LLM is generating a tool call.
type v1ToolCallStart struct {
	ID   string
	Name string
}

func (v1ToolCallStart) isDelta() {}

// v1ToolCallArgument carries a JSON fragment of arguments. An empty ID
// means the most recently started call.
type v1ToolCallArgument struct {
	ID      string
	Content string
}

func (v1ToolCallArgument) isDelta() {}

// v1ToolCallEnd signals the LLM finished generating a tool call. An
// empty ID closes the oldest open call.
type v1ToolCallEnd struct {
	ID             string
	Arguments      map[string]any
	ArgumentsError string
}

func (v1ToolCallEnd) isDelta() {}

// v1ThinkingStart signals the beginning of an extended thinking block.
type v1ThinkingStart struct{}

func (v1ThinkingStart) isDelta() {}

// v1ThinkingContent carries an incremental thinking fragment.
type v1ThinkingContent struct {
	Content string
}

func (v1ThinkingContent) isDelta() {}

// v1ThinkingEnd signals the end of an extended thinking block.
type v1ThinkingEnd struct {
	Signature string
}

func (v1ThinkingEnd) isDelta() {}

// v1ServerToolCall reports a tool call the provider executes itself.
type v1ServerToolCall struct {
	ID    string
	Kind  ServerToolKind
	Name  string
	Input map[string]any
}

func (v1ServerToolCall) isDelta() {}

// v1ServerToolResult carries the outcome of a ServerToolCallDelta.
type v1ServerToolResult struct {
	ID      string
	Kind    ServerToolKind
	Text    string
	Result  json.RawMessage
	IsError bool
	Files   []wireFile
}

func (v1ServerToolResult) isDelta() {}

// DecodeToolArguments parses streamed argument text. Empty text is a call
// with no arguments. Text that is not a JSON object yields an error message
// and nil arguments, never an empty map that would look like a valid call
// (D-36).
func DecodeToolArguments(raw string) (map[string]any, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ""
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, err.Error()
	}
	return args, ""
}

// ── Upgrading v1 to v2 ───────────────────────────────────────────────

// NewV1Upgrader returns a function that turns one delta of a v1 stream into
// the part deltas it means. It keeps state, so use one per stream. Indices
// are assigned in start order; a tool call's arguments and end are matched
// by ID, an argument without an ID goes to the newest open call, and an end
// without an ID closes the oldest, as v1 specified. Model citations become
// citation parts. Deltas that are not v1 content deltas pass through, and
// deltas nested in a ToolExecDelta are upgraded with their own state per
// tool call.
func NewV1Upgrader() func(Delta) []Delta {
	u := newV1Upgrader()
	return u.upgrade
}

type v1Tool struct {
	idx      int
	id, name string
	args     strings.Builder
}

type v1Upgrader struct {
	next        int
	text, think int
	tools       []*v1Tool
	nested      map[string]*v1Upgrader
}

func newV1Upgrader() *v1Upgrader { return &v1Upgrader{text: -1, think: -1} }

func (u *v1Upgrader) alloc() int {
	i := u.next
	u.next++
	return i
}

//nolint:gocyclo // one case per v1 delta
func (u *v1Upgrader) upgrade(d Delta) []Delta {
	switch v := d.(type) {
	case v1TextStart:
		u.text = u.alloc()
		return []Delta{PartStart{Index: u.text, Kind: KindText}}
	case v1TextContent:
		var out []Delta
		if u.text < 0 {
			// v1 allowed text without a start; open one rather than drop it.
			u.text = u.alloc()
			out = append(out, PartStart{Index: u.text, Kind: KindText})
		}
		if v.Content == "" {
			return out
		}
		return append(out, PartDelta{Index: u.text, Text: v.Content})
	case v1TextEnd:
		if u.text < 0 {
			return nil
		}
		i := u.text
		u.text = -1
		return []Delta{PartEnd{Index: i}}
	case v1ThinkingStart:
		u.think = u.alloc()
		return []Delta{PartStart{Index: u.think, Kind: KindThinking}}
	case v1ThinkingContent:
		var out []Delta
		if u.think < 0 {
			u.think = u.alloc()
			out = append(out, PartStart{Index: u.think, Kind: KindThinking})
		}
		if v.Content == "" {
			return out
		}
		return append(out, PartDelta{Index: u.think, Thinking: v.Content})
	case v1ThinkingEnd:
		if u.think < 0 {
			return nil
		}
		i := u.think
		u.think = -1
		var out []Delta
		if v.Signature != "" {
			out = append(out, PartDelta{Index: i, Signature: v.Signature})
		}
		return append(out, PartEnd{Index: i})
	case v1ToolCallStart:
		// A repeated start for an open ID restarts that call at its index.
		if v.ID != "" {
			for i, t := range u.tools {
				if t.id == v.ID {
					u.tools = append(u.tools[:i], u.tools[i+1:]...)
					u.tools = append(u.tools, &v1Tool{idx: t.idx, id: v.ID, name: v.Name})
					return []Delta{PartStart{Index: t.idx, Kind: KindToolCall, ID: v.ID, Name: v.Name}}
				}
			}
		}
		t := &v1Tool{idx: u.alloc(), id: v.ID, name: v.Name}
		u.tools = append(u.tools, t)
		return []Delta{PartStart{Index: t.idx, Kind: KindToolCall, ID: v.ID, Name: v.Name}}
	case v1ToolCallArgument:
		t := u.argTarget(v.ID)
		if t == nil || v.Content == "" {
			return nil
		}
		t.args.WriteString(v.Content)
		return []Delta{PartDelta{Index: t.idx, Args: v.Content}}
	case v1ToolCallEnd:
		i := u.endTarget(v.ID)
		if i < 0 {
			return nil
		}
		t := u.tools[i]
		u.tools = append(u.tools[:i], u.tools[i+1:]...)
		args, argsErr := v.Arguments, v.ArgumentsError
		if args == nil && argsErr == "" {
			args, argsErr = DecodeToolArguments(t.args.String())
		}
		if argsErr != "" {
			args = nil
		}
		return []Delta{PartEnd{Index: t.idx, Part: ToolCallPart{ID: t.id, Name: t.name, Arguments: args, ArgumentsError: argsErr}}}
	case v1ServerToolCall:
		i := u.alloc()
		return []Delta{
			PartStart{Index: i, Kind: KindServerToolCall, ID: v.ID, Name: v.Name},
			PartEnd{Index: i, Part: ServerToolCallPart{ID: v.ID, ToolKind: v.Kind, Name: v.Name, Input: v.Input}},
		}
	case v1ServerToolResult:
		i := u.alloc()
		p := ServerToolResultPart{CallID: v.ID, ToolKind: v.Kind, Text: v.Text, Result: v.Result, IsError: v.IsError}
		for _, f := range v.Files {
			p.Outputs = append(p.Outputs, Media(f.source()))
		}
		return []Delta{PartStart{Index: i, Kind: KindServerToolResult, ID: v.ID}, PartEnd{Index: i, Part: p}}
	case CitationDelta:
		if v.ToolCallID != "" {
			return []Delta{v}
		}
		i := u.alloc()
		return []Delta{PartStart{Index: i, Kind: KindCitation}, PartEnd{Index: i, Part: CitationPart{Citation: v.Citation}}}
	case ToolExecDelta:
		if u.nested == nil {
			u.nested = map[string]*v1Upgrader{}
		}
		n := u.nested[v.ToolCallID]
		if n == nil {
			n = newV1Upgrader()
			u.nested[v.ToolCallID] = n
		}
		inner := n.upgrade(v.Inner)
		out := make([]Delta, len(inner))
		for k, d := range inner {
			out[k] = ToolExecDelta{ToolCallID: v.ToolCallID, Inner: d}
		}
		return out
	default:
		return []Delta{d}
	}
}

func (u *v1Upgrader) argTarget(id string) *v1Tool {
	if id != "" {
		for _, t := range u.tools {
			if t.id == id {
				return t
			}
		}
		return nil
	}
	if len(u.tools) == 0 {
		return nil
	}
	return u.tools[len(u.tools)-1]
}

func (u *v1Upgrader) endTarget(id string) int {
	if id != "" {
		for i, t := range u.tools {
			if t.id == id {
				return i
			}
		}
		return -1
	}
	if len(u.tools) == 0 {
		return -1
	}
	return 0
}

// ── Downgrading v2 to v1 ─────────────────────────────────────────────

// NewV1Downgrader returns a function that turns one delta of a part stream
// into its v1 form, for a client that reads only wire version 1. It keeps
// state, so use one per stream. Output with no v1 form (refusals and
// produced media) becomes an ErrorDelta matching ErrWireUnrepresentable,
// once per part, and is never dropped silently. Conversion reports have no
// v1 form and are omitted; they describe the request, not its output.
func NewV1Downgrader() func(Delta) []Delta {
	g := newV1Downgrader()
	return g.downgrade
}

type v2Open struct {
	kind     PartKind
	id, name string
	text     strings.Builder
	sig      strings.Builder
	args     strings.Builder
	bad      bool
}

type v1Downgrader struct {
	open   map[int]*v2Open
	nested map[string]*v1Downgrader
}

func newV1Downgrader() *v1Downgrader { return &v1Downgrader{open: map[int]*v2Open{}} }

func unrepresentable(k PartKind) Delta {
	return ErrorDelta{Error: fmt.Errorf("%w: %s part in wire version 1", ErrWireUnrepresentable, k)}
}

func v1Representable(k PartKind) bool {
	switch k {
	case KindText, KindThinking, KindToolCall, KindServerToolCall, KindServerToolResult, KindCitation:
		return true
	}
	return false
}

//nolint:gocyclo // one case per part kind
func (g *v1Downgrader) downgrade(d Delta) []Delta {
	switch v := d.(type) {
	case PartStart:
		o := &v2Open{kind: v.Kind, id: v.ID, name: v.Name}
		g.open[v.Index] = o
		if !v1Representable(v.Kind) {
			o.bad = true
			return []Delta{unrepresentable(v.Kind)}
		}
		switch v.Kind {
		case KindText:
			return []Delta{v1TextStart{}}
		case KindThinking:
			return []Delta{v1ThinkingStart{}}
		case KindToolCall:
			return []Delta{v1ToolCallStart{ID: v.ID, Name: v.Name}}
		}
		return nil
	case PartDelta:
		o := g.open[v.Index]
		if o == nil || o.bad {
			return nil
		}
		switch {
		case v.Text != "" && o.kind == KindText:
			o.text.WriteString(v.Text)
			return []Delta{v1TextContent{Content: v.Text}}
		case v.Thinking != "" && o.kind == KindThinking:
			o.text.WriteString(v.Thinking)
			return []Delta{v1ThinkingContent{Content: v.Thinking}}
		case v.Signature != "" && o.kind == KindThinking:
			o.sig.WriteString(v.Signature)
		case v.Args != "" && o.kind == KindToolCall:
			o.args.WriteString(v.Args)
			return []Delta{v1ToolCallArgument{ID: o.id, Content: v.Args}}
		}
		return nil
	case PartEnd:
		o := g.open[v.Index]
		delete(g.open, v.Index)
		if o == nil {
			if v.Part == nil {
				return nil
			}
			o = &v2Open{kind: v.Part.Kind()}
			if !v1Representable(o.kind) {
				return []Delta{unrepresentable(o.kind)}
			}
		}
		if o.bad {
			return nil
		}
		return g.end(o, v.Part)
	case ToolExecDelta:
		if g.nested == nil {
			g.nested = map[string]*v1Downgrader{}
		}
		n := g.nested[v.ToolCallID]
		if n == nil {
			n = newV1Downgrader()
			g.nested[v.ToolCallID] = n
		}
		inner := n.downgrade(v.Inner)
		out := make([]Delta, 0, len(inner))
		for _, d := range inner {
			if _, isErr := d.(ErrorDelta); isErr {
				// An unrepresentable inner part reports at the top level,
				// where a v1 client looks for errors.
				out = append(out, d)
				continue
			}
			out = append(out, ToolExecDelta{ToolCallID: v.ToolCallID, Inner: d})
		}
		return out
	case ConversionDelta:
		return nil
	default:
		return []Delta{d}
	}
}

//nolint:gocyclo // one case per part kind
func (g *v1Downgrader) end(o *v2Open, part AssistantPart) []Delta {
	switch o.kind {
	case KindText:
		var out []Delta
		if t, ok := part.(TextPart); ok && o.text.Len() == 0 && t.Text != "" {
			out = append(out, v1TextContent{Content: t.Text})
		}
		return append(out, v1TextEnd{})
	case KindThinking:
		var out []Delta
		sig := o.sig.String()
		if t, ok := part.(ThinkingPart); ok {
			if o.text.Len() == 0 && t.Text != "" {
				out = append(out, v1ThinkingContent{Content: t.Text})
			}
			if t.Signature != "" {
				sig = t.Signature
			}
		}
		return append(out, v1ThinkingEnd{Signature: sig})
	case KindToolCall:
		if tc, ok := part.(ToolCallPart); ok {
			return []Delta{v1ToolCallEnd{ID: firstNonEmpty(tc.ID, o.id), Arguments: tc.Arguments, ArgumentsError: tc.ArgumentsError}}
		}
		args, argsErr := DecodeToolArguments(o.args.String())
		return []Delta{v1ToolCallEnd{ID: o.id, Arguments: args, ArgumentsError: argsErr}}
	case KindServerToolCall:
		if sc, ok := part.(ServerToolCallPart); ok {
			return []Delta{v1ServerToolCall{ID: sc.ID, Kind: sc.ToolKind, Name: sc.Name, Input: sc.Input}}
		}
		return []Delta{v1ServerToolCall{ID: o.id, Name: o.name}}
	case KindServerToolResult:
		sr, ok := part.(ServerToolResultPart)
		if !ok {
			return []Delta{v1ServerToolResult{ID: o.id}}
		}
		r := v1ServerToolResult{ID: sr.CallID, Kind: sr.ToolKind, Text: sr.Text, Result: sr.Result, IsError: sr.IsError}
		for _, p := range sr.Outputs {
			src, ok := SourceOf(p)
			if !ok || p.Kind() == KindVideo || p.Kind() == KindAudioOut || p.Kind() == KindImageOut || p.Kind() == KindVideoOut {
				return []Delta{unrepresentable(p.Kind())}
			}
			r.Files = append(r.Files, wireFile{URI: src.URI, MediaType: src.MediaType, Filename: src.Filename, Data: src.Inline})
		}
		return []Delta{r}
	case KindCitation:
		if c, ok := part.(CitationPart); ok {
			return []Delta{CitationDelta{Citation: c.Citation}}
		}
	}
	return nil
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// v1 tool output block kinds.
const (
	v1BlockText  = "text"
	v1BlockImage = "image"
	v1BlockFile  = "file"
	v1BlockJSON  = "json"
)

// partsToBlocks is the v1 wire form of tool output parts.
func partsToBlocks(parts []ToolOutputPart) []wireBlock {
	if parts == nil {
		return nil
	}
	out := make([]wireBlock, 0, len(parts))
	for _, p := range parts {
		switch v := p.(type) {
		case TextPart:
			out = append(out, wireBlock{Kind: v1BlockText, Text: v.Text})
		case JSONPart:
			out = append(out, wireBlock{Kind: v1BlockJSON, JSON: v.JSON})
		case ImagePart:
			out = append(out, wireBlock{Kind: v1BlockImage, MediaType: v.Source.MediaType,
				URI: v.Source.URI, Filename: v.Source.Filename, Data: v.Source.Inline})
		default:
			if src, ok := SourceOf(p); ok {
				out = append(out, wireBlock{Kind: v1BlockFile, MediaType: src.MediaType,
					URI: src.URI, Filename: src.Filename, Data: src.Inline})
			}
		}
	}
	return out
}

// blocksToParts upgrades v1 tool output blocks.
func blocksToParts(bs []wireBlock) []ToolOutputPart {
	if bs == nil {
		return nil
	}
	out := make([]ToolOutputPart, len(bs))
	for i, b := range bs {
		out[i] = b.part()
	}
	return out
}

// source returns the media source a v1 file names.
func (f wireFile) source() Source {
	s := Source{URI: f.URI, MediaType: f.MediaType, Filename: f.Filename}
	if len(f.Data) > 0 {
		s = Bytes(f.MediaType, f.Data).With(s)
	}
	return s
}

// part returns the tool output part for a v1 block. A file block becomes a
// document, audio or opaque file part by its media type.
func (b wireBlock) part() ToolOutputPart {
	switch b.Kind {
	case v1BlockText:
		return Text(b.Text)
	case v1BlockJSON:
		return JSONPart{JSON: b.JSON}
	}
	src := wireFile{URI: b.URI, MediaType: b.MediaType, Filename: b.Filename, Data: b.Data}.source()
	if b.Kind == v1BlockImage {
		return Image(src)
	}
	switch src.MediaType.Modality() {
	case ModalityImage:
		return Image(src)
	case ModalityAudio:
		return Audio(src)
	case ModalityDocument:
		return Document(src)
	default:
		return File(src)
	}
}
