// Package durablecodec encodes the values durable engines record: step
// results, run input and final messages.
//
// Records are gob, but messages and parts inside them are stored through
// the shared JSON part codec (types.MarshalPartInline, which keeps media
// bytes so a replayed step sends what the live one did). gob names an
// interface value by its Go type, so storing parts as gob would tie every
// journal to the names of today's types; JSON parts name themselves.
//
// Every record carries a version. Records an earlier release wrote, before
// typed parts, are read through legacy.go and upgraded, so a run that was in
// flight across an upgrade replays.
package durablecodec

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/urmzd/saige/agent/types"
)

// Version is the format of the records Encode writes.
const Version = types.StepFormatVersion

func init() {
	// Tool-call Arguments are map[string]any decoded from JSON; nested arrays and
	// objects arrive as []interface{} / map[string]interface{} inside interface
	// values and must be registered or gob.Encode fails on real tool schemas.
	gob.Register([]interface{}{})
	gob.Register(map[string]interface{}{})
	gob.Register(json.Number(""))
}

// Encode returns the record of v. A types.StepResult, a []types.Message
// and a types.AssistantMessage are stored in the current record format;
// any other value is plain gob.
func Encode(v any) ([]byte, error) {
	switch x := v.(type) {
	case types.StepResult:
		rec, err := newStepRecord(x)
		if err != nil {
			return nil, err
		}
		return gobEncode(rec)
	case *types.StepResult:
		return Encode(*x)
	case []types.Message:
		rec, err := newMessagesRecord(x)
		if err != nil {
			return nil, err
		}
		return gobEncode(rec)
	case types.AssistantMessage:
		rec, err := NewAssistant(x)
		if err != nil {
			return nil, err
		}
		return gobEncode(rec)
	case *types.AssistantMessage:
		return Encode(*x)
	default:
		return gobEncode(v)
	}
}

// Decode reads a record Encode wrote, or one an earlier release wrote, into
// v. v is a pointer, as for gob.
func Decode(raw []byte, v any) error {
	switch x := v.(type) {
	case *types.StepResult:
		var rec stepRecord
		if err := gobDecode(raw, &rec); err != nil {
			return err
		}
		out, err := rec.result()
		if err != nil {
			return err
		}
		*x = out
		return nil
	case *[]types.Message:
		out, err := decodeMessages(raw)
		if err != nil {
			return err
		}
		*x = out
		return nil
	case *types.AssistantMessage:
		var rec Assistant
		if err := gobDecode(raw, &rec); err != nil {
			return err
		}
		out, err := rec.Message()
		if err != nil {
			return err
		}
		*x = out
		return nil
	default:
		return gobDecode(raw, v)
	}
}

func gobEncode(v any) ([]byte, error) {
	var b bytes.Buffer
	err := gob.NewEncoder(&b).Encode(v)
	return b.Bytes(), err
}

func gobDecode(raw []byte, v any) error { return gob.NewDecoder(bytes.NewReader(raw)).Decode(v) }

// ── Parts ────────────────────────────────────────────────────────────

// encodeParts stores parts as a JSON array, with their media bytes. A nil
// slice stores nothing.
func encodeParts[P types.Part](parts []P) ([]byte, error) {
	if parts == nil {
		return nil, nil
	}
	raw := make([]json.RawMessage, len(parts))
	for i, p := range parts {
		b, err := types.MarshalPartInline(p)
		if err != nil {
			return nil, err
		}
		raw[i] = b
	}
	return json.Marshal(raw)
}

// decodeParts reads what encodeParts wrote. No parts is a nil slice, as
// gob reads an empty one.
// Numbers in tool arguments read as float64, as a provider's stream
// decodes them, so a replayed call carries the arguments the live one did.
func decodeParts[T types.Part](b []byte) ([]T, error) {
	if b == nil {
		return nil, nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	var out []T
	for _, r := range raw {
		p, err := types.UnmarshalRolePart[T](r)
		if err != nil {
			return nil, err
		}
		out = append(out, liveNumbers(p).(T))
	}
	return out, nil
}

func liveNumbers(p types.Part) types.Part {
	switch v := p.(type) {
	case types.ToolCallPart:
		if v.Arguments != nil {
			v.Arguments = floatNumbers(v.Arguments).(map[string]any)
		}
		return v
	case types.ServerToolCallPart:
		if v.Input != nil {
			v.Input = floatNumbers(v.Input).(map[string]any)
		}
		return v
	}
	return p
}

// floatNumbers replaces json.Number values in v with float64.
func floatNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return f
		}
		return x
	case map[string]any:
		for k, e := range x {
			x[k] = floatNumbers(e)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = floatNumbers(e)
		}
		return x
	}
	return v
}

