package types

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/urmzd/saige/agent/selector/rank"
)

// WireVersion is the envelope version this package writes. Readers accept
// every version up to it and reject a higher one instead of guessing at its
// meaning. Version 2 streams model output as part deltas; version 1 kinds
// stay decodable (see Decoder) and writable (see EncodeOptions).
const WireVersion = 2

// DefaultMaxInlineBytes is the default limit on inline media bytes in one
// envelope field.
const DefaultMaxInlineBytes = 256 << 10

// Wire codec errors.
var (
	ErrWireVersion     = errors.New("unsupported wire version")
	ErrUnknownWireKind = errors.New("unknown wire kind")
	// ErrWireInlineTooLarge reports inline media bytes over the encoder's
	// limit. The producer must store the bytes and send a reference first;
	// the encoder never truncates them.
	ErrWireInlineTooLarge = errors.New("inline media too large for the wire")
)

// Envelope is the versioned JSON frame for one streamed event: a Delta, an
// Interrupt, or an InterruptReply. Kind discriminates Data. The producer owns
// Seq (monotonic per stream, so a client can resume after a gap), RunID, and
// Path (tool call IDs from the root run to the run that emitted the event).
//
//	{"v":2,"seq":42,"run_id":"r1","path":["call_9"],"kind":"part.delta","data":{"index":0,"text":"hi"}}
type Envelope struct {
	V     int             `json:"v"`
	Seq   uint64          `json:"seq,omitempty"`
	RunID string          `json:"run_id,omitempty"`
	Path  []string        `json:"path,omitempty"`
	Kind  string          `json:"kind"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// Wire kinds. They are part of the wire contract: never rename one.
const (
	// Version 2 model output.
	WirePartStart  = "part.start"
	WirePartDelta  = "part.delta"
	WirePartEnd    = "part.end"
	WireConversion = "conversion"

	// Version 1 model output, decodable through Decoder.
	WireTextStart          = "text.start"
	WireTextDelta          = "text.delta"
	WireTextEnd            = "text.end"
	WireReasoningStart     = "reasoning.start"
	WireReasoningDelta     = "reasoning.delta"
	WireReasoningEnd       = "reasoning.end"
	WireToolCallStart      = "tool.call.start"
	WireToolCallArgs       = "tool.call.args"
	WireToolCallEnd        = "tool.call.end"
	WireToolExecStart      = "tool.exec.start"
	WireToolExecDelta      = "tool.exec.delta"
	WireToolExecEnd        = "tool.exec.end"
	WireMarker             = "marker"
	WireHandoff            = "handoff"
	WireCitation           = "citation"
	WireError              = "error"
	WireDone               = "done"
	WireFeedback           = "feedback"
	WireUsage              = "usage"
	WireRoute              = "route"
	WireTruncated          = "truncated"
	WireQueued             = "queued"
	WireInjected           = "injected"
	WireInterrupted        = "interrupted"
	WireServerToolCall     = "server_tool.call"
	WireServerToolResult   = "server_tool.result"
	WirePartialJSON        = "partial_json"
	WireCompaction         = "compaction"
	WireInterrupt          = "interrupt"
	WireInterruptReplyKind = "interrupt.reply"
	WireGuardrail          = "guardrail"
)

// ── Public API ───────────────────────────────────────────────────────

// MarshalDelta encodes d as a JSON envelope with only v, kind, and data set.
func MarshalDelta(d Delta) ([]byte, error) {
	env, err := NewDeltaEnvelope(d)
	if err != nil {
		return nil, err
	}
	return json.Marshal(env)
}

// UnmarshalDelta decodes an envelope produced by MarshalDelta.
func UnmarshalDelta(b []byte) (Delta, error) {
	env, err := UnmarshalEnvelope(b)
	if err != nil {
		return nil, err
	}
	return env.Delta()
}

// MarshalInterrupt encodes an Interrupt as a JSON envelope.
func MarshalInterrupt(in Interrupt) ([]byte, error) {
	env, err := NewInterruptEnvelope(in)
	if err != nil {
		return nil, err
	}
	return json.Marshal(env)
}

// UnmarshalInterrupt decodes an envelope produced by MarshalInterrupt.
func UnmarshalInterrupt(b []byte) (Interrupt, error) {
	env, err := UnmarshalEnvelope(b)
	if err != nil {
		return Interrupt{}, err
	}
	return env.Interrupt()
}

// MarshalInterruptReply encodes an InterruptReply as a JSON envelope.
func MarshalInterruptReply(r InterruptReply) ([]byte, error) {
	env, err := NewInterruptReplyEnvelope(r)
	if err != nil {
		return nil, err
	}
	return json.Marshal(env)
}

// UnmarshalInterruptReply decodes an envelope produced by MarshalInterruptReply.
func UnmarshalInterruptReply(b []byte) (InterruptReply, error) {
	env, err := UnmarshalEnvelope(b)
	if err != nil {
		return InterruptReply{}, err
	}
	return env.InterruptReply()
}

// UnmarshalEnvelope decodes the frame and checks its version. Use it when the
// kind is not known in advance, then call Delta, Interrupt, or InterruptReply.
func UnmarshalEnvelope(b []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return Envelope{}, err
	}
	if env.V < 1 || env.V > WireVersion {
		return Envelope{}, fmt.Errorf("%w: %d", ErrWireVersion, env.V)
	}
	return env, nil
}

// IsDelta reports whether the envelope carries a Delta.
func (e Envelope) IsDelta() bool {
	return e.Kind != WireInterrupt && e.Kind != WireInterruptReplyKind
}

// NewDeltaEnvelope builds the current-version envelope for d, with inline
// media bytes up to DefaultMaxInlineBytes. A v1 content delta gets a v1
// envelope. Seq, RunID, and Path are left for the producer to fill.
func NewDeltaEnvelope(d Delta) (Envelope, error) {
	return newDeltaEnvelope(d, encodeOpts{version: WireVersion, codec: codecOptions{inline: true, maxInline: DefaultMaxInlineBytes}})
}

func newDeltaEnvelope(d Delta, o encodeOpts) (Envelope, error) {
	kind, v, data, err := encodeDelta(d, o)
	if err != nil {
		return Envelope{}, err
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return Envelope{}, fmt.Errorf("encode %s: %w", kind, err)
	}
	return Envelope{V: v, Kind: kind, Data: raw}, nil
}

// EncodeOptions configures an Encoder.
type EncodeOptions struct {
	// Version is the wire version to write: 2 (the default) or 1. Version
	// 1 downgrades part deltas to v1 kinds (see NewV1Downgrader).
	Version int
	// MaxInlineBytes caps the inline media bytes one field may carry. Zero
	// means DefaultMaxInlineBytes; a negative value means no limit.
	MaxInlineBytes int
}

type encodeOpts struct {
	version int
	codec   codecOptions
}

// Encoder writes the envelopes for one stream. It keeps state, because a
// v1 downgrade must pair each part's deltas, so use one per stream.
type Encoder struct {
	o    encodeOpts
	down func(Delta) []Delta
}

// NewEncoder returns an encoder for one stream.
func NewEncoder(opts EncodeOptions) (*Encoder, error) {
	v := opts.Version
	if v == 0 {
		v = WireVersion
	}
	if v < 1 || v > WireVersion {
		return nil, fmt.Errorf("%w: %d", ErrWireVersion, v)
	}
	limit := opts.MaxInlineBytes
	if limit == 0 {
		limit = DefaultMaxInlineBytes
	}
	e := &Encoder{o: encodeOpts{version: v, codec: codecOptions{inline: true, maxInline: limit}}}
	if v == 1 {
		e.down = NewV1Downgrader()
	}
	return e, nil
}

// Version is the wire version the encoder writes.
func (e *Encoder) Version() int { return e.o.version }

// Encode returns the envelopes for d: usually one, none for a v2 delta with
// no v1 form, or several when a downgrade splits it.
func (e *Encoder) Encode(d Delta) ([]Envelope, error) {
	ds := []Delta{d}
	if e.down != nil {
		ds = e.down(d)
	}
	out := make([]Envelope, 0, len(ds))
	for _, x := range ds {
		env, err := newDeltaEnvelope(x, e.o)
		if err != nil {
			return nil, err
		}
		out = append(out, env)
	}
	return out, nil
}

// Delta decodes the envelope's payload. A v1 model-output envelope
// decodes to a v1 delta that only NewV1Upgrader understands; use a Decoder
// to read a stream that may be v1.
func (e Envelope) Delta() (Delta, error) {
	return decodeDelta(e.Kind, e.Data)
}

// Decoder reads the envelopes of one stream as part deltas, upgrading v1
// model output on the way. It keeps state, because a v1 stream's indices
// are assigned in order, so use one per stream.
type Decoder struct {
	up func(Delta) []Delta
}

// NewDecoder returns a decoder for one stream.
func NewDecoder() *Decoder { return &Decoder{up: NewV1Upgrader()} }

// Decode returns the deltas env carries: usually one, none for a v1 delta
// that only closes state, or several when an upgrade splits one.
func (d *Decoder) Decode(env Envelope) ([]Delta, error) {
	x, err := env.Delta()
	if err != nil {
		return nil, err
	}
	return d.up(x), nil
}

// NewInterruptEnvelope builds the envelope for an Interrupt. RunID and Path
// are copied from the interrupt.
func NewInterruptEnvelope(in Interrupt) (Envelope, error) {
	raw, err := json.Marshal(toWireInterrupt(in))
	if err != nil {
		return Envelope{}, fmt.Errorf("encode interrupt: %w", err)
	}
	return Envelope{V: WireVersion, RunID: in.RunID, Path: in.Path, Kind: WireInterrupt, Data: raw}, nil
}

// Interrupt decodes an interrupt envelope.
func (e Envelope) Interrupt() (Interrupt, error) {
	if e.Kind != WireInterrupt {
		return Interrupt{}, fmt.Errorf("%w: %q is not %q", ErrUnknownWireKind, e.Kind, WireInterrupt)
	}
	var w wireInterrupt
	if err := decodeStrict(e.Data, &w); err != nil {
		return Interrupt{}, fmt.Errorf("decode interrupt: %w", err)
	}
	return fromWireInterrupt(w), nil
}

func toWireInterrupt(in Interrupt) wireInterrupt {
	return wireInterrupt{
		ID: in.ID, RunID: in.RunID, Path: in.Path, Kind: in.Kind, Payload: in.Payload,
		Markers: toWireMarkers(in.Markers), Policy: in.Policy,
		CreatedAt: timePtr(in.CreatedAt), ExpiresAt: timePtr(in.ExpiresAt),
	}
}

func fromWireInterrupt(w wireInterrupt) Interrupt {
	return Interrupt{
		ID: w.ID, RunID: w.RunID, Path: w.Path, Kind: w.Kind, Payload: w.Payload,
		Markers: fromWireMarkers(w.Markers), Policy: w.Policy,
		CreatedAt: timeVal(w.CreatedAt), ExpiresAt: timeVal(w.ExpiresAt),
	}
}

// NewInterruptReplyEnvelope builds the envelope for an InterruptReply.
func NewInterruptReplyEnvelope(r InterruptReply) (Envelope, error) {
	w := wireInterruptReply{
		ID: r.ID, IdempotencyKey: r.IdempotencyKey, Answer: r.Answer,
		Decision: toWireDecision(r.Decision),
	}
	raw, err := json.Marshal(w)
	if err != nil {
		return Envelope{}, fmt.Errorf("encode interrupt reply: %w", err)
	}
	return Envelope{V: WireVersion, Kind: WireInterruptReplyKind, Data: raw}, nil
}

// InterruptReply decodes an interrupt reply envelope.
func (e Envelope) InterruptReply() (InterruptReply, error) {
	if e.Kind != WireInterruptReplyKind {
		return InterruptReply{}, fmt.Errorf("%w: %q is not %q", ErrUnknownWireKind, e.Kind, WireInterruptReplyKind)
	}
	var w wireInterruptReply
	if err := decodeStrict(e.Data, &w); err != nil {
		return InterruptReply{}, fmt.Errorf("decode interrupt reply: %w", err)
	}
	return InterruptReply{
		ID: w.ID, IdempotencyKey: w.IdempotencyKey, Answer: w.Answer,
		Decision: fromWireDecision(w.Decision),
	}, nil
}

func toWireDecision(d ApprovalDecision) wireDecision {
	return wireDecision(d)
}

func fromWireDecision(w wireDecision) ApprovalDecision {
	return ApprovalDecision(w)
}

// FlattenDelta unwraps nested ToolExecDelta values and returns the tool call
// IDs from the outermost to the innermost wrapper, plus the innermost delta.
// A producer can put the path in Envelope.Path and send the inner delta, so a
// client attributes sub-agent output without decoding recursive frames.
func FlattenDelta(d Delta) ([]string, Delta) {
	var path []string
	for {
		te, ok := d.(ToolExecDelta)
		if !ok {
			return path, d
		}
		path = append(path, te.ToolCallID)
		d = te.Inner
	}
}

// ── Errors on the wire ───────────────────────────────────────────────

// RemoteError is an error decoded from the wire. It keeps the original
// message, the ErrorKind, and the stable codes of the sentinel errors the
// original matched, so errors.Is works across a process boundary.
type RemoteError struct {
	Message string
	Kind    ErrorKind
	Codes   []string // stable sentinel names, see ErrorCode
	Err     error    // decoded cause, e.g. a *ProviderError; may be nil
}

func (e *RemoteError) Error() string { return e.Message }

func (e *RemoteError) Unwrap() error { return e.Err }

func (e *RemoteError) Is(target error) bool {
	for _, c := range e.Codes {
		if s := sentinelForCode(c); s != nil && s == target {
			return true
		}
	}
	return false
}

// ErrorKind implements KindReporter.
func (e *RemoteError) ErrorKind() ErrorKind { return e.Kind }

type wireSentinel struct {
	code string
	err  error
}

// wireSentinels lists the sentinel errors with a stable wire code, in match
// order: this package's, then those other packages register. Codes are part
// of the wire contract: never rename one.
var wireSentinels = []wireSentinel{
	{"stream_canceled", ErrStreamCanceled},
	{"context_canceled", context.Canceled},
	{"deadline_exceeded", context.DeadlineExceeded},
	{"max_iterations", ErrMaxIterations},
	{"tool_not_found", ErrToolNotFound},
	{"provider_failed", ErrProviderFailed},
	{"context_length", ErrContextLength},
	{"content_filtered", ErrContentFiltered},
	{"auth", ErrAuth},
	{"rate_limited", ErrRateLimited},
	{"unavailable", ErrUnavailable},
	{"invalid_request", ErrInvalidRequest},
	{"response_truncated", ErrResponseTruncated},
	{"invalid_model_config", ErrInvalidModelConfig},
	{"unsupported_media_type", ErrUnsupportedMediaType},
	{"resolver_not_found", ErrResolverNotFound},
	{"suspended", ErrSuspended},
	{"budget_exceeded", ErrBudgetExceeded},
	{"budget_admission", ErrBudgetAdmission},
	{"budget_busy", ErrBudgetBusy},
	{"unpriced", ErrUnpriced},
	{"interrupt_not_found", ErrInterruptNotFound},
	{"interrupt_expired", ErrInterruptExpired},
	{"no_interrupt_router", ErrNoInterruptRouter},
	{"hook_aborted", ErrHookAborted},
	{"guardrail_tripped", ErrGuardrailTripped},
	{"media_unavailable", ErrMediaUnavailable},
	{"modality_unsupported", ErrModalityUnsupported},
	{"wire_unrepresentable", ErrWireUnrepresentable},
	{"wire_inline_too_large", ErrWireInlineTooLarge},
	{"invalid_config", ErrInvalidConfig},
	// rank cannot import this package, so its sentinel is listed here.
	{"selector.rank.empty_query", rank.ErrEmptyQuery},
	{"batch_ambiguous", ErrBatchAmbiguous},
	{"batch_not_found", ErrBatchNotFound},
	{"batch_request", ErrBatchRequest},
	{"invalid_channel", ErrInvalidChannel},
	{"invalid_grant", ErrInvalidGrant},
	{"invalid_target", ErrInvalidTarget},
	{"invalid_tool_arguments", ErrInvalidToolArguments},
	{"notifier_closed", ErrNotifierClosed},
	{"options_unsupported", ErrOptionsUnsupported},
	{"part_role", ErrPartRole},
	{"reservation_active", ErrReservationActive},
	{"schema_mismatch", ErrSchemaMismatch},
	{"schema_unsupported", ErrSchemaUnsupported},
	{"split_tool_call", ErrSplitToolCall},
	{"tool_exists", ErrToolExists},
	{"tool_quota_exceeded", ErrToolQuotaExceeded},
	{"unknown_part_kind", ErrUnknownPartKind},
	{"unknown_reservation", ErrUnknownReservation},
	{"unknown_target", ErrUnknownTarget},
	{"unknown_wire_kind", ErrUnknownWireKind},
	{"untrusted_locator", ErrUntrustedLocator},
	{"version_conflict", ErrVersionConflict},
	{"wire_version", ErrWireVersion},
}

var (
	sentinelMu     sync.RWMutex
	sentinelByCode = func() map[string]error {
		m := make(map[string]error, len(wireSentinels))
		for _, s := range wireSentinels {
			m[s.code] = s.err
		}
		return m
	}()
)

// RegisterWireSentinel gives err the stable wire code code, so that after
// an error crosses a process boundary (EncodeError, DecodeError, an error
// envelope, a durable record) errors.Is(decoded, err) still holds. A
// package that defines a sentinel the agent loop can surface registers it
// from init; this package cannot import those packages, so it cannot list
// them itself. Codes are part of the wire contract: never rename one.
//
// It panics when code is empty or taken by another error, or err is nil:
// those are programming errors, found when the program starts.
// Registering the same pair twice is a no-op.
func RegisterWireSentinel(code string, err error) {
	if code == "" || err == nil {
		panic("types: RegisterWireSentinel needs a code and an error")
	}
	sentinelMu.Lock()
	defer sentinelMu.Unlock()
	if prev, ok := sentinelByCode[code]; ok {
		if prev == err {
			return
		}
		panic("types: wire code " + strconv.Quote(code) + " is already registered")
	}
	sentinelByCode[code] = err
	wireSentinels = append(wireSentinels, wireSentinel{code: code, err: err})
}

// WireSentinels returns every registered sentinel by its wire code.
func WireSentinels() map[string]error {
	sentinelMu.RLock()
	defer sentinelMu.RUnlock()
	out := make(map[string]error, len(sentinelByCode))
	for c, e := range sentinelByCode {
		out[c] = e
	}
	return out
}

// sentinelForCode returns the sentinel registered under code, or nil.
func sentinelForCode(code string) error {
	sentinelMu.RLock()
	defer sentinelMu.RUnlock()
	return sentinelByCode[code]
}

// ErrorCodes returns the stable wire codes of every registered sentinel err
// matches, in registration order.
func ErrorCodes(err error) []string {
	if err == nil {
		return nil
	}
	sentinelMu.RLock()
	defer sentinelMu.RUnlock()
	var codes []string
	for _, s := range wireSentinels {
		if errors.Is(err, s.err) {
			codes = append(codes, s.code)
		}
	}
	return codes
}

// EncodedError is the wire form of an error: its message, its ErrorKind,
// the wire codes of every registered sentinel it matches (see
// RegisterWireSentinel), and the provider details of a *ProviderError in its
// chain. It is the data of an error envelope, and what a durable record
// stores for a failed step.
type EncodedError struct {
	Message   string                `json:"message"`
	Kind      string                `json:"kind"`
	Retryable bool                  `json:"retryable"`
	Codes     []string              `json:"codes,omitempty"`
	Provider  *EncodedProviderError `json:"provider,omitempty"`
}

// EncodedProviderError is the wire form of a *ProviderError.
type EncodedProviderError struct {
	Name         string   `json:"name,omitempty"`
	Model        string   `json:"model,omitempty"`
	Kind         string   `json:"kind"`
	Status       int      `json:"status,omitempty"`
	RetryAfterMS *float64 `json:"retry_after_ms,omitempty"`
	Cause        string   `json:"cause"`
}

// EncodeError returns the wire form of err, or nil for a nil error.
func EncodeError(err error) *EncodedError {
	if err == nil {
		return nil
	}
	w := &EncodedError{
		Message:   err.Error(),
		Kind:      KindOf(err).String(),
		Retryable: IsTransient(err),
		Codes:     ErrorCodes(err),
	}
	var pe *ProviderError
	if errors.As(err, &pe) {
		wp := &EncodedProviderError{Name: pe.Provider, Model: pe.Model, Kind: pe.Kind.String(), Status: pe.Code}
		if pe.Err != nil {
			wp.Cause = pe.Err.Error()
		}
		if pe.RetryAfter > 0 {
			wp.RetryAfterMS = durationMS(pe.RetryAfter)
		}
		w.Provider = wp
	}
	return w
}

// DecodeError rebuilds an error from its wire form. The result keeps the
// message and the kind, matches with errors.Is every registered sentinel the
// original matched, and carries a *ProviderError when the original did. A
// nil form decodes to nil.
func DecodeError(w *EncodedError) error {
	if w == nil {
		return nil
	}
	kind := ParseErrorKind(w.Kind)
	if w.Provider == nil {
		return &RemoteError{Message: w.Message, Kind: kind, Codes: w.Codes}
	}
	pk := ParseErrorKind(w.Provider.Kind)
	pe := &ProviderError{
		Provider: w.Provider.Name, Model: w.Provider.Model, Kind: pk, Code: w.Provider.Status,
		Err: &RemoteError{Message: w.Provider.Cause, Kind: pk, Codes: w.Codes},
	}
	if w.Provider.RetryAfterMS != nil {
		pe.RetryAfter = msDuration(*w.Provider.RetryAfterMS)
	}
	if pe.Error() == w.Message {
		return pe
	}
	// The provider error was wrapped; keep the outer message and the cause.
	return &RemoteError{Message: w.Message, Kind: kind, Codes: w.Codes, Err: pe}
}

// ── Payload shapes ───────────────────────────────────────────────────

type wireEmpty struct{}

type wireContent struct {
	Content string `json:"content"`
}

type wireSignature struct {
	Signature string `json:"signature,omitempty"`
}

type wireToolCall struct {
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Content   string         `json:"content,omitempty"`
	Arguments map[string]any `json:"arguments,omitempty"`
	// ArgumentsError carries ToolCallEndDelta.ArgumentsError.
	ArgumentsError string `json:"arguments_error,omitempty"`
}

type wireToolExec struct {
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name,omitempty"`
	Result     string `json:"result,omitempty"`
	Error      string `json:"error,omitempty"`
	// Parts is the v2 form of the output; Blocks the v1 form.
	Parts     []json.RawMessage `json:"parts,omitempty"`
	Blocks    []wireBlock       `json:"blocks,omitempty"`
	Citations []Citation        `json:"citations,omitempty"`
	Inner     *Envelope         `json:"inner,omitempty"`
	Version   string            `json:"version,omitempty"`
}

