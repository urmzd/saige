package failure_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	saigetypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/flow/turn"
	"github.com/urmzd/saige/flow/turn/failure"
)

func provider(kind saigetypes.ErrorKind) error {
	return &saigetypes.ProviderError{Provider: "p", Model: "m", Kind: kind, Err: errors.New("refused"), RetryAfter: time.Second}
}

func chain(members ...error) error { return &saigetypes.FallbackError{Errors: members} }

var cases = []struct {
	name    string
	err     error
	repeats bool
}{
	{"nil", nil, false},
	{"an unknown error", errors.New("boom"), false},

	{"max iterations", saigetypes.ErrMaxIterations, true},
	{"repeated tool calls", saigetypes.ErrRepeatedToolCalls, true},
	{"invalid model config", saigetypes.ErrInvalidModelConfig, true},
	{"media unavailable", saigetypes.ErrMediaUnavailable, true},
	{"context length", provider(saigetypes.ErrorKindContextLength), true},
	{"wrapped twice", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", saigetypes.ErrMaxIterations)), true},
	{"after retries", &saigetypes.RetryError{Attempts: 3, Last: provider(saigetypes.ErrorKindContextLength)}, true},

	{"tool error limit", saigetypes.ErrToolErrorLimit, false},
	{"guardrail tripped", saigetypes.ErrGuardrailTripped, false},
	{"budget exceeded", saigetypes.ErrBudgetExceeded, false},
	{"transient", provider(saigetypes.ErrorKindTransient), false},
	{"rate limited", provider(saigetypes.ErrorKindRateLimit), false},
	{"unavailable", provider(saigetypes.ErrorKindUnavailable), false},
	{"auth", provider(saigetypes.ErrorKindAuth), false},
	{"permanent", provider(saigetypes.ErrorKindPermanent), false},
	{"invalid request", provider(saigetypes.ErrorKindInvalidRequest), false},
	{"truncated", provider(saigetypes.ErrorKindTruncated), false},
	{"content filter", provider(saigetypes.ErrorKindContentFilter), false},
	{"rate limited after retries", &saigetypes.RetryError{Attempts: 3, Last: provider(saigetypes.ErrorKindRateLimit)}, false},

	{"canceled", context.Canceled, false},
	{"deadline exceeded", context.DeadlineExceeded, false},
	{"stream canceled", saigetypes.ErrStreamCanceled, false},
	{"max iterations as the context ended", errors.Join(context.Canceled, saigetypes.ErrMaxIterations), false},
	{"max iterations as the deadline passed", errors.Join(context.DeadlineExceeded, saigetypes.ErrMaxIterations), false},

	{"chain: context length twice", chain(provider(saigetypes.ErrorKindContextLength), provider(saigetypes.ErrorKindContextLength)), true},
	{"chain: auth, then context length", chain(provider(saigetypes.ErrorKindAuth), provider(saigetypes.ErrorKindContextLength)), true},
	{"chain: invalid model config twice", chain(saigetypes.ErrInvalidModelConfig, fmt.Errorf("backup: %w", saigetypes.ErrInvalidModelConfig)), true},
	{"chain: rate limited, then context length", chain(provider(saigetypes.ErrorKindRateLimit), provider(saigetypes.ErrorKindContextLength)), false},
	// TestARouterThatFailedOverFromAPassingFailureIsRetried in
	// flow/turn/native builds this chain through a real router: this package
	// may not import saige's providers.
	{"chain: unavailable, then invalid model config", chain(provider(saigetypes.ErrorKindUnavailable), saigetypes.ErrInvalidModelConfig), false},
	{"chain: transient, then media unavailable", chain(provider(saigetypes.ErrorKindTransient), saigetypes.ErrMediaUnavailable), false},
	{"chain: canceled, then context length", chain(context.Canceled, provider(saigetypes.ErrorKindContextLength)), false},
	{"chain: context length, then rate limited", chain(provider(saigetypes.ErrorKindContextLength), provider(saigetypes.ErrorKindRateLimit)), false},
	{"chain in a chain: rate limited inside", chain(chain(provider(saigetypes.ErrorKindRateLimit), provider(saigetypes.ErrorKindAuth)), provider(saigetypes.ErrorKindContextLength)), false},
	{"chain, wrapped: rate limited, then context length", fmt.Errorf("turn: %w", chain(provider(saigetypes.ErrorKindRateLimit), provider(saigetypes.ErrorKindContextLength))), false},
	{"chain: empty", chain(), false},
}

func TestRepeats(t *testing.T) {
	t.Parallel()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := failure.Repeats(tc.err); got != tc.repeats {
				t.Errorf("Repeats(%v) = %v, want %v", tc.err, got, tc.repeats)
			}
		})
	}
}