// ── Messages ─────────────────────────────────────────────────────────

// ErrVersion reports a record newer than this release reads.
var ErrVersion = errors.New("durablecodec: unsupported record version")

// Assistant is the record of an assistant message inside another record,
// such as a recorded batch result. It reads both the current form and the
// one earlier releases wrote (an AssistantMessage with Content).
type Assistant struct {
	V     int
	Parts []byte
	// Content is the form an earlier release wrote.
	Content []legacyContent
}

// NewAssistant returns the record of m.
func NewAssistant(m types.AssistantMessage) (Assistant, error) {
	parts, err := encodeParts(m.Parts)
	if err != nil {
		return Assistant{}, err
	}
	return Assistant{V: Version, Parts: parts}, nil
}

// Message returns the message a record holds.
func (a Assistant) Message() (types.AssistantMessage, error) {
	switch a.V {
	case 0:
		parts, err := legacyRoleParts[types.AssistantPart](types.RoleAssistant, a.Content)
		return types.AssistantMessage{Parts: parts}, err
	case Version:
		parts, err := decodeParts[types.AssistantPart](a.Parts)
		return types.AssistantMessage{Parts: parts}, err
	default:
		return types.AssistantMessage{}, fmt.Errorf("%w: %d", ErrVersion, a.V)
	}
}

// messagesRecord is the record of a run's input.
type messagesRecord struct {
	V        int
	Messages []messageRecord
}

type messageRecord struct {
	Role  types.Role
	Parts []byte
}

func newMessagesRecord(msgs []types.Message) (messagesRecord, error) {
	rec := messagesRecord{V: Version, Messages: make([]messageRecord, len(msgs))}
	for i, m := range msgs {
		if m == nil {
			return rec, fmt.Errorf("durablecodec: message %d is nil", i)
		}
		parts, err := encodeParts(types.PartsOf(m))
		if err != nil {
			return rec, err
		}
		rec.Messages[i] = messageRecord{Role: m.Role(), Parts: parts}
	}
	return rec, nil
}

// decodeMessages reads a run's input, in the current record form or as
// the bare message list an earlier release wrote.
func decodeMessages(raw []byte) ([]types.Message, error) {
	var rec messagesRecord
	if err := gobDecode(raw, &rec); err != nil {
		msgs, legacyErr := decodeLegacyMessages(raw)
		if legacyErr != nil {
			return nil, errors.Join(err, legacyErr)
		}
		return msgs, nil
	}
	if rec.V != Version {
		return nil, fmt.Errorf("%w: %d", ErrVersion, rec.V)
	}
	var out []types.Message
	for i, m := range rec.Messages {
		msg, err := decodeMessage(m.Role, m.Parts)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", i, err)
		}
		out = append(out, msg)
	}
	return out, nil
}

func decodeMessage(role types.Role, b []byte) (types.Message, error) {
	switch role {
	case types.RoleSystem:
		parts, err := decodeParts[types.SystemPart](b)
		return types.SystemMessage{Parts: parts}, err
	case types.RoleUser:
		parts, err := decodeParts[types.UserPart](b)
		return types.UserMessage{Parts: parts}, err
	case types.RoleAssistant:
		parts, err := decodeParts[types.AssistantPart](b)
		return types.AssistantMessage{Parts: parts}, err
	default:
		return nil, fmt.Errorf("durablecodec: unknown role %q", role)
	}
}

// ── Steps ────────────────────────────────────────────────────────────

// stepRecord is the record of a types.StepResult. Fields keep the
// StepResult names, so records an earlier release wrote decode into the
// same struct: Message and ToolBlocks are their form, and MessageParts and
// ToolOutput hold the message and the tool output now.
type stepRecord struct {
	V                  int
	Receipt            *types.BudgetReceipt
	Usage              *types.UsageDelta
	Kind               types.StepKind
	MessageParts       []byte
	ToolCallID         string
	ToolResult         string
	ToolOutput         []byte
	ToolCitations      []types.Citation
	ToolError          string
	Approval           *types.ApprovalVerdict
	Hook               *hookRecord
	Conversion         *conversionRecord
	ConversionReceipts []types.BudgetReceipt

	// The form an earlier release wrote.
	Message    *legacyAssistantMessage
	ToolBlocks []legacyBlock
}

