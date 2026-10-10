package types

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrSuspended means work is saved and no worker needs to remain attached.
var ErrSuspended = errors.New("run suspended for a durable decision")

// ApprovalRequest identifies one approval phase. Gate and marker approvals use
// distinct IDs even when they concern the same tool call.
type ApprovalRequest struct {
	ID       string
	ToolCall ToolCallPart
	Markers  []Marker
}

// ApprovalDecision is serializable. The host authenticates the decision maker.
type ApprovalDecision struct {
	Approved     bool
	ModifiedArgs map[string]any
	Message      string
	// Approver names who decided, as the host authenticated them, for
	// example a user ID. It is recorded with the call and never shown to
	// the model. Empty means the host did not say.
	Approver string `json:",omitempty"`
	// Grant extends an approval beyond this call, to the tool, to matching
	// arguments, or to the conversation, until an optional expiry. It is
	// ignored on a refusal, and when the run has no ApprovalPolicy. Only
	// the host supplies it.
	Grant *GrantRequest `json:",omitempty"`
}

// CallApproval describes how a tool call was cleared to run. The agent loop
// attaches it to the call's context once every gate and marker has passed.
type CallApproval struct {
	// Required reports that a gate or a marker held the call for a decision.
	// It is false for a call that needed none.
	Required bool
	// Approver is the ApprovalDecision.Approver of the decision that cleared
	// the call, when one did.
	Approver string
	// Grant is the ID of the standing grant that approved the call without
	// asking, when one did.
	Grant string
}

type callApprovalKey struct{}

// WithCallApproval returns ctx carrying a.
func WithCallApproval(ctx context.Context, a CallApproval) context.Context {
	return context.WithValue(ctx, callApprovalKey{}, a)
}

// CallApprovalFrom returns the approval attached to ctx, or the zero value.
func CallApprovalFrom(ctx context.Context) CallApproval {
	a, _ := ctx.Value(callApprovalKey{}).(CallApproval)
	return a
}

// ApprovalRunner persists approval requests and decisions. Pending requests
// return ErrSuspended immediately. Replay must reject changed request data.
type ApprovalRunner interface {
	ResolveApproval(context.Context, ApprovalRequest) (ApprovalDecision, error)
}

// ConcurrentStepRunner declares whether named steps can execute concurrently.
// Engines that correlate operations by sequence must leave this unsupported.
type ConcurrentStepRunner interface{ ConcurrentSteps() bool }

// ── Interrupts ───────────────────────────────────────────────────────

// Interrupt errors.
var (
	// ErrInterruptNotFound is returned when a reply names no pending interrupt.
	ErrInterruptNotFound = errors.New("interrupt not found")
	// ErrInterruptExpired is returned when a reply arrives after ExpiresAt.
	ErrInterruptExpired = errors.New("interrupt expired")
	// ErrNoInterruptRouter is returned when a run needs a human decision but
	// nothing can deliver it. It fails the run immediately instead of waiting.
	ErrNoInterruptRouter = errors.New("interrupt needs a decision but no router is configured")
	// ErrInterruptPayload is returned when an interrupt's payload or a
	// reply's answer does not have the shape its kind defines.
	ErrInterruptPayload = errors.New("interrupt payload does not match its kind")
)

// InterruptKind says what the run is waiting for.
type InterruptKind string

// Interrupt kinds.
const (
	InterruptApproval      InterruptKind = "approval"      // allow or deny a tool call
	InterruptClarification InterruptKind = "clarification" // answer a question from the model
	InterruptBudget        InterruptKind = "budget"        // raise or confirm a spending limit
	InterruptInput         InterruptKind = "input"         // supply missing input
)

// InterruptExpiry says what happens when an interrupt is not answered in time.
type InterruptExpiry string

const (
	// InterruptExpireDeny resolves the interrupt as denied. An unanswered
	// approval is never treated as a yes. It is the default.
	InterruptExpireDeny InterruptExpiry = "deny"
	// InterruptExpireEscalate re-posts the interrupt to the parent run.
	InterruptExpireEscalate InterruptExpiry = "escalate"
	// InterruptExpireFail fails the run.
	InterruptExpireFail InterruptExpiry = "fail"
)

// InterruptPolicy controls an interrupt's lifetime.
type InterruptPolicy struct {
	OnExpire InterruptExpiry `json:"on_expire,omitempty"` // zero value means InterruptExpireDeny
}

// Interrupt is one pending request for a decision from outside the run. It is
// serializable so it can cross a process boundary or wait in durable storage.
type Interrupt struct {
	ID    string
	RunID string
	// Path lists tool call IDs from the root run down to the run that raised
	// the interrupt. It is empty for the root run.
	Path []string
	Kind InterruptKind
	// Payload is the kind-specific detail: a ClarificationPayload for a
	// clarification, the ToolCallPart for an approval a durable runner
	// posts. Read it with Clarification, ToolCall or InterruptPayload.
	Payload   json.RawMessage
	Markers   []Marker
	CreatedAt time.Time
	ExpiresAt time.Time // zero means no expiry
	Policy    InterruptPolicy
}

