package durable

import (
	"fmt"

	"github.com/urmzd/saige/durable/run"
)

// NonRetryable marks an error terminal: the failure is recorded (memoized)
// and the run fails without burning further attempts. Re-exported from
// pkg/run for the common case.
func NonRetryable(err error) error { return run.NonRetryable(err) }

// DecodeError reports a payload that would not round-trip through the codec
// — a recorded output that no longer decodes into today's type, or an
// unserializable value. Always terminal: retrying deterministic corruption
// is a hazard, not resilience. Fix: versioned names for breaking type
// changes ("charge-payment.v2").
type DecodeError struct {
	Name string
	Err  error
}

func (e *DecodeError) Error() string {
	return fmt.Sprintf("durable: decode %q: %v", e.Name, e.Err)
}

func (e *DecodeError) Unwrap() error { return e.Err }

// RunFailedError is the terminal failure of a run, surfaced by
// Handle.Result.
type RunFailedError struct {
	RunID   string
	Message string
}

func (e *RunFailedError) Error() string {
	return fmt.Sprintf("durable: run %s failed: %s", e.RunID, e.Message)
}
