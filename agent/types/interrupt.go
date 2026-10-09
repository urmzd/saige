package types

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ErrSuspended means work is saved and no worker needs to remain attached.
var ErrSuspended = errors.New("run suspended for a durable decision")

// ApprovalRequest identifies one approval phase. Gate and marker approvals use
// distinct IDs even when they concern the same tool call.
type ApprovalRequest struct {
	ID       string
	ToolCall ToolUseContent
	Markers  []Marker
}

// ApprovalDecision is serializable. The host authenticates the decision maker.
type ApprovalDecision struct {
	Approved     bool
	ModifiedArgs map[string]any
	Message      string
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
)

// InterruptKind says what the run is waiting for.
type InterruptKind string

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
	Path      []string
	Kind      InterruptKind
	Payload   json.RawMessage // kind-specific detail, e.g. a question or the tool arguments
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
	Answer         json.RawMessage  // used by clarification and input interrupts
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
