// Package sandbox confines what agents can touch. It is the enforcement
// point: every workspace operation and spawn attempt a tool makes passes
// through a decision point (PDP) before it reaches the backend, and the
// default is deny.
//
// Access is *defined* in an NGAC policy graph (package ngac) and *enforced*
// here. Two ways to define it:
//
//   - Policy, a flat per-tool grant of workspace areas and spawn targets.
//     FromPolicies compiles a set of them into an NGAC graph. This is the
//     simple path and covers most deployments.
//   - A full ngac.Spec (ServiceSpec.Access) for relational policies: user
//     attributes grouping agents, shared object attributes, multiple policy
//     classes, and prohibitions.
//
// Either way, tools never receive the raw workspace: they get the view
// ScopePDP returns, and a tool nothing was granted to can do nothing.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/urmzd/saige/sandbox/ngac"
	"github.com/urmzd/saige/sandbox/workspace"
)

// Operations checked by the sandbox. Associations in the policy graph grant
// these; anything else a graph grants is ignored by the enforcement layer.
const (
	OpRead   = "read"
	OpWrite  = "write"
	OpDelete = "delete"
	OpSpawn  = "spawn"
)

// ErrDenied is returned when an operation is not granted by the policy in
// force.
var ErrDenied = errors.New("sandbox: access denied by policy")

// PDP is the policy decision point the sandbox consults. *ngac.Graph
// implements it; test doubles can too.
type PDP interface {
	Can(user, op, object string) bool
}

// SpawnObject returns the canonical policy-graph object name for spawning
// tool, "tool:<name>". Spawn grants target these objects so that workspace
// keys and spawn targets can never collide.
func SpawnObject(tool string) string { return "tool:" + tool }

// Area grants access to one workspace key prefix. A key matches an area when
// it equals the prefix or sits underneath it as a path segment
// ("runs" matches "runs/1/out" but not "runs-archive").
type Area struct {
	// Prefix is the workspace key prefix this area covers.
	Prefix string `json:"prefix"`
	// ReadOnly limits the area to reading and listing when true.
	ReadOnly bool `json:"read_only,omitempty"`
}

// Policy is the flat form of access definition: it binds one tool to the
// workspace areas it may touch and the tools it may spawn sub-tasks for.
// Tools without a policy get neither.
type Policy struct {
	// Tool is the name of the tool this policy applies to.
	Tool string `json:"tool"`
	// Areas are the prefixes the tool may access. Empty means deny all.
	Areas []Area `json:"areas"`
	// Spawn lists the tool names this tool may submit sub-tasks for:
	// the self-referential path where an agent calls another agent (or
	// itself). Empty means the tool cannot spawn anything.
	Spawn []string `json:"spawn,omitempty"`
}

// FromPolicies compiles flat policies into an NGAC policy graph: one policy
// class, one user per tool, one object attribute per area, and one object
// per spawn target. An empty set yields a graph that denies everything.
func FromPolicies(policies []Policy) PDP {
	g := ngac.New()
	const pc = "pc:default"
	must(g.AddPolicyClass(pc))

	for _, p := range policies {
		if err := g.AddUser(p.Tool); err != nil {
			continue // duplicate tool: first policy wins
		}
		for i, area := range p.Areas {
			oa := fmt.Sprintf("area:%s:%d", p.Tool, i)
			must(g.AddObjectAttribute(oa, area.Prefix))
			must(g.Assign(oa, pc))
			ops := []string{OpRead}
			if !area.ReadOnly {
				ops = append(ops, OpWrite, OpDelete)
			}
			must(g.Associate(p.Tool, ops, oa))
		}
		if len(p.Spawn) > 0 {
			oa := "spawn:" + p.Tool
			must(g.AddObjectAttribute(oa, ""))
			must(g.Assign(oa, pc))
			for _, target := range p.Spawn {
				obj := SpawnObject(target)
				if err := g.AddObject(obj); err != nil && !errors.Is(err, ngac.ErrExists) {
					panic(err)
				}
				must(g.Assign(obj, oa))
			}
			must(g.Associate(p.Tool, []string{OpSpawn}, oa))
		}
	}
	return g
}

// must panics on impossible-by-construction graph errors: FromPolicies
// generates every node name itself, so a failure is a bug, not bad input.
func must(err error) {
	if err != nil {
		panic("sandbox: compile policies: " + err.Error())
	}
}

// Scope returns a view of ws restricted to policy p, the flat-policy
// shorthand for ScopePDP.
func Scope(ws workspace.Workspace, p Policy) workspace.Workspace {
	return ScopePDP(ws, p.Tool, FromPolicies([]Policy{p}))
}

// ScopePDP returns a view of ws on which every operation by user is checked
// against pdp before it reaches the backend; violations return ErrDenied. A
// nil pdp denies everything.
func ScopePDP(ws workspace.Workspace, user string, pdp PDP) workspace.Workspace {
	return &scoped{ws: ws, user: user, pdp: pdp}
}

type scoped struct {
	ws   workspace.Workspace
	user string
	pdp  PDP
}

func (s *scoped) allow(op, key string) error {
	if !workspace.ValidKey(key) {
		return fmt.Errorf("%w: %q", workspace.ErrInvalidKey, key)
	}
	if s.pdp == nil || !s.pdp.Can(s.user, op, key) {
		return fmt.Errorf("%w: user %q, op %s, key %q", ErrDenied, s.user, op, key)
	}
	return nil
}

func (s *scoped) Read(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := s.allow(OpRead, key); err != nil {
		return nil, err
	}
	return s.ws.Read(ctx, key)
}

func (s *scoped) Write(ctx context.Context, key string, r io.Reader) error {
	if err := s.allow(OpWrite, key); err != nil {
		return err
	}
	return s.ws.Write(ctx, key, r)
}

// List returns only the keys the user may read, so a scoped tool cannot
// observe the existence of blobs outside its grants.
func (s *scoped) List(ctx context.Context, prefix string) ([]string, error) {
	keys, err := s.ws.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	visible := keys[:0]
	for _, k := range keys {
		if s.allow(OpRead, k) == nil {
			visible = append(visible, k)
		}
	}
	return visible, nil
}

func (s *scoped) Delete(ctx context.Context, key string) error {
	if err := s.allow(OpDelete, key); err != nil {
		return err
	}
	return s.ws.Delete(ctx, key)
}
