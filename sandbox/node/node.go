// Package node defines the agent execution node: the unit that runs tasks
// by calling tools. The interfaces here say nothing about how a node runs
// its tools (in process today), so the execution substrate can change
// without touching the control plane.
package node

import (
	"context"

	"github.com/urmzd/saige/sandbox/sandbox"
	"github.com/urmzd/saige/sandbox/task"
)

// Health is a node's reported condition.
type Health string

// A node is ready to take a task until it stops.
const (
	Ready   Health = "ready"
	Stopped Health = "stopped"
)

// Node is one agent execution node.
type Node interface {
	// ID uniquely identifies the node within its deployment.
	ID() string
	// Run executes t and returns its result. Implementations must be safe
	// for concurrent Run calls.
	Run(ctx context.Context, t task.Task) (task.Result, error)
	// Health reports the node's current condition.
	Health(ctx context.Context) Health
	// Close releases the node's resources. A closed node reports Stopped
	// and rejects Run.
	Close(ctx context.Context) error
}

// Spawner submits a sub-task back into the node's deployment and returns its
// task ID. The control plane injects it when creating nodes, and it submits
// to the same deployment's pool. Tools only reach it through their policy's
// spawn allowlist.
type Spawner func(ctx context.Context, t task.Task) (string, error)

// Spec describes the nodes a Factory should produce for one deployment.
type Spec struct {
	// Deployment is the owning deployment's name; node IDs derive from it.
	Deployment string
	// PDP decides every workspace operation and spawn attempt the node's
	// tools make. It is compiled from the deployment's access definition
	// (flat policies or an NGAC spec); nil denies everything.
	PDP sandbox.PDP
	// Spawn is how this node's tools submit sub-tasks. Nil disables
	// spawning entirely.
	Spawn Spawner
}

// Factory creates nodes. The control plane calls it when a deployment
// grows; it is the seam where new execution substrates plug in.
type Factory interface {
	New(ctx context.Context, spec Spec) (Node, error)
}
