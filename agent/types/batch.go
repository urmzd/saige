package types

import (
	"context"
	"errors"
	"iter"
	"strings"
	"time"
)

// BatchProvider submits many independent model calls as one asynchronous
// job. Vendor batch endpoints finish within hours rather than seconds and
// charge about half the interactive price, which suits evals, bulk
// extraction, judges and other offline work.
//
// A request carries what a normal call carries: messages, tools, a response
// schema and request options, with dials compiled for the serving model. A
// control the batch endpoint cannot take is rejected by Submit, before
// anything is sent (D-12).
//
// Results arrive in any order and are keyed by BatchRequest.CustomID.
// Implementations live next to each adapter; agent/batch adds a local
// fallback with bounded concurrency for providers without a batch endpoint,
// and a durable job runner on top of any BatchProvider.
type BatchProvider interface {
	// Submit validates every request and creates one batch. It returns the
	// handle that names the batch in later calls. A validation error
	// matches ErrInvalidModelConfig and means nothing was submitted.
	Submit(ctx context.Context, requests []BatchRequest, opts BatchSubmitOptions) (BatchHandle, error)
	// Status reports the batch's progress.
	Status(ctx context.Context, h BatchHandle) (BatchStatus, error)
	// Results streams one result per request once the batch has ended. A
	// non-nil error ends the stream; per-request failures are results with
	// an Outcome other than BatchSucceeded, not stream errors.
	Results(ctx context.Context, h BatchHandle) iter.Seq2[BatchResult, error]
	// Cancel asks the vendor to stop the batch. Requests already finished
	// keep their results.
	Cancel(ctx context.Context, h BatchHandle) error
}

// BatchFinder is an optional BatchProvider interface for vendors that can
// look up a batch after a crash lost its handle, by metadata or by the
// request IDs in its results. FindBatch reports false when no batch matches
// and ErrBatchAmbiguous when it cannot tell.
type BatchFinder interface {
	FindBatch(ctx context.Context, q BatchQuery) (BatchHandle, bool, error)
}

// BatchQuery identifies a submission for BatchFinder.
type BatchQuery struct {
	// Tag is BatchSubmitOptions.Tag of the submission.
	Tag string
	// Since bounds the search: the submission started at or after it.
	Since time.Time
	// Count is the number of requests submitted.
	Count int
	// CustomIDs are the request IDs submitted, for vendors that can only
	// match on them.
	CustomIDs []string
}

// ErrBatchAmbiguous reports a lookup that found a batch it could not prove
// is, or is not, the one searched for.
var ErrBatchAmbiguous = errors.New("batch lookup is ambiguous")

// ErrBatchNotFound reports a handle the provider does not know, such as a
// local batch whose process exited.
var ErrBatchNotFound = errors.New("batch not found")

// BatchSubmitOptions carries job-level settings for Submit.
type BatchSubmitOptions struct {
	// Tag is stored with the batch where the vendor keeps metadata or a
	// display name, so BatchFinder can locate it again. Empty sends none.
	Tag string
}

// BatchRequest is one call in a batch.
type BatchRequest struct {
	// CustomID keys the result. It must be unique within the batch. Vendors
	// limit its alphabet and length (Anthropic: [A-Za-z0-9_-]{1,64}); the
	// agent/batch runner maps arbitrary IDs onto safe ones.
	CustomID string
	Messages []Message
	Tools    []ToolDef
	// Schema constrains the answer to JSON matching it, as
	// Stream with a schema does. Nil leaves the answer free text.
	Schema *ParameterSchema
	// Options are per-request controls and dials, applied on top of the
	// adapter's configured ones as Stream with options applies them.
	Options RequestOptions
}

// BatchHandle names a submitted batch. It is plain data, so a job record can
// persist it and a restarted process can resume with it.
type BatchHandle struct {
	// Provider is the adapter name, such as "anthropic".
	Provider string `json:"provider"`
	// Model is the model the batch runs.
	Model string `json:"model,omitempty"`
	// ID is the vendor's batch ID.
	ID string `json:"id"`
	// Meta holds vendor details a later call needs, such as the endpoint
	// of an OpenAI batch or the output location of a Vertex AI job.
	Meta map[string]string `json:"meta,omitempty"`
}

// BatchState is the provider-neutral state of a batch.
type BatchState string

