package types

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Part codec errors.
var (
	// ErrUnknownPartKind reports a part whose type tag this release does
	// not know.
	ErrUnknownPartKind = errors.New("unknown part kind")
	// ErrPartRole reports a part decoded for a role whose seal it lacks,
	// such as a tool call in a user message.
	ErrPartRole = errors.New("part not allowed here")
)

// codecOptions selects the wire form, which carries inline media bytes up
// to a limit, over the persisted form, which never does.
type codecOptions struct {
	inline    bool
	maxInline int // bytes per source; <0 means no limit
}

// MarshalPart encodes p in its persisted JSON form: the part's fields plus a
// "type" tag. Inline media bytes are never written; the other locators and
// the digest are.
//
//	{"type":"image","source":{"media_type":"image/png","sha256":"9f…","ref":"saige-artifact://9f…"},"image":{"width":1024}}
func MarshalPart(p Part) ([]byte, error) {
	return marshalPart(p, codecOptions{})
}

// MarshalPartInline encodes p in its wire form: MarshalPart's form plus the
// inline media bytes, base64 encoded in each source's "data" field. Use it
// where the bytes must travel, such as a cache entry or a live stream, never
// for a conversation store.
func MarshalPartInline(p Part) ([]byte, error) {
	return marshalPart(p, codecOptions{inline: true, maxInline: -1})
}

// UnmarshalPart decodes a part written by MarshalPart or by the wire codec.
// It is strict on the type tag and lenient on fields it does not know, so a
// newer writer's additions are ignored (D-35). Tool-call arguments and other
// free-form maps decode numbers as json.Number.
func UnmarshalPart(b []byte) (Part, error) {
	var tag struct {
		Type PartKind `json:"type"`
	}
	if err := json.Unmarshal(b, &tag); err != nil {
		return nil, fmt.Errorf("decode part: %w", err)
	}
	return decodePartAs(tag.Type, b)
}

// UnmarshalRolePart decodes a part and checks that it has type T, such as
// UserPart or ToolOutputPart.
func UnmarshalRolePart[T Part](b []byte) (T, error) {
	var zero T
	p, err := UnmarshalPart(b)
	if err != nil {
		return zero, err
	}
	v, ok := p.(T)
	if !ok {
		return zero, fmt.Errorf("%w: %s as %T", ErrPartRole, p.Kind(), (*T)(nil))
	}
	return v, nil
}

func marshalPart(p Part, o codecOptions) ([]byte, error) {
	if p == nil {
		return nil, fmt.Errorf("%w: nil part", ErrUnknownPartKind)
	}
	var body []byte
	var err error
	switch v := p.(type) {
	case ToolResultPart:
		body, err = v.marshal(o)
	case ServerToolResultPart:
		body, err = v.marshal(o)
	default:
		if !knownKind(p.Kind()) {
			return nil, fmt.Errorf("%w: %T", ErrUnknownPartKind, p)
		}
		body, err = json.Marshal(p)
	}
	if err != nil {
		return nil, fmt.Errorf("encode %s part: %w", p.Kind(), err)
	}
	if o.inline {
		if body, err = withInline(p, body, o); err != nil {
			return nil, err
		}
	}
	return withType(p.Kind(), body)
}

// withType prepends the type tag to an encoded JSON object.
func withType(k PartKind, body []byte) ([]byte, error) {
	body = bytes.TrimSpace(body)
	if len(body) < 2 || body[0] != '{' {
		return nil, fmt.Errorf("encode %s part: not an object", k)
	}
	tag, _ := json.Marshal(k)
	var b bytes.Buffer
	b.WriteString(`{"type":`)
	b.Write(tag)
	if rest := bytes.TrimSpace(body[1:]); len(rest) > 0 && rest[0] != '}' {
		b.WriteByte(',')
	}
	b.Write(body[1:])
	return b.Bytes(), nil
}

