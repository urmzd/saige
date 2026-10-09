package mcp

import (
	"fmt"
	"unicode/utf8"
)

// DefaultMaxResultBytes caps the text in one tool result when the spec sets
// no limit. One oversized response is stored in the transcript and resent on
// every later model turn, so the cap protects the context window.
const DefaultMaxResultBytes = 256 << 10

// DefaultMaxBinaryBytes caps image, audio and blob bytes in one tool result.
const DefaultMaxBinaryBytes = 5 << 20

// resultLimits tracks the remaining text and binary budget of one result.
// A negative budget means unlimited.
type resultLimits struct {
	textLeft   int
	binaryLeft int
}

func newResultLimits(spec ServerSpec) *resultLimits {
	pick := func(v, def int) int {
		switch {
		case v == 0:
			return def
		case v < 0:
			return -1
		default:
			return v
		}
	}
	return &resultLimits{
		textLeft:   pick(spec.MaxResultBytes, DefaultMaxResultBytes),
		binaryLeft: pick(spec.MaxBinaryBytes, DefaultMaxBinaryBytes),
	}
}

// text returns s cut to the remaining text budget, on a rune boundary, with a
// marker naming how much was dropped.
func (l *resultLimits) text(s string) string {
	if l.textLeft < 0 {
		return s
	}
	if len(s) <= l.textLeft {
		l.textLeft -= len(s)
		return s
	}
	cut := l.textLeft
	for cut > 0 && cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut--
	}
	l.textLeft = 0
	return fmt.Sprintf("%s\n[truncated %d bytes]", s[:cut], len(s)-cut)
}

// take reserves n bytes of text budget whole, or reports that they do not fit.
func (l *resultLimits) take(n int) bool {
	if l.textLeft < 0 {
		return true
	}
	if n > l.textLeft {
		return false
	}
	l.textLeft -= n
	return true
}

// binary reserves n bytes of binary budget whole, or reports that they do not
// fit. Binary content is never cut: half an image is not an image.
func (l *resultLimits) binary(n int) bool {
	if l.binaryLeft < 0 {
		return true
	}
	if n > l.binaryLeft {
		return false
	}
	l.binaryLeft -= n
	return true
}