// Expired reports whether the interrupt has passed its deadline at now.
func (i Interrupt) Expired(now time.Time) bool {
	return !i.ExpiresAt.IsZero() && !now.Before(i.ExpiresAt)
}

// InterruptReply answers an Interrupt. Replies are idempotent by
// (ID, IdempotencyKey): delivering the same reply twice has no extra effect.
type InterruptReply struct {
	ID             string
	IdempotencyKey string
	Decision       ApprovalDecision // used by approval interrupts
	// Answer is used by clarification and input interrupts: a JSON string
	// for a clarification. Build it with Answer; read it with ReplyAnswer.
	Answer json.RawMessage
}

// ClarificationPayload is the Payload of an InterruptClarification
// interrupt: the question the model asked.
type ClarificationPayload struct {
	Question string `json:"question"`
}

// InterruptPayload decodes i.Payload as a T. An empty payload, or one that
// does not decode, is an error wrapping ErrInterruptPayload.
func InterruptPayload[T any](i Interrupt) (T, error) {
	var v T
	if len(i.Payload) == 0 {
		return v, fmt.Errorf("%w: interrupt %s has no payload", ErrInterruptPayload, i.ID)
	}
	if err := json.Unmarshal(i.Payload, &v); err != nil {
		return v, fmt.Errorf("%w: interrupt %s: %w", ErrInterruptPayload, i.ID, err)
	}
	return v, nil
}

// Clarification returns the question of an InterruptClarification
// interrupt. Another kind is an error wrapping ErrInterruptPayload.
func (i Interrupt) Clarification() (ClarificationPayload, error) {
	if i.Kind != InterruptClarification {
		return ClarificationPayload{}, fmt.Errorf("%w: interrupt %s is %q, not %q", ErrInterruptPayload, i.ID, i.Kind, InterruptClarification)
	}
	return InterruptPayload[ClarificationPayload](i)
}

// ToolCall returns the tool call an approval or budget interrupt asks
// about, as a durable runner records it in the payload. Another kind, or an
// interrupt without the call, is an error wrapping ErrInterruptPayload.
func (i Interrupt) ToolCall() (ToolCallPart, error) {
	if i.Kind != InterruptApproval && i.Kind != InterruptBudget {
		return ToolCallPart{}, fmt.Errorf("%w: interrupt %s is %q, not an approval", ErrInterruptPayload, i.ID, i.Kind)
	}
	return InterruptPayload[ToolCallPart](i)
}

// ReplyAnswer decodes r.Answer as a T: a string for a clarification. An
// empty answer, or one that does not decode, is an error wrapping
// ErrInterruptPayload.
func ReplyAnswer[T any](r InterruptReply) (T, error) {
	var v T
	if len(r.Answer) == 0 {
		return v, fmt.Errorf("%w: reply to %s has no answer", ErrInterruptPayload, r.ID)
	}
	if err := json.Unmarshal(r.Answer, &v); err != nil {
		return v, fmt.Errorf("%w: reply to %s: %w", ErrInterruptPayload, r.ID, err)
	}
	return v, nil
}

// Answer returns a reply to interrupt id carrying answer as JSON, such as
// the text that answers a clarification.
func Answer(id string, answer any) (InterruptReply, error) {
	raw, err := json.Marshal(answer)
	if err != nil {
		return InterruptReply{}, fmt.Errorf("%w: %w", ErrInterruptPayload, err)
	}
	return InterruptReply{ID: id, Answer: raw}, nil
}

// ModifiedArgsAs decodes the arguments an approver substituted into a T.
// ok is false when the decision kept the model's arguments.
func ModifiedArgsAs[T any](d ApprovalDecision) (v T, ok bool, err error) {
	if d.ModifiedArgs == nil {
		return v, false, nil
	}
	raw, err := json.Marshal(d.ModifiedArgs)
	if err != nil {
		return v, true, err
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, true, fmt.Errorf("%w: modified arguments: %w", ErrInterruptPayload, err)
	}
	return v, true, nil
}

// InterruptRouter delivers interrupts to whoever can answer them and routes
// replies back to the waiting run.
type InterruptRouter interface {
	// Post publishes an interrupt and returns a channel that receives its
	// reply. A durable router returns ErrSuspended instead of waiting.
	Post(ctx context.Context, in Interrupt) (<-chan InterruptReply, error)
	// Reply delivers a decision. It is idempotent by (ID, IdempotencyKey) and
	// returns ErrInterruptNotFound or ErrInterruptExpired when it cannot apply.
	Reply(ctx context.Context, r InterruptReply) error
	// Pending lists unanswered interrupts for a run, including those raised by
	// its children.
	Pending(ctx context.Context, runID string) ([]Interrupt, error)
}

// InterruptID derives a deterministic interrupt ID: the same run, path,
// phase and call always map to the same ID, so a stored reply still matches
// it. phase separates distinct
// decisions about the same tool call, such as a gate and a marker.
func InterruptID(runID string, path []string, phase, toolCallID string) string {
	h := sha256.New()
	for _, part := range []string{runID, strings.Join(path, "/"), phase, toolCallID} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return "int_" + hex.EncodeToString(h.Sum(nil)[:16])
}
