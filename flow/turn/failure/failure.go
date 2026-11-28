// Package failure decides which failed turns a retry would only repeat, and
// carries that decision with the error across a task boundary.
//
// A durable run retries a failed turn by running the whole turn again, model
// calls included. Repeats names the few failures for which that reaches the
// same end. Everything else is retried: when it is unclear whether a failure
// would come back, the answer is that it would not.
//
// The decision itself is turn.WithRepeats, which needs nothing of saige;
// this package reads saige's error types to make it, and imports nothing
// else of saige's agent side.
package failure

import (
	"context"
	"errors"

	saigetypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/flow/turn"
)

// repeating lists the failures a retry of the same input ends in again.
//
// Not listed, and so retried: a guardrail that tripped (a check on model
// output sees new output on a retry, a check that calls a model can answer
// differently, and a check that itself failed says nothing about the input)
// and a run whose tool calls kept failing (the tools' faults may have been
// passing ones).
//
// A turn the output limit cut short repeats too, and has no sentinel of its
// own to list: see outputLimit.
var repeating = []error{
	// The agent loop used every iteration it was given.
	saigetypes.ErrMaxIterations,
	// The model asked for the same tool calls with the same arguments too
	// often in a row.
	saigetypes.ErrRepeatedToolCalls,
	// The request was refused locally, before any network call.
	saigetypes.ErrInvalidModelConfig,
	// The prompt does not fit the model's context window.
	saigetypes.ErrContextLength,
}

// Repeats reports whether a retry of the same turn would fail the same way.
//
// It is false for a nil error, for a turn whose context ended, for a
// transient failure, and for a fallback chain in which any member failed
// transiently or was cut short: a retry reaches that member again and may
// get an answer from it. An error that carries a decision (turn.WithRepeats:
// one rebuilt from a Wire, or one a Runtime marked) answers that decision,
// whatever sentinels it still matches.
func Repeats(err error) bool {
	if err == nil {
		return false
	}
	if repeats, ok := turn.Repeats(err); ok {
		return repeats
	}
	if interrupted(err) || passing(err) {
		return false
	}
	for _, sentinel := range repeating {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return outputLimit(err)
}

// outputLimit reports whether err is a turn the model's output limit cut
// short: a tool call it could not finish writing, or a limit spent before
// any answer. The same input meets the same limit, and each attempt spends
// the whole of it first, so a retry costs the most a model call can and
// ends where the last one did. The caller is told instead, and can ask for
// less.
//
// A stream that only ended early (types.ErrResponseTruncated with no output
// limit behind it, as when a connection drops while a tool call is open) is
// not this, and is retried.
//
// Nor is an output limit under a guardrail that tripped or a run whose tool
// calls kept failing. Those are retried for what a new attempt changes (see
// repeating), and the limit below them was met by a check's own model call,
// or by the answer forced after the tools had failed: an attempt that goes
// another way does not reach it.
func outputLimit(err error) bool {
	if errors.Is(err, saigetypes.ErrGuardrailTripped) || errors.Is(err, saigetypes.ErrToolErrorLimit) {
		return false
	}
	var cut *saigetypes.ResponseTruncatedError
	if errors.As(err, &cut) {
		return true
	}
	var p *saigetypes.ProviderError
	return errors.As(err, &p) && p.Kind == saigetypes.ErrorKindTruncated
}

// interrupted reports whether err is a turn that was cut short and did not
// run to an end of its own.
func interrupted(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, saigetypes.ErrStreamCanceled)
}

// passing reports whether err, or any member of a fallback chain in it, is a
// failure that may be gone on the next attempt. A *FallbackError is
// classified by its last member alone (see its Unwrap), so its members are
// read here one by one.
func passing(err error) bool {
	if saigetypes.IsTransient(err) {
		return true
	}
	var chain *saigetypes.FallbackError
	if !errors.As(err, &chain) {
		return false
	}
	for _, member := range chain.Errors {
		if member == nil {
			continue
		}
		if interrupted(member) || passing(member) {
			return true
		}
	}
	return false
}

// Wire is a failed turn's error in the form that crosses a task boundary:
// the error as saige encodes it, and whether a retry would repeat it. The
// decision is made where the error still has its structure and travels as a
// field, because the encoded error keeps only the sentinels it matched.
type Wire struct {
	Error   *saigetypes.EncodedError `json:"error"`
	Repeats bool                     `json:"repeats,omitempty"`
}

// Encode returns the wire form of err, or nil for a nil error.
func Encode(err error) *Wire {
	if err == nil {
		return nil
	}
	return &Wire{Error: saigetypes.EncodeError(err), Repeats: Repeats(err)}
}

// Err rebuilds the error. It keeps the message, matches every registered
// sentinel the original matched, and answers Repeats as the original did. A
// nil form, or one with no error, is nil.
func (w *Wire) Err() error {
	if w == nil || w.Error == nil {
		return nil
	}
	return turn.WithRepeats(saigetypes.DecodeError(w.Error), w.Repeats)
}
