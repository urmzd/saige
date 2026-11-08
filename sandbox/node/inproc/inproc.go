// Package inproc provides an in-process node.Factory: each node executes
// tools on the calling goroutine, resolving them from a shared registry and
// confining each call through the deployment's policy decision point. It is
// the execution substrate controlplane.Memory runs its pools on.
package inproc

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/urmzd/saige/sandbox/node"
	"github.com/urmzd/saige/sandbox/sandbox"
	"github.com/urmzd/saige/sandbox/task"
	"github.com/urmzd/saige/sandbox/tool"
	"github.com/urmzd/saige/sandbox/workspace"
)

// Factory creates in-process nodes over a shared tool registry and
// workspace backend.
type Factory struct {
	registry *tool.Registry
	ws       workspace.Workspace
	seq      atomic.Uint64
}

// NewFactory returns a factory whose nodes resolve tools from registry and
// reach ws only through the deployment's policy decisions.
func NewFactory(registry *tool.Registry, ws workspace.Workspace) *Factory {
	return &Factory{registry: registry, ws: ws}
}

// New implements node.Factory.
func (f *Factory) New(_ context.Context, spec node.Spec) (node.Node, error) {
	return &inprocNode{
		id:       fmt.Sprintf("%s-%d", spec.Deployment, f.seq.Add(1)),
		registry: f.registry,
		ws:       f.ws,
		pdp:      spec.PDP,
		spawn:    spec.Spawn,
	}, nil
}

type inprocNode struct {
	id       string
	registry *tool.Registry
	ws       workspace.Workspace
	pdp      sandbox.PDP // nil denies everything
	spawn    node.Spawner

	mu     sync.RWMutex
	closed bool
}

func (n *inprocNode) ID() string { return n.id }

// Run implements node.Node. The executing tool is the policy-graph user:
// its workspace view and spawn attempts are decided under its own name,
// default deny.
func (n *inprocNode) Run(ctx context.Context, t task.Task) (task.Result, error) {
	n.mu.RLock()
	closed := n.closed
	n.mu.RUnlock()
	if closed {
		return task.Result{}, fmt.Errorf("node %s: stopped", n.id)
	}

	tl, ok := n.registry.Get(t.Tool)
	if !ok {
		return task.Result{}, fmt.Errorf("node %s: unknown tool %q", n.id, t.Tool)
	}
	rt := &runtime{
		user:  t.Tool,
		ws:    sandbox.ScopePDP(n.ws, t.Tool, n.pdp),
		pdp:   n.pdp,
		spawn: n.spawn,
	}

	out, err := tl.Call(ctx, rt, t.Input)
	if err != nil {
		return task.Result{}, fmt.Errorf("node %s: tool %q: %w", n.id, t.Tool, err)
	}
	return task.Result{TaskID: t.ID, NodeID: n.id, Output: out}, nil
}

// Health implements node.Node.
func (n *inprocNode) Health(context.Context) node.Health {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.closed {
		return node.Stopped
	}
	return node.Ready
}

// Close implements node.Node.
func (n *inprocNode) Close(context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.closed = true
	return nil
}

// runtime implements tool.Runtime for one tool invocation.
type runtime struct {
	user  string
	ws    workspace.Workspace
	pdp   sandbox.PDP
	spawn node.Spawner
}

func (r *runtime) Workspace() workspace.Workspace { return r.ws }

var _ tool.SpawnChecker = (*runtime)(nil)

func (r *runtime) Spawn(ctx context.Context, t task.Task) (string, error) {
	if err := r.CheckSpawn(t.Tool); err != nil {
		return "", err
	}
	return r.spawn(ctx, t)
}

// CheckSpawn implements tool.SpawnChecker: the decision Spawn makes before it
// dispatches, and nothing else.
func (r *runtime) CheckSpawn(target string) error {
	if r.spawn == nil {
		return fmt.Errorf("%w: user %q: spawning disabled on this node", sandbox.ErrDenied, r.user)
	}
	if r.pdp == nil || !r.pdp.Can(r.user, sandbox.OpSpawn, sandbox.SpawnObject(target)) {
		return fmt.Errorf("%w: user %q may not spawn %q", sandbox.ErrDenied, r.user, target)
	}
	return nil
}