type wirePartStart struct {
	Index     int       `json:"index"`
	Kind      PartKind  `json:"kind"`
	MediaType MediaType `json:"media_type,omitempty"`
	ID        string    `json:"id,omitempty"`
	Name      string    `json:"name,omitempty"`
}

type wirePartDelta struct {
	Index      int    `json:"index"`
	Text       string `json:"text,omitempty"`
	Thinking   string `json:"thinking,omitempty"`
	Signature  string `json:"signature,omitempty"`
	Args       string `json:"args,omitempty"`
	Refusal    string `json:"refusal,omitempty"`
	Data       []byte `json:"data,omitempty"`
	Transcript string `json:"transcript,omitempty"`
}

type wirePartEnd struct {
	Index int             `json:"index"`
	Part  json.RawMessage `json:"part,omitempty"`
}

type wireConversion struct {
	Profile string           `json:"profile,omitempty"`
	Report  ConversionReport `json:"report"`
}

// wireBlock carries raw bytes too: a live consumer needs the image a tool
// produced, even though tree persistence keeps only the URI.
type wireBlock struct {
	Kind      string          `json:"kind"`
	Text      string          `json:"text,omitempty"`
	MediaType MediaType       `json:"media_type,omitempty"`
	URI       string          `json:"uri,omitempty"`
	Filename  string          `json:"filename,omitempty"`
	Data      []byte          `json:"data,omitempty"`
	JSON      json.RawMessage `json:"json,omitempty"`
}