const (
	// BatchPending: accepted, not yet running (validating or queued).
	BatchPending BatchState = "pending"
	// BatchRunning: requests are being processed.
	BatchRunning BatchState = "running"
	// BatchCanceling: a cancel was requested and is being applied.
	BatchCanceling BatchState = "canceling"
	// BatchEnded: processing finished; results are available. Individual
	// requests may still have failed.
	BatchEnded BatchState = "ended"
	// BatchExpired: the completion window passed. Requests that finished
	// have results; the rest report BatchExpiredOutcome.
	BatchExpired BatchState = "expired"
	// BatchCanceled: canceled. Requests that finished have results.
	BatchCanceled BatchState = "canceled"
	// BatchFailed: the batch as a whole was rejected, such as a malformed
	// input file. There are no per-request results.
	BatchFailed BatchState = "failed"
)

// Terminal reports whether the state can no longer change.
func (s BatchState) Terminal() bool {
	switch s {
	case BatchEnded, BatchExpired, BatchCanceled, BatchFailed:
		return true
	}
	return false
}

// HasResults reports whether Results can be read in this state.
func (s BatchState) HasResults() bool {
	return s == BatchEnded || s == BatchExpired || s == BatchCanceled
}

// BatchCounts tallies requests by outcome. Vendors that report counts only
// at the end leave them zero until then.
type BatchCounts struct {
	Total      int `json:"total"`
	Processing int `json:"processing,omitempty"`
	Succeeded  int `json:"succeeded,omitempty"`
	Errored    int `json:"errored,omitempty"`
	Canceled   int `json:"canceled,omitempty"`
	Expired    int `json:"expired,omitempty"`
}

// BatchStatus is a progress report.
type BatchStatus struct {
	State  BatchState  `json:"state"`
	Counts BatchCounts `json:"counts"`
	// VendorState is the vendor's own status string, for logs.
	VendorState string    `json:"vendor_state,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitzero"`
	ExpiresAt   time.Time `json:"expires_at,omitzero"`
	EndedAt     time.Time `json:"ended_at,omitzero"`
	// Error describes a BatchFailed batch.
	Error string `json:"error,omitempty"`
}

// BatchOutcome is how one request in a batch ended.
type BatchOutcome string

const (
	BatchSucceeded       BatchOutcome = "succeeded"
	BatchErrored         BatchOutcome = "errored"
	BatchCanceledOutcome BatchOutcome = "canceled"
	BatchExpiredOutcome  BatchOutcome = "expired"
)

// Billed reports whether vendors charge for a request with this outcome.
// Every vendor in the catalog bills completed requests only.
func (o BatchOutcome) Billed() bool { return o == BatchSucceeded }

// BatchResult is the result of one request.
type BatchResult struct {
	CustomID string
	Outcome  BatchOutcome
	// Message is the assistant turn of a succeeded request. With a
	// response schema it is one text block holding the JSON answer.
	Message AssistantMessage
	// FinishReason is the vendor's stop reason, normalized where the
	// adapter normalizes it for streaming calls.
	FinishReason string
	// Usage is the request's token usage. It is zero for requests vendors
	// do not bill.
	Usage UsageDelta
	// Err describes a request that did not succeed, and a succeeded
	// request that stopped at the output limit or a content filter.
	Err error
}

// Text returns the concatenated text blocks of the message.
func (r BatchResult) Text() string {
	var b strings.Builder
	for _, c := range r.Message.Parts {
		if t, ok := c.(TextPart); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// ErrBatchRequest marks a request in a batch that did not succeed. A
// BatchResult.Err matches it, along with a more specific ProviderError kind
// where the vendor reported one.
var ErrBatchRequest = errors.New("batch request did not succeed")

// BatchRequestError describes a failed, canceled or expired request.
type BatchRequestError struct {
	Outcome BatchOutcome
	// Code and Message are the vendor's error, when it gave one.
	Code    string
	Message string
	// Err is a classified ProviderError, when the adapter can classify it.
	Err error
}

func (e *BatchRequestError) Error() string {
	msg := "batch request " + string(e.Outcome)
	if e.Code != "" {
		msg += ": " + e.Code
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

func (e *BatchRequestError) Unwrap() error { return e.Err }

func (e *BatchRequestError) Is(target error) bool { return target == ErrBatchRequest }