// hookRecord is the record of a types.HookRecord. MessageParts holds the
// changed message; Message is the form an earlier release wrote.
type hookRecord struct {
	Abort        bool
	Name         string
	Reason       string
	Changed      bool
	MessageParts []byte
	Arguments    map[string]any
	Text         string
	Error        string
	Skip         bool
	Action       string
	Canceled     bool
	Usage        []types.UsageDelta
	Receipts     []types.BudgetReceipt

	Message *legacyUserMessage
}

// conversionRecord is the record of a types.ConversionEntry.
type conversionRecord struct {
	Parts []byte
	Via   string
}

// present stores a message's parts so that a message with none still
// reads back as a message.
func present[P types.Part](parts []P) ([]byte, error) {
	b, err := encodeParts(parts)
	if b == nil && err == nil {
		b = []byte("[]")
	}
	return b, err
}

func newStepRecord(s types.StepResult) (stepRecord, error) {
	rec := stepRecord{V: Version, Receipt: s.Receipt, Usage: s.Usage, Kind: s.Kind, ToolCallID: s.ToolCallID,
		ToolResult: s.ToolResult, ToolCitations: s.ToolCitations, ToolError: s.ToolError, Approval: s.Approval,
		ConversionReceipts: s.ConversionReceipts}
	var err error
	if s.Message != nil {
		if rec.MessageParts, err = present(s.Message.Parts); err != nil {
			return rec, err
		}
	}
	if rec.ToolOutput, err = encodeParts(s.ToolParts); err != nil {
		return rec, err
	}
	if h := s.Hook; h != nil {
		hr := &hookRecord{Abort: h.Abort, Name: h.Name, Reason: h.Reason, Changed: h.Changed, Arguments: h.Arguments, Text: h.Text,
			Error: h.Error, Skip: h.Skip, Action: h.Action, Canceled: h.Canceled, Usage: h.Usage, Receipts: h.Receipts}
		if h.Message != nil {
			if hr.MessageParts, err = present(h.Message.Parts); err != nil {
				return rec, err
			}
		}
		rec.Hook = hr
	}
	if c := s.Conversion; c != nil {
		parts, err := encodeParts(c.Parts)
		if err != nil {
			return rec, err
		}
		rec.Conversion = &conversionRecord{Parts: parts, Via: c.Via}
	}
	return rec, nil
}

// result returns the step a record holds. A record an earlier release
// wrote is upgraded and reports version 1.
func (r stepRecord) result() (types.StepResult, error) {
	out := types.StepResult{V: r.V, Receipt: r.Receipt, Usage: r.Usage, Kind: r.Kind, ToolCallID: r.ToolCallID,
		ToolResult: r.ToolResult, ToolCitations: r.ToolCitations, ToolError: r.ToolError, Approval: r.Approval,
		ConversionReceipts: r.ConversionReceipts}
	switch r.V {
	case 0:
		return r.legacyResult(out)
	case Version:
	default:
		return out, fmt.Errorf("%w: %d", ErrVersion, r.V)
	}
	if r.MessageParts != nil {
		parts, err := decodeParts[types.AssistantPart](r.MessageParts)
		if err != nil {
			return out, err
		}
		out.Message = &types.AssistantMessage{Parts: parts}
	}
	var err error
	if out.ToolParts, err = decodeParts[types.ToolOutputPart](r.ToolOutput); err != nil {
		return out, err
	}
	if h := r.Hook; h != nil {
		hr := h.common()
		if h.MessageParts != nil {
			parts, err := decodeParts[types.UserPart](h.MessageParts)
			if err != nil {
				return out, err
			}
			hr.Message = &types.UserMessage{Parts: parts}
		}
		out.Hook = hr
	}
	if c := r.Conversion; c != nil {
		parts, err := decodeParts[types.Part](c.Parts)
		if err != nil {
			return out, err
		}
		out.Conversion = &types.ConversionEntry{Parts: parts, Via: c.Via}
	}
	return out, nil
}

// common returns the hook record's fields that need no conversion.
func (h *hookRecord) common() *types.HookRecord {
	return &types.HookRecord{Abort: h.Abort, Name: h.Name, Reason: h.Reason, Changed: h.Changed, Arguments: h.Arguments,
		Text: h.Text, Error: h.Error, Skip: h.Skip, Action: h.Action, Canceled: h.Canceled, Usage: h.Usage, Receipts: h.Receipts}
}
