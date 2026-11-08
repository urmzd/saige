package run

import (
	"errors"
	"fmt"
)

// nonRetryable marks an error that must fail the run instead of burning
// retry attempts: replaying identical code cannot fix it.
type nonRetryable struct{ err error }

func (n nonRetryable) Error() string { return n.err.Error() }
func (n nonRetryable) Unwrap() error { return n.err }

// NonRetryable marks err as terminal: the activity's failure is recorded in
// the ledger (the failure itself is memoized) and the run completes failed
// without further attempts.
func NonRetryable(err error) error {
	if err == nil {
		return nil
	}
	return nonRetryable{err: err}
}

// IsNonRetryable reports whether err carries the NonRetryable marker (or is
// a replayed RecordedError) anywhere in its chain.
func IsNonRetryable(err error) bool {
	var n nonRetryable
	if errors.As(err, &n) {
		return true
	}
	var r *RecordedError
	return errors.As(err, &r)
}

// RecordedError is a memoized non-retryable failure replayed from the
// ledger: the stable form of the error an activity recorded on a previous
// attempt. It is itself non-retryable.
type RecordedError struct {
	Name    string
	Key     string
	Message string
}

func (e *RecordedError) Error() string {
	return fmt.Sprintf("durable: %s (%s): %s", e.Name, e.Key, e.Message)
}