type wireFile struct {
	URI       string    `json:"uri,omitempty"`
	MediaType MediaType `json:"media_type,omitempty"`
	Filename  string    `json:"filename,omitempty"`
	Data      []byte    `json:"data,omitempty"`
}

type wireMarker struct {
	Kind    string         `json:"kind"`
	Message string         `json:"message,omitempty"`
	Meta    map[string]any `json:"meta,omitempty"`
}

type wireMarkerDelta struct {
	ToolCallID string         `json:"tool_call_id"`
	ToolName   string         `json:"tool_name,omitempty"`
	Arguments  map[string]any `json:"arguments,omitempty"`
	Markers    []wireMarker   `json:"markers,omitempty"`
	// Interrupt carries MarkerDelta.Interrupt, so a remote client can reply
	// by interrupt ID and see the deadline.
	Interrupt *wireInterrupt `json:"interrupt,omitempty"`
}

type wireHandoff struct {
	From   string `json:"from,omitempty"`
	To     string `json:"to"`
	Reason string `json:"reason,omitempty"`
}

type wireCitation struct {
	Citation   Citation `json:"citation"`
	ToolCallID string   `json:"tool_call_id,omitempty"`
}

type wireFeedback struct {
	TargetNodeID string `json:"target_node_id"`
	Rating       Rating `json:"rating"`
	Comment      string `json:"comment,omitempty"`
}

