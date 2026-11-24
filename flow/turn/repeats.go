package turn

import "errors"

// WithRepeats returns err carrying a decision: whether a retry of the same
// turn would fail the same way. A Runtime that can tell marks the error its
// turn returns, and the decision then travels with the error wherever its
// structure does not (out of a sandbox task, for one). A nil err stays nil.
//
// The result unwraps to err, so errors.Is and errors.As answer as before.
func WithRepeats(err error, repeats bool) error {
	if err == nil {
		return nil
	}
	return &decided{err: err, repeats: repeats}
}

// Repeats returns the decision the outermost WithRepeats in err's chain
// carries. ok is false when none does: nobody decided, and the caller
// classifies the error itself or retries.
func Repeats(err error) (repeats, ok bool) {
	var d *decided
	if !errors.As(err, &d) {
		return false, false
	}
	return d.repeats, true
}

// decided is an error whose retry decision is already made.
type decided struct {
	err     error
	repeats bool
}

func (d *decided) Error() string { return d.err.Error() }
func (d *decided) Unwrap() error { return d.err }
