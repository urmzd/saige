package streamcheck

import (
	"fmt"
	"sort"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// Violation is one breach of the part-delta protocol.
type Violation struct {
	// Pos is the position of the offending delta in the stream; for a part
	// left open it is the length of the stream.
	Pos int
	// Path names the tool calls whose nested stream the delta belongs to,
	// outermost first; it is empty for the top-level stream.
	Path []string
	Rule string
}

func (v Violation) String() string {
	if len(v.Path) > 0 {
		return fmt.Sprintf("delta %d (in %v): %s", v.Pos, v.Path, v.Rule)
	}
	return fmt.Sprintf("delta %d: %s", v.Pos, v.Rule)
}

// CheckParts checks a delta stream against the part-delta protocol:
//   - every part starts before its deltas, at an index not used before in
//     the stream, with a known kind;
//   - every PartDelta targets an open part and sets exactly one payload;
//   - every PartEnd closes an open part, and a Part it carries has the
//     kind the part started with;
//   - every part is closed by the end of a stream that did not fail.
//
// Deltas nested in a ToolExecDelta are checked as their own stream per tool
// call. It returns every violation, in stream order.
func CheckParts(deltas []types.Delta) []Violation {
	c := &partChecker{}
	for i, d := range deltas {
		c.check(i, nil, d)
	}
	return c.finish(len(deltas))
}

// RunPartConformance fails t once for every violation in deltas.
func RunPartConformance(t testing.TB, deltas []types.Delta) {
	t.Helper()
	for _, v := range CheckParts(deltas) {
		t.Errorf("part conformance: %s", v)
	}
}

type partState struct {
	open   map[int]types.PartKind
	used   map[int]bool
	failed bool
}

type partChecker struct {
	streams map[string]*partState // keyed by the joined tool call path
	paths   map[string][]string
	out     []Violation
}

func (c *partChecker) state(path []string) *partState {
	if c.streams == nil {
		c.streams, c.paths = map[string]*partState{}, map[string][]string{}
	}
	key := fmt.Sprint(path)
	s := c.streams[key]
	if s == nil {
		s = &partState{open: map[int]types.PartKind{}, used: map[int]bool{}}
		c.streams[key] = s
		c.paths[key] = append([]string(nil), path...)
	}
	return s
}

func (c *partChecker) fail(pos int, path []string, format string, args ...any) {
	c.out = append(c.out, Violation{Pos: pos, Path: append([]string(nil), path...), Rule: fmt.Sprintf(format, args...)})
}

func (c *partChecker) check(pos int, path []string, d types.Delta) {
	s := c.state(path)
	switch v := d.(type) {
	case types.ToolExecDelta:
		c.check(pos, append(path, v.ToolCallID), v.Inner)
	case types.ErrorDelta:
		s.failed = true
	case types.PartStart:
		switch {
		case v.Index < 0:
			c.fail(pos, path, "start at negative index %d", v.Index)
		case s.used[v.Index]:
			c.fail(pos, path, "start reuses index %d", v.Index)
		case v.Kind == "":
			c.fail(pos, path, "start at index %d has no kind", v.Index)
		}
		s.used[v.Index] = true
		s.open[v.Index] = v.Kind
	case types.PartDelta:
		if _, ok := s.open[v.Index]; !ok {
			c.fail(pos, path, "delta for index %d that is not open", v.Index)
		}
		if err := v.Validate(); err != nil {
			c.fail(pos, path, "%v", err)
		}
	case types.PartEnd:
		kind, ok := s.open[v.Index]
		if !ok {
			c.fail(pos, path, "end for index %d that is not open", v.Index)
			return
		}
		delete(s.open, v.Index)
		if v.Part != nil && v.Part.Kind() != kind {
			c.fail(pos, path, "end at index %d carries a %s part for a %s start", v.Index, v.Part.Kind(), kind)
		}
	}
}

func (c *partChecker) finish(n int) []Violation {
	keys := make([]string, 0, len(c.streams))
	for k := range c.streams {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := c.streams[k]
		if s.failed {
			continue
		}
		idx := make([]int, 0, len(s.open))
		for i := range s.open {
			idx = append(idx, i)
		}
		sort.Ints(idx)
		for _, i := range idx {
			c.fail(n, c.paths[k], "%s part at index %d never ended", s.open[i], i)
		}
	}
	return c.out
}