type wireUsage struct {
	AccountingID       string   `json:"accounting_id,omitempty"`
	Cumulative         bool     `json:"cumulative,omitempty"`
	PromptTokens       int      `json:"prompt_tokens"`
	CachedPromptTokens int      `json:"cached_prompt_tokens,omitempty"`
	CacheWriteTokens   int      `json:"cache_write_tokens,omitempty"`
	CompletionTokens   int      `json:"completion_tokens"`
	TotalTokens        int      `json:"total_tokens"`
	LatencyMS          *float64 `json:"latency_ms,omitempty"`
	ResponseModel      string   `json:"response_model,omitempty"`
	ResponseID         string   `json:"response_id,omitempty"`
	FinishReasons      []string `json:"finish_reasons,omitempty"`
	CacheHit           bool     `json:"cache_hit,omitempty"`
	// PromptByModality and CompletionByModality are absent from streams
	// written before they existed, which decode with nil maps.
	PromptByModality     map[Modality]int `json:"prompt_by_modality,omitempty"`
	CompletionByModality map[Modality]int `json:"completion_by_modality,omitempty"`
	Requests             int              `json:"requests,omitempty"`
}

type wireRoute struct {
	Profile         string            `json:"profile,omitempty"`
	Provider        string            `json:"provider,omitempty"`
	Model           string            `json:"model,omitempty"`
	Experiment      string            `json:"experiment,omitempty"`
	Variant         string            `json:"variant,omitempty"`
	Reason          string            `json:"reason,omitempty"`
	Preset          string            `json:"preset,omitempty"`
	ConfigHash      string            `json:"config_hash,omitempty"`
	CatalogRevision string            `json:"catalog_revision,omitempty"`
	Options         *wireOptions      `json:"options,omitempty"`
	Dials           *DialReport       `json:"dials,omitempty"`
	Conversions     *ConversionReport `json:"conversions,omitempty"`
}

