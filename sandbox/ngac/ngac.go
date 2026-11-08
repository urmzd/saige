// Package ngac implements a Next Generation Access Control (NGAC) policy
// machine in the style of NIST SP 800-178: a directed acyclic graph of
// users, user attributes, objects, object attributes, and policy classes,
// connected by assignments and granted capabilities through associations,
// with prohibitions as overriding denials.
//
// In sandbox, NGAC is where access is *defined*: which agents (users) may
// exercise which operations over which workspace areas and spawn targets
// (objects). The sandbox package is where those decisions are *enforced*:
// it consults a Graph on every workspace operation and spawn attempt. The
// package is a leaf: it knows nothing about workspaces, tools, or nodes;
// operations and node names are opaque strings.
//
// Decision semantics follow NGAC:
//
//   - Access is granted only if, for EVERY policy class the object is
//     contained in, some association (ua, ops, target) covers the request:
//     the user is contained in ua, the op is in ops, the object is
//     contained in target, and target is contained in that policy class.
//   - A matching prohibition denies regardless of associations.
//   - Anything not reachable through the graph is denied: unknown users,
//     unknown objects, and objects outside every policy class.
//
// Object attributes may carry a key prefix. A request about an undeclared
// object (e.g. a workspace key like "runs/42/out") is resolved to every
// attribute whose prefix contains it, path-segment aware, so hierarchies of
// storage keys do not need to be materialized as graph nodes.
package ngac

import (
	"errors"
	"fmt"
	"strings"
)

// Kind classifies a graph node.
type Kind string

// The five kinds of node in an NGAC policy graph.
const (
	PolicyClass     Kind = "policy_class"
	UserAttribute   Kind = "user_attribute"
	User            Kind = "user"
	ObjectAttribute Kind = "object_attribute"
	Object          Kind = "object"
)

// ErrExists is returned when adding a node whose name is already taken.
var ErrExists = errors.New("ngac: node already exists")

// ErrNotFound is returned when referencing a node that was never added.
var ErrNotFound = errors.New("ngac: node not found")

type association struct {
	ua     string
	target string
	ops    map[string]bool
}

// Graph is an NGAC policy graph. Build it once per deployment and treat it
// as immutable afterwards; Can is safe for concurrent use only when no
// mutations are in flight.
type Graph struct {
	kinds    map[string]Kind
	parents  map[string][]string // assignment edges, child -> parents
	prefixes map[string]string   // object attribute -> key prefix ("" = none)
	assocs   []association
	prohibs  []association
}

// New returns an empty policy graph.
func New() *Graph {
	return &Graph{
		kinds:    make(map[string]Kind),
		parents:  make(map[string][]string),
		prefixes: make(map[string]string),
	}
}

func (g *Graph) add(name string, k Kind) error {
	if name == "" {
		return fmt.Errorf("ngac: %s name required", k)
	}
	if _, ok := g.kinds[name]; ok {
		return fmt.Errorf("%w: %q", ErrExists, name)
	}
	g.kinds[name] = k
	return nil
}

// AddPolicyClass adds a policy class node.
func (g *Graph) AddPolicyClass(name string) error { return g.add(name, PolicyClass) }

// AddUserAttribute adds a user attribute node.
func (g *Graph) AddUserAttribute(name string) error { return g.add(name, UserAttribute) }

// AddUser adds a user node. In sandbox a user is an agent or tool name.
func (g *Graph) AddUser(name string) error { return g.add(name, User) }

// AddObjectAttribute adds an object attribute node. A non-empty prefix makes
// the attribute contain every undeclared object it prefixes, path-segment
// aware ("runs" contains "runs/1/out" but not "runs-archive").
func (g *Graph) AddObjectAttribute(name, prefix string) error {
	if err := g.add(name, ObjectAttribute); err != nil {
		return err
	}
	g.prefixes[name] = prefix
	return nil
}

// AddObject adds a concrete object node, such as a spawn target.
func (g *Graph) AddObject(name string) error { return g.add(name, Object) }

