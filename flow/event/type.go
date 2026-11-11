package event

import (
	"strconv"
	"strings"
)

// ValidType reports whether t follows the type grammar:
//
//	type   = family *( "." name ) ".v" N
//	name   = [a-z0-9_]+
//	N      = [1-9][0-9]*
func ValidType(t string) bool {
	segs := strings.Split(t, ".")
	if len(segs) < 2 {
		return false
	}
	for _, s := range segs[:len(segs)-1] {
		if !validName(s) {
			return false
		}
	}
	_, ok := versionOf(segs[len(segs)-1])
	return ok
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

func versionOf(seg string) (int, bool) {
	if len(seg) < 2 || seg[0] != 'v' || seg[1] == '0' {
		return 0, false
	}
	for i := 1; i < len(seg); i++ {
		if seg[i] < '0' || seg[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(seg[1:])
	if err != nil {
		return 0, false
	}
	return n, true
}

// Family returns the first segment of a type: "run", "control", "github".
func Family(t string) string {
	if i := strings.IndexByte(t, '.'); i >= 0 {
		return t[:i]
	}
	return t
}

// Version returns the version a type carries, and false when its last
// segment is not a version.
func Version(t string) (int, bool) {
	i := strings.LastIndexByte(t, '.')
	if i < 0 {
		return 0, false
	}
	return versionOf(t[i+1:])
}

// Base returns a type without its version: "run.completed" for
// "run.completed.v1". A string with no version is returned as it is.
func Base(t string) string {
	if _, ok := Version(t); ok {
		return t[:strings.LastIndexByte(t, '.')]
	}
	return t
}

// HasPrefix reports whether prefix selects t, segment by segment: "run.delta"
// selects "run.delta.text.v1" and not "run.deltas.v1". An empty prefix
// selects nothing.
func HasPrefix(t, prefix string) bool {
	if prefix == "" || !strings.HasPrefix(t, prefix) {
		return false
	}
	return len(t) == len(prefix) || t[len(prefix)] == '.'
}

// optInFamily is the prefix a subscription must name to receive an opt-in
// type: its first two segments, such as "run.usage".
func optInFamily(t string) string {
	segs := strings.SplitN(Base(t), ".", 3)
	if len(segs) < 2 {
		return segs[0]
	}
	return segs[0] + "." + segs[1]
}

// Selects reports whether a subscription to prefix receives events of type t.
// An opt-in type (Entry.OptIn) is selected only by a prefix that names its
// family explicitly: "run.usage" selects "run.usage.v1", and "run" does not.
func Selects(prefix, t string) bool {
	if !HasPrefix(t, prefix) {
		return false
	}
	e, ok := Lookup(t)
	if !ok || !e.OptIn {
		return true
	}
	return len(prefix) >= len(optInFamily(t))
}