func toWireRoute(r RouteDelta) wireRoute {
	return wireRoute{Profile: r.Profile, Provider: r.Provider, Model: r.Model, Experiment: r.Experiment,
		Variant: r.Variant, Reason: r.Reason, Preset: r.Preset, ConfigHash: r.ConfigHash,
		CatalogRevision: r.CatalogRevision, Options: toWireOptions(r.Options), Dials: r.Dials,
		Conversions: r.Conversions}
}

func (w wireRoute) delta() RouteDelta {
	return RouteDelta{Profile: w.Profile, Provider: w.Provider, Model: w.Model, Experiment: w.Experiment,
		Variant: w.Variant, Reason: w.Reason, Preset: w.Preset, ConfigHash: w.ConfigHash,
		CatalogRevision: w.CatalogRevision, Options: w.Options.requestOptions(), Dials: w.Dials,
		Conversions: w.Conversions}
}

// wireOptions is the snake_case wire form of RequestOptions.
type wireOptions struct {
	Temperature      *float64    `json:"temperature,omitempty"`
	TopP             *float64    `json:"top_p,omitempty"`
	TopK             *float64    `json:"top_k,omitempty"`
	FrequencyPenalty *float64    `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64    `json:"presence_penalty,omitempty"`
	Seed             *int64      `json:"seed,omitempty"`
	MaxOutputTokens  *int64      `json:"max_output_tokens,omitempty"`
	StopSequences    []string    `json:"stop,omitempty"`
	ParallelTools    *bool       `json:"parallel_tools,omitempty"`
	ReasoningEnabled *bool       `json:"reasoning_enabled,omitempty"`
	ReasoningEffort  *string     `json:"reasoning_effort,omitempty"`
	ReasoningBudget  *int64      `json:"reasoning_budget,omitempty"`
	ToolChoice       *ToolChoice `json:"tool_choice,omitempty"`
}

func toWireOptions(o *RequestOptions) *wireOptions {
	if o == nil {
		return nil
	}
	c := o.Clone()
	return &wireOptions{Temperature: c.Temperature, TopP: c.TopP, TopK: c.TopK,
		FrequencyPenalty: c.FrequencyPenalty, PresencePenalty: c.PresencePenalty, Seed: c.Seed,
		MaxOutputTokens: c.MaxOutputTokens, StopSequences: c.StopSequences, ParallelTools: c.ParallelTools,
		ReasoningEnabled: c.ReasoningEnabled, ReasoningEffort: c.ReasoningEffort, ReasoningBudget: c.ReasoningBudget,
		ToolChoice: c.ToolChoice}
}

func (w *wireOptions) requestOptions() *RequestOptions {
	if w == nil {
		return nil
	}
	return &RequestOptions{Temperature: w.Temperature, TopP: w.TopP, TopK: w.TopK,
		FrequencyPenalty: w.FrequencyPenalty, PresencePenalty: w.PresencePenalty, Seed: w.Seed,
		MaxOutputTokens: w.MaxOutputTokens, StopSequences: w.StopSequences, ParallelTools: w.ParallelTools,
		ReasoningEnabled: w.ReasoningEnabled, ReasoningEffort: w.ReasoningEffort, ReasoningBudget: w.ReasoningBudget,
		ToolChoice: w.ToolChoice}
}

type wireCompaction struct {
	Branch string `json:"branch"`
	NodeID string `json:"node_id,omitempty"`
	CompactionPart
}

type wireRunControl struct {
	NodeID       string `json:"node_id,omitempty"`
	Reason       string `json:"reason,omitempty"`
	SubmissionID string `json:"submission_id,omitempty"`
	Mode         string `json:"mode,omitempty"`
	Position     int    `json:"position,omitempty"`
}

type wireServerTool struct {
	ID      string          `json:"id"`
	Kind    ServerToolKind  `json:"kind"`
	Name    string          `json:"name,omitempty"`
	Input   map[string]any  `json:"input,omitempty"`
	Text    string          `json:"text,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
	Files   []wireFile      `json:"files,omitempty"`
}