// withInline replaces the source of a media part with its wire form, which
// carries the bytes.
func withInline(p Part, body []byte, o codecOptions) ([]byte, error) {
	src, ok := SourceOf(p)
	if !ok || len(src.Inline) == 0 {
		return body, nil
	}
	if o.maxInline >= 0 && len(src.Inline) > o.maxInline {
		return nil, fmt.Errorf("%w: %s part holds %d bytes, limit %d", ErrWireInlineTooLarge, p.Kind(), len(src.Inline), o.maxInline)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	raw, err := src.wireJSON()
	if err != nil {
		return nil, err
	}
	fields["source"] = raw
	return json.Marshal(fields)
}

func knownKind(k PartKind) bool {
	switch k {
	case KindText, KindImage, KindAudio, KindVideo, KindDocument, KindFile, KindToolResult, KindJSON,
		KindThinking, KindToolCall, KindServerToolCall, KindServerToolResult, KindCitation,
		KindAudioOut, KindImageOut, KindVideoOut, KindRefusal,
		KindConfig, KindRoute, KindSteer, KindTruncation, KindApproval, KindHandoff, KindFeedback,
		KindCompaction, KindGuardrail:
		return true
	}
	return false
}

//nolint:gocyclo // one case per part kind
func decodePartAs(k PartKind, b []byte) (Part, error) {
	switch k {
	case KindText:
		return decodePart[TextPart](k, b)
	case KindJSON:
		return decodePart[JSONPart](k, b)
	case KindImage:
		return decodePart[ImagePart](k, b)
	case KindAudio:
		return decodePart[AudioPart](k, b)
	case KindVideo:
		return decodePart[VideoPart](k, b)
	case KindDocument:
		return decodePart[DocumentPart](k, b)
	case KindFile:
		return decodePart[FilePart](k, b)
	case KindToolResult:
		return decodePart[ToolResultPart](k, b)
	case KindThinking:
		return decodePart[ThinkingPart](k, b)
	case KindToolCall:
		return decodePart[ToolCallPart](k, b)
	case KindServerToolCall:
		return decodePart[ServerToolCallPart](k, b)
	case KindServerToolResult:
		return decodePart[ServerToolResultPart](k, b)
	case KindCitation:
		return decodePart[CitationPart](k, b)
	case KindAudioOut:
		return decodePart[AudioOutPart](k, b)
	case KindImageOut:
		return decodePart[ImageOutPart](k, b)
	case KindVideoOut:
		return decodePart[VideoOutPart](k, b)
	case KindRefusal:
		return decodePart[RefusalPart](k, b)
	case KindConfig:
		return decodePart[ConfigPart](k, b)
	case KindRoute:
		return decodePart[RoutePart](k, b)
	case KindSteer:
		return decodePart[SteerPart](k, b)
	case KindTruncation:
		return decodePart[TruncationPart](k, b)
	case KindApproval:
		return decodePart[ApprovalPart](k, b)
	case KindHandoff:
		return decodePart[HandoffPart](k, b)
	case KindFeedback:
		return decodePart[FeedbackPart](k, b)
	case KindCompaction:
		return decodePart[CompactionPart](k, b)
	case KindGuardrail:
		return decodePart[GuardrailPart](k, b)
	case "":
		return nil, fmt.Errorf("%w: missing type", ErrUnknownPartKind)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownPartKind, k)
	}
}

func decodePart[T Part](k PartKind, b []byte) (Part, error) {
	var v T
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("decode %s part: %w", k, err)
	}
	return v, nil
}

// ── Nested parts ─────────────────────────────────────────────────────

type toolResultJSON struct {
	CallID      string            `json:"call_id"`
	Parts       []json.RawMessage `json:"parts"`
	IsError     bool              `json:"is_error,omitempty"`
	Citations   []Citation        `json:"citations,omitempty"`
	ToolVersion string            `json:"tool_version,omitempty"`
}

func (r ToolResultPart) marshal(o codecOptions) ([]byte, error) {
	w := toolResultJSON{CallID: r.CallID, IsError: r.IsError, Citations: r.Citations, ToolVersion: r.ToolVersion}
	if r.Parts != nil {
		w.Parts = make([]json.RawMessage, 0, len(r.Parts))
	}
	for _, p := range r.Parts {
		raw, err := marshalPart(p, o)
		if err != nil {
			return nil, err
		}
		w.Parts = append(w.Parts, raw)
	}
	return json.Marshal(w)
}

// MarshalJSON writes the persisted form, with each nested part tagged.
func (r ToolResultPart) MarshalJSON() ([]byte, error) { return r.marshal(codecOptions{}) }

// UnmarshalJSON reads the form MarshalJSON writes.
func (r *ToolResultPart) UnmarshalJSON(b []byte) error {
	var w toolResultJSON
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	out := ToolResultPart{CallID: w.CallID, IsError: w.IsError, Citations: w.Citations, ToolVersion: w.ToolVersion}
	if w.Parts != nil {
		out.Parts = make([]ToolOutputPart, 0, len(w.Parts))
	}
	for _, raw := range w.Parts {
		p, err := UnmarshalRolePart[ToolOutputPart](raw)
		if err != nil {
			return err
		}
		out.Parts = append(out.Parts, p)
	}
	*r = out
	return nil
}

type serverToolResultJSON struct {
	CallID   string            `json:"call_id"`
	ToolKind ServerToolKind    `json:"tool_kind"`
	Text     string            `json:"text,omitempty"`
	Result   json.RawMessage   `json:"result,omitempty"`
	IsError  bool              `json:"is_error,omitempty"`
	Outputs  []json.RawMessage `json:"outputs,omitempty"`
}

func (r ServerToolResultPart) marshal(o codecOptions) ([]byte, error) {
	w := serverToolResultJSON{CallID: r.CallID, ToolKind: r.ToolKind, Text: r.Text, Result: r.Result, IsError: r.IsError}
	for _, p := range r.Outputs {
		raw, err := marshalPart(p, o)
		if err != nil {
			return nil, err
		}
		w.Outputs = append(w.Outputs, raw)
	}
	return json.Marshal(w)
}

// MarshalJSON writes the persisted form, with each output part tagged.
func (r ServerToolResultPart) MarshalJSON() ([]byte, error) { return r.marshal(codecOptions{}) }

// UnmarshalJSON reads the form MarshalJSON writes.
func (r *ServerToolResultPart) UnmarshalJSON(b []byte) error {
	var w serverToolResultJSON
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&w); err != nil {
		return err
	}
	out := ServerToolResultPart{CallID: w.CallID, ToolKind: w.ToolKind, Text: w.Text, Result: w.Result, IsError: w.IsError}
	for _, raw := range w.Outputs {
		p, err := UnmarshalPart(raw)
		if err != nil {
			return err
		}
		out.Outputs = append(out.Outputs, p)
	}
	*r = out
	return nil
}