// Assign adds a containment edge from child to parent (user -> user
// attribute, object -> object attribute, attribute -> attribute or policy
// class). Cycles are rejected.
func (g *Graph) Assign(child, parent string) error {
	for _, n := range []string{child, parent} {
		if _, ok := g.kinds[n]; !ok {
			return fmt.Errorf("%w: %q", ErrNotFound, n)
		}
	}
	if child == parent || g.contains(parent, child) {
		return fmt.Errorf("ngac: assigning %q to %q would create a cycle", child, parent)
	}
	g.parents[child] = append(g.parents[child], parent)
	return nil
}

// Associate grants users contained in ua the operations ops over everything
// contained in target. ua may be a user attribute or a single user; target
// may be an object attribute or a single object.
func (g *Graph) Associate(ua string, ops []string, target string) error {
	a, err := g.newAssociation(ua, ops, target)
	if err != nil {
		return err
	}
	g.assocs = append(g.assocs, a)
	return nil
}

// Prohibit denies users contained in ua the operations ops over everything
// contained in target, overriding any association.
func (g *Graph) Prohibit(ua string, ops []string, target string) error {
	a, err := g.newAssociation(ua, ops, target)
	if err != nil {
		return err
	}
	g.prohibs = append(g.prohibs, a)
	return nil
}

func (g *Graph) newAssociation(ua string, ops []string, target string) (association, error) {
	for _, n := range []string{ua, target} {
		if _, ok := g.kinds[n]; !ok {
			return association{}, fmt.Errorf("%w: %q", ErrNotFound, n)
		}
	}
	if len(ops) == 0 {
		return association{}, fmt.Errorf("ngac: association %q -> %q needs at least one operation", ua, target)
	}
	set := make(map[string]bool, len(ops))
	for _, op := range ops {
		set[op] = true
	}
	return association{ua: ua, target: target, ops: set}, nil
}

// Can reports whether user may perform op on object. object may be a
// declared object node or an arbitrary key resolved through attribute
// prefixes. The default is deny.
func (g *Graph) Can(user, op, object string) bool {
	userSet := g.closure(user, nil)
	objSet := g.closure(object, g.resolvePrefixes(object))

	// Prohibitions override everything.
	for _, p := range g.prohibs {
		if p.ops[op] && userSet[p.ua] && objSet[p.target] {
			return false
		}
	}

	// Every policy class containing the object must be satisfied.
	satisfied := false
	for node := range objSet {
		if g.kinds[node] != PolicyClass {
			continue
		}
		satisfied = true
		if !g.classSatisfied(node, op, userSet, objSet) {
			return false
		}
	}
	return satisfied
}

// classSatisfied reports whether some association grants op within pc.
func (g *Graph) classSatisfied(pc, op string, userSet, objSet map[string]bool) bool {
	for _, a := range g.assocs {
		if !a.ops[op] || !userSet[a.ua] || !objSet[a.target] {
			continue
		}
		if a.target == pc || g.contains(a.target, pc) {
			return true
		}
	}
	return false
}

// resolvePrefixes returns the object attributes whose prefix contains key.
func (g *Graph) resolvePrefixes(key string) []string {
	var atts []string
	for att, prefix := range g.prefixes {
		if prefix == "" {
			continue
		}
		if key == prefix || strings.HasPrefix(key, prefix+"/") {
			atts = append(atts, att)
		}
	}
	return atts
}

// closure returns start, extra roots, and every node reachable upward from
// them through assignment edges.
func (g *Graph) closure(start string, extra []string) map[string]bool {
	seen := make(map[string]bool)
	queue := append([]string{start}, extra...)
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if seen[n] {
			continue
		}
		seen[n] = true
		queue = append(queue, g.parents[n]...)
	}
	return seen
}

// contains reports whether ancestor is reachable upward from node.
func (g *Graph) contains(node, ancestor string) bool {
	return g.closure(node, nil)[ancestor]
}