type wirePartialJSON struct {
	JSON json.RawMessage `json:"json"`
}

type wireInterrupt struct {
	ID        string          `json:"id"`
	RunID     string          `json:"run_id,omitempty"`
	Path      []string        `json:"path,omitempty"`
	Kind      InterruptKind   `json:"kind"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	Markers   []wireMarker    `json:"markers,omitempty"`
	CreatedAt *time.Time      `json:"created_at,omitempty"`
	ExpiresAt *time.Time      `json:"expires_at,omitempty"`
	Policy    InterruptPolicy `json:"policy"`
}

type wireDecision struct {
	Approved     bool           `json:"approved"`
	ModifiedArgs map[string]any `json:"modified_args,omitempty"`
	Message      string         `json:"message,omitempty"`
	Approver     string         `json:"approver,omitempty"`
	Grant        *GrantRequest  `json:"grant,omitempty"`
}

type wireInterruptReply struct {
	ID             string          `json:"id"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	Decision       wireDecision    `json:"decision"`
	Answer         json.RawMessage `json:"answer,omitempty"`
}

type wireGuardrail struct {
	Guardrail string `json:"guardrail"`
	Phase     string `json:"phase"`
	Action    string `json:"action"`
	Reason    string `json:"reason,omitempty"`
	Text      string `json:"text,omitempty"`
	Canceled  bool   `json:"canceled,omitempty"`
}

// ── Encoding ─────────────────────────────────────────────────────────

