package wrapper

import (
	"context"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

type leaf struct{ name string }

func (leaf) ChatStream(context.Context, []types.Message, []types.ToolDef) (<-chan types.Delta, error) {
	return nil, nil
}

type marked struct{ leaf }

func (marked) Mark() string { return "found" }

type marker interface{ Mark() string }

type single struct {
	leaf
	inner types.Provider
}

func (s single) Unwrap() types.Provider { return s.inner }

type multi struct {
	leaf
	members []types.Provider
}

func (m multi) Unwrap() []types.Provider { return m.members }

// loop unwraps to itself.
type loop struct{ leaf }

func (l *loop) Unwrap() types.Provider { return l }

func TestAs(t *testing.T) {
	target := marked{leaf{"target"}}
	for _, tc := range []struct {
		name string
		p    types.Provider
		want bool
	}{
		{"self", target, true},
		{"through one wrapper", single{inner: target}, true},
		{"through nested wrappers", single{inner: single{inner: target}}, true},
		{"second member of a multi wrapper", multi{members: []types.Provider{leaf{"a"}, single{inner: target}}}, true},
		{"absent", single{inner: leaf{"a"}}, false},
		{"self-referential wrapper terminates", &loop{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, ok := As[marker](tc.p)
			if ok != tc.want || (ok && m.Mark() != "found") {
				t.Fatalf("As = %v, %v", m, ok)
			}
		})
	}
}

func TestUnwrapMembersInnermost(t *testing.T) {
	a, b := leaf{"a"}, leaf{"b"}
	if Unwrap(single{inner: a}) != types.Provider(a) || Unwrap(a) != nil {
		t.Fatal("Unwrap")
	}
	if got := Members(multi{members: []types.Provider{a, b}}); len(got) != 2 {
		t.Fatalf("Members = %v", got)
	}
	if Members(a) != nil {
		t.Fatal("a leaf has members")
	}
	if Innermost(single{inner: single{inner: a}}) != types.Provider(a) {
		t.Fatal("Innermost")
	}
	m := multi{members: []types.Provider{a}}
	if Innermost(single{inner: m}).(multi).members[0] != types.Provider(a) {
		t.Fatal("Innermost should stop at a multi wrapper")
	}
	var visited []string
	Walk(multi{members: []types.Provider{single{inner: a}, b}}, func(p types.Provider) bool {
		if l, ok := p.(leaf); ok {
			visited = append(visited, l.name)
		}
		return true
	})
	if len(visited) != 2 || visited[0] != "a" || visited[1] != "b" {
		t.Fatalf("Walk order = %v", visited)
	}
}
