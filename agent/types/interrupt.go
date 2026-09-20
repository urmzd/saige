package types

import (
	"context"
	"errors"
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