//nolint:gocyclo // one case per wire kind
func encodeDelta(d Delta, o encodeOpts) (string, int, any, error) {
	// Kinds that exist in both versions are written at the encoder's.
	v := o.version
	switch x := d.(type) {
	case PartStart:
		return WirePartStart, 2, wirePartStart(x), nil
	case PartDelta:
		if o.codec.maxInline >= 0 && len(x.Data) > o.codec.maxInline {
			return "", 0, nil, fmt.Errorf("%w: part %d delta holds %d bytes, limit %d", ErrWireInlineTooLarge, x.Index, len(x.Data), o.codec.maxInline)
		}
		return WirePartDelta, 2, wirePartDelta(x), nil
	case PartEnd:
		w := wirePartEnd{Index: x.Index}
		if x.Part != nil {
			raw, err := marshalPart(x.Part, o.codec)
			if err != nil {
				return "", 0, nil, err
			}
			w.Part = raw
		}
		return WirePartEnd, 2, w, nil
	case ConversionDelta:
		return WireConversion, 2, wireConversion(x), nil
	case v1TextStart:
		return WireTextStart, 1, wireEmpty{}, nil
	case v1TextContent:
		return WireTextDelta, 1, wireContent(x), nil
	case v1TextEnd:
		return WireTextEnd, 1, wireEmpty{}, nil
	case v1ThinkingStart:
		return WireReasoningStart, 1, wireEmpty{}, nil
	case v1ThinkingContent:
		return WireReasoningDelta, 1, wireContent(x), nil
	case v1ThinkingEnd:
		return WireReasoningEnd, 1, wireSignature(x), nil
	case v1ToolCallStart:
		return WireToolCallStart, 1, wireToolCall{ID: x.ID, Name: x.Name}, nil
	case v1ToolCallArgument:
		return WireToolCallArgs, 1, wireToolCall{ID: x.ID, Content: x.Content}, nil
	case v1ToolCallEnd:
		return WireToolCallEnd, 1, wireToolCall{ID: x.ID, Arguments: x.Arguments, ArgumentsError: x.ArgumentsError}, nil
	case v1ServerToolCall:
		return WireServerToolCall, 1, wireServerTool{ID: x.ID, Kind: x.Kind, Name: x.Name, Input: x.Input}, nil
	case v1ServerToolResult:
		return WireServerToolResult, 1, wireServerTool{
			ID: x.ID, Kind: x.Kind, Text: x.Text, Result: x.Result, IsError: x.IsError, Files: x.Files,
		}, nil
	case ToolExecStartDelta:
		return WireToolExecStart, v, wireToolExec{ToolCallID: x.ToolCallID, Name: x.Name}, nil
	case ToolExecDelta:
		if x.Inner == nil {
			return "", 0, nil, fmt.Errorf("encode %s: nil inner delta", WireToolExecDelta)
		}
		inner, err := newDeltaEnvelope(x.Inner, o)
		if err != nil {
			return "", 0, nil, err
		}
		return WireToolExecDelta, max(v, inner.V), wireToolExec{ToolCallID: x.ToolCallID, Inner: &inner}, nil
	case ToolExecEndDelta:
		w := wireToolExec{ToolCallID: x.ToolCallID, Name: x.Name, Result: x.Result, Error: x.Error, Version: x.Version,
			Citations: x.Citations}
		if v == 1 {
			w.Blocks = partsToBlocks(x.Parts)
		} else {
			for _, p := range x.Parts {
				raw, err := marshalPart(p, o.codec)
				if err != nil {
					return "", 0, nil, err
				}
				w.Parts = append(w.Parts, raw)
			}
		}
		return WireToolExecEnd, v, w, nil
	case MarkerDelta:
		w := wireMarkerDelta{
			ToolCallID: x.ToolCallID, ToolName: x.ToolName, Arguments: x.Arguments, Markers: toWireMarkers(x.Markers),
		}
		if x.Interrupt != nil {
			in := toWireInterrupt(*x.Interrupt)
			w.Interrupt = &in
		}
		return WireMarker, v, w, nil
	case HandoffDelta:
		return WireHandoff, v, wireHandoff(x), nil
	case CitationDelta:
		return WireCitation, v, wireCitation(x), nil
	case ErrorDelta:
		w := EncodeError(x.Error)
		if w == nil {
			return WireError, v, wireEmpty{}, nil
		}
		return WireError, v, w, nil
	case DoneDelta:
		return WireDone, v, wireEmpty{}, nil
	case FeedbackDelta:
		return WireFeedback, v, wireFeedback(x), nil
	case UsageDelta:
		w := wireUsage{
			AccountingID: x.AccountingID, Cumulative: x.Cumulative,
			PromptTokens: x.PromptTokens, CachedPromptTokens: x.CachedPromptTokens,
			CacheWriteTokens: x.CacheWriteTokens, CompletionTokens: x.CompletionTokens, TotalTokens: x.TotalTokens,
			ResponseModel: x.ResponseModel, ResponseID: x.ResponseID, FinishReasons: x.FinishReasons, CacheHit: x.CacheHit,
			PromptByModality: x.PromptByModality, CompletionByModality: x.CompletionByModality, Requests: x.Requests,
		}
		if x.Latency != 0 {
			w.LatencyMS = durationMS(x.Latency)
		}
		return WireUsage, v, w, nil
	case RouteDelta:
		return WireRoute, v, toWireRoute(x), nil
	case TruncatedDelta:
		return WireTruncated, v, wireRunControl{NodeID: x.NodeID, Reason: x.Reason}, nil
	case QueuedDelta:
		return WireQueued, v, wireRunControl{SubmissionID: x.SubmissionID, Mode: x.Mode, Position: x.Position}, nil
	case InjectedDelta:
		return WireInjected, v, wireRunControl{SubmissionID: x.SubmissionID, Mode: x.Mode, NodeID: x.NodeID}, nil
	case InterruptedDelta:
		return WireInterrupted, v, wireRunControl{Reason: x.Reason, SubmissionID: x.SubmissionID}, nil
	case PartialJSONDelta:
		return WirePartialJSON, v, wirePartialJSON(x), nil
	case GuardrailDelta:
		return WireGuardrail, v, wireGuardrail(x), nil
	case CompactionDelta:
		return WireCompaction, v, wireCompaction{Branch: string(x.Branch), NodeID: x.NodeID, CompactionPart: x.Record}, nil
	case nil:
		return "", 0, nil, fmt.Errorf("%w: nil delta", ErrUnknownWireKind)
	default:
		return "", 0, nil, fmt.Errorf("%w: %T", ErrUnknownWireKind, d)
	}
}

// ── Decoding ─────────────────────────────────────────────────────────

