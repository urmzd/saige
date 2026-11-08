package runtime

import "fmt"

// ReservedName is the one name no agent may take. It names no subscriber,
// so a run can never be confused with a request addressed to one.
const ReservedName = "_control"

// CheckRun reports whether agent and runID can name a run: each one segment
// of ASCII letters, digits, '-' or '_', so either is safe to embed in stream
// keys and URLs unescaped.
func CheckRun(agent, runID string) error {
	if err := checkSegment("agent name", agent); err != nil {
		return err
	}
	return checkSegment("run id", runID)
}

func checkSegment(what, s string) error {
	if s == "" {
		return fmt.Errorf("%w: empty %s", ErrInvalid, what)
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return fmt.Errorf("%w: illegal character %q in %s %q at position %d", ErrInvalid, c, what, s, i)
		}
	}
	return nil
}
