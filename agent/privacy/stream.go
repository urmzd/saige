package privacy

import "strings"

// StreamRestorer restores placeholders in text that arrives in fragments. A
// placeholder can be split across fragments, so the restorer holds back any
// suffix that could still become one and releases it once it is complete or
// can no longer match. Call Flush when the text ends, the stream finishes, or
// it fails, so no held text is lost.
//
// A StreamRestorer is not safe for concurrent use; use one per text block.
type StreamRestorer struct {
	restore func(string) string
	held    string
}

// NewStreamRestorer returns a restorer that calls restore on each releasable
// prefix, for example Vault.Restore.
func NewStreamRestorer(restore func(string) string) *StreamRestorer {
	return &StreamRestorer{restore: restore}
}

// Write adds a fragment and returns the restored text that is safe to emit.
// It may return "" while a possible placeholder is pending.
func (r *StreamRestorer) Write(fragment string) string {
	buf := r.held + fragment
	cut := holdFrom(buf)
	r.held = buf[cut:]
	return r.restore(buf[:cut])
}

// Flush returns everything still held, restored.
func (r *StreamRestorer) Flush() string {
	out := r.restore(r.held)
	r.held = ""
	return out
}

// holdFrom returns the index from which buf must be held back: the start of
// the last "<<" that is not yet closed by ">>", or a trailing "<" that may be
// the first half of one. A pending opener longer than any placeholder is
// released, since it can no longer complete.
func holdFrom(buf string) int {
	open := strings.LastIndex(buf, "<<")
	if open >= 0 && !strings.Contains(buf[open:], ">>") && len(buf)-open <= maxPlaceholderLen && couldBePlaceholder(buf[open+2:]) {
		return open
	}
	if strings.HasSuffix(buf, "<") {
		return len(buf) - 1
	}
	return len(buf)
}

// couldBePlaceholder reports whether s can still grow into the body and
// closing of a placeholder: label characters, then optionally a ">".
func couldBePlaceholder(s string) bool {
	for i, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		case r == '>' && i == len(s)-1:
		default:
			return false
		}
	}
	return true
}