//nolint:gocyclo // one case per wire kind
func decodeDelta(kind string, data json.RawMessage) (Delta, error) {
	switch kind {
	case WirePartStart:
		w, err := decodeAs[wirePartStart](kind, data)
		return PartStart(w), err
	case WirePartDelta:
		w, err := decodeAs[wirePartDelta](kind, data)
		return PartDelta(w), err
	case WirePartEnd:
		w, err := decodeAs[wirePartEnd](kind, data)
		if err != nil {
			return nil, err
		}
		end := PartEnd{Index: w.Index}
		if len(w.Part) > 0 && !isEmptyObject(w.Part) {
			p, err := UnmarshalRolePart[AssistantPart](w.Part)
			if err != nil {
				return nil, fmt.Errorf("decode %s: %w", kind, err)
			}
			end.Part = p
		}
		return end, nil
	case WireConversion:
		w, err := decodeAs[wireConversion](kind, data)
		return ConversionDelta(w), err
	case WireTextStart:
		return v1TextStart{}, nil
	case WireTextDelta:
		w, err := decodeAs[wireContent](kind, data)
		return v1TextContent(w), err
	case WireTextEnd:
		return v1TextEnd{}, nil
	case WireReasoningStart:
		return v1ThinkingStart{}, nil
	case WireReasoningDelta:
		w, err := decodeAs[wireContent](kind, data)
		return v1ThinkingContent(w), err
	case WireReasoningEnd:
		w, err := decodeAs[wireSignature](kind, data)
		return v1ThinkingEnd(w), err
	case WireToolCallStart:
		w, err := decodeAs[wireToolCall](kind, data)
		return v1ToolCallStart{ID: w.ID, Name: w.Name}, err
	case WireToolCallArgs:
		w, err := decodeAs[wireToolCall](kind, data)
		return v1ToolCallArgument{ID: w.ID, Content: w.Content}, err
	case WireToolCallEnd:
		w, err := decodeAs[wireToolCall](kind, data)
		return v1ToolCallEnd{ID: w.ID, Arguments: w.Arguments, ArgumentsError: w.ArgumentsError}, err
	case WireToolExecStart:
		w, err := decodeAs[wireToolExec](kind, data)
		return ToolExecStartDelta{ToolCallID: w.ToolCallID, Name: w.Name}, err
	case WireToolExecDelta:
		w, err := decodeAs[wireToolExec](kind, data)
		if err != nil {
			return nil, err
		}
		if w.Inner == nil {
			return nil, fmt.Errorf("decode %s: missing inner envelope", kind)
		}
		inner, err := w.Inner.Delta()
		if err != nil {
			return nil, err
		}
		return ToolExecDelta{ToolCallID: w.ToolCallID, Inner: inner}, nil
	case WireToolExecEnd:
		w, err := decodeAs[wireToolExec](kind, data)
		if err != nil {
			return nil, err
		}
		end := ToolExecEndDelta{ToolCallID: w.ToolCallID, Name: w.Name, Result: w.Result, Error: w.Error,
			Version: w.Version, Citations: w.Citations, Parts: blocksToParts(w.Blocks)}
		for _, raw := range w.Parts {
			p, err := UnmarshalRolePart[ToolOutputPart](raw)
			if err != nil {
				return nil, fmt.Errorf("decode %s: %w", kind, err)
			}
			end.Parts = append(end.Parts, p)
		}
		return end, nil
	case WireMarker:
		w, err := decodeAs[wireMarkerDelta](kind, data)
		if err != nil {
			return nil, err
		}
		md := MarkerDelta{ToolCallID: w.ToolCallID, ToolName: w.ToolName, Arguments: w.Arguments, Markers: fromWireMarkers(w.Markers)}
		if w.Interrupt != nil {
			in := fromWireInterrupt(*w.Interrupt)
			md.Interrupt = &in
		}
		return md, nil
	case WireHandoff:
		w, err := decodeAs[wireHandoff](kind, data)
		return HandoffDelta(w), err
	case WireCitation:
		w, err := decodeAs[wireCitation](kind, data)
		return CitationDelta(w), err
	case WireError:
		if isEmptyObject(data) {
			return ErrorDelta{}, nil
		}
		w, err := decodeAs[EncodedError](kind, data)
		if err != nil {
			return nil, err
		}
		return ErrorDelta{Error: DecodeError(&w)}, nil
	case WireDone:
		return DoneDelta{}, nil
	case WireFeedback:
		w, err := decodeAs[wireFeedback](kind, data)
		return FeedbackDelta(w), err
	case WireUsage:
		w, err := decodeAs[wireUsage](kind, data)
		u := UsageDelta{
			AccountingID: w.AccountingID, Cumulative: w.Cumulative,
			PromptTokens: w.PromptTokens, CachedPromptTokens: w.CachedPromptTokens,
			CacheWriteTokens: w.CacheWriteTokens, CompletionTokens: w.CompletionTokens, TotalTokens: w.TotalTokens,
			ResponseModel: w.ResponseModel, ResponseID: w.ResponseID, FinishReasons: w.FinishReasons, CacheHit: w.CacheHit,
			PromptByModality: w.PromptByModality, CompletionByModality: w.CompletionByModality, Requests: w.Requests,
		}
		if w.LatencyMS != nil {
			u.Latency = msDuration(*w.LatencyMS)
		}
		return u, err
	case WireRoute:
		w, err := decodeAs[wireRoute](kind, data)
		return w.delta(), err
	case WireTruncated:
		w, err := decodeAs[wireRunControl](kind, data)
		return TruncatedDelta{NodeID: w.NodeID, Reason: w.Reason}, err
	case WireQueued:
		w, err := decodeAs[wireRunControl](kind, data)
		return QueuedDelta{SubmissionID: w.SubmissionID, Mode: w.Mode, Position: w.Position}, err
	case WireInjected:
		w, err := decodeAs[wireRunControl](kind, data)
		return InjectedDelta{SubmissionID: w.SubmissionID, Mode: w.Mode, NodeID: w.NodeID}, err
	case WireInterrupted:
		w, err := decodeAs[wireRunControl](kind, data)
		return InterruptedDelta{Reason: w.Reason, SubmissionID: w.SubmissionID}, err
	case WireServerToolCall:
		w, err := decodeAs[wireServerTool](kind, data)
		return v1ServerToolCall{ID: w.ID, Kind: w.Kind, Name: w.Name, Input: w.Input}, err
	case WireServerToolResult:
		w, err := decodeAs[wireServerTool](kind, data)
		return v1ServerToolResult{
			ID: w.ID, Kind: w.Kind, Text: w.Text, Result: w.Result, IsError: w.IsError, Files: w.Files,
		}, err
	case WirePartialJSON:
		w, err := decodeAs[wirePartialJSON](kind, data)
		return PartialJSONDelta(w), err
	case WireGuardrail:
		w, err := decodeAs[wireGuardrail](kind, data)
		return GuardrailDelta(w), err
	case WireCompaction:
		w, err := decodeAs[wireCompaction](kind, data)
		return CompactionDelta{Branch: BranchID(w.Branch), NodeID: w.NodeID, Record: w.CompactionPart}, err
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownWireKind, kind)
	}
}

// decodeAs decodes a payload with UseNumber, so numbers inside argument and
// metadata maps keep their exact text as json.Number instead of becoming
// float64. An absent payload decodes to the zero value.
func decodeAs[T any](kind string, data json.RawMessage) (T, error) {
	var v T
	if err := decodeStrict(data, &v); err != nil {
		return v, fmt.Errorf("decode %s: %w", kind, err)
	}
	return v, nil
}

func decodeStrict(data json.RawMessage, v any) error {
	if len(data) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}

func isEmptyObject(data json.RawMessage) bool {
	t := bytes.TrimSpace(data)
	return len(t) == 0 || bytes.Equal(t, []byte("{}")) || bytes.Equal(t, []byte(jsonNull))
}

// ── Helpers ──────────────────────────────────────────────────────────

func toWireMarkers(ms []Marker) []wireMarker {
	if ms == nil {
		return nil
	}
	out := make([]wireMarker, len(ms))
	for i, m := range ms {
		out[i] = wireMarker(m)
	}
	return out
}

func fromWireMarkers(ws []wireMarker) []Marker {
	if ws == nil {
		return nil
	}
	out := make([]Marker, len(ws))
	for i, w := range ws {
		out[i] = Marker(w)
	}
	return out
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func timeVal(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// durationMS renders a duration as fractional milliseconds. Six decimal
// places keep nanosecond precision for any realistic latency.
func durationMS(d time.Duration) *float64 {
	ms := float64(d) / float64(time.Millisecond)
	return &ms
}

func msDuration(ms float64) time.Duration {
	return time.Duration(math.Round(ms * float64(time.Millisecond)))
}