// The decision crosses a task boundary as it was made, through JSON, and the
// rebuilt error keeps its message and the sentinels it matched.
func TestTheDecisionSurvivesTheWire(t *testing.T) {
	t.Parallel()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(failure.Encode(tc.err))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var wire *failure.Wire
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			back := wire.Err()
			if tc.err == nil {
				if back != nil {
					t.Fatalf("a nil error came back as %v", back)
				}
				return
			}
			if back == nil {
				t.Fatal("the error came back nil")
			}
			if back.Error() != tc.err.Error() {
				t.Errorf("message %q, want %q", back.Error(), tc.err.Error())
			}
			wrapped := fmt.Errorf("task: %w", back)
			if got := failure.Repeats(wrapped); got != tc.repeats {
				t.Errorf("Repeats after the wire = %v, want %v", got, tc.repeats)
			}
			for _, sentinel := range []error{
				saigetypes.ErrMaxIterations, saigetypes.ErrRepeatedToolCalls, saigetypes.ErrToolErrorLimit,
				saigetypes.ErrContextLength, saigetypes.ErrInvalidModelConfig, saigetypes.ErrGuardrailTripped,
				saigetypes.ErrRateLimited, context.Canceled,
			} {
				if errors.Is(tc.err, sentinel) && !errors.Is(wrapped, sentinel) {
					t.Errorf("the error no longer matches %v", sentinel)
				}
			}
			if saigetypes.IsTransient(tc.err) != saigetypes.IsTransient(wrapped) {
				t.Errorf("IsTransient changed across the wire")
			}
		})
	}
}

// An error whose form carried no decision is retried, whatever it matches.
func TestAFormWithoutADecisionIsRetried(t *testing.T) {
	t.Parallel()
	var wire failure.Wire
	if err := json.Unmarshal([]byte(`{"error":{"message":"max iterations reached","kind":"permanent","retryable":false,"codes":["max_iterations"]}}`), &wire); err != nil {
		t.Fatal(err)
	}
	err := wire.Err()
	if !errors.Is(err, saigetypes.ErrMaxIterations) {
		t.Fatalf("the error does not match its sentinel: %v", err)
	}
	if failure.Repeats(err) {
		t.Error("Repeats is true for a form that did not say so")
	}
	if (&failure.Wire{}).Err() != nil || (*failure.Wire)(nil).Err() != nil {
		t.Error("an empty form is not a nil error")
	}
}

// A decision a Runtime already made is the answer, whatever the error matches.
func TestADecisionAlreadyMadeIsKept(t *testing.T) {
	t.Parallel()
	if failure.Repeats(fmt.Errorf("task: %w", turn.WithRepeats(saigetypes.ErrMaxIterations, false))) {
		t.Error("Repeats is true for an error decided not to repeat")
	}
	if !failure.Repeats(fmt.Errorf("task: %w", turn.WithRepeats(errors.New("boom"), true))) {
		t.Error("Repeats is false for an error decided to repeat")
	}
	if turn.WithRepeats(nil, true) != nil {
		t.Error("a nil error did not stay nil")
	}
	if _, ok := turn.Repeats(saigetypes.ErrMaxIterations); ok {
		t.Error("an unmarked error reports a decision")
	}
}
