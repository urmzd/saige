// Package controlplane defines the deployment surface of playscape: deploy a
// service, size its pool of agent execution nodes, submit tasks to it, and
// observe the result. It is the composition root: the only package that
// knows about nodes, sandbox policies, and metrics together; each of those
// stays ignorant of the others.
//
// Execution is in-process. Each deployment owns a bounded pool of nodes,
// each running one task at a time, and a bounded backlog of tasks waiting
// for a free node. Nothing is persisted: a task lives as long as the
// process does. Durable, retried execution belongs to the caller (in the
// stack, durable runs wrap the tasks), so this package does not pretend to
// it.
package controlplane

import (
	"context"
	"errors"

	"github.com/urmzd/saige/sandbox/ngac"
	"github.com/urmzd/saige/sandbox/sandbox"
	"github.com/urmzd/saige/sandbox/task"
)

var (
	// ErrNotFound is returned when a deployment name is unknown.
	ErrNotFound = errors.New("controlplane: deployment not found")
	// ErrExists is returned when deploying a name that is already taken.
	ErrExists = errors.New("controlplane: deployment already exists")
	// ErrUnknownTask is returned by Result for a task ID the deployment
	// was never given, or whose result it no longer keeps.
	ErrUnknownTask = errors.New("controlplane: unknown task")
	// ErrFull is returned by SubmitAsync when a deployment's backlog of
	// tasks waiting for a node is full.
	ErrFull = errors.New("controlplane: backlog full")
	// ErrClosed is returned once the control plane has been closed, and is
	// the error of every task it failed while closing.
	ErrClosed = errors.New("controlplane: closed")
)

// ServiceSpec declares one service: a set of tools, the capabilities each is
// granted, and how many of its tasks may run at once.
type ServiceSpec struct {
	// Name uniquely identifies the deployment.
	Name string `json:"name"`
	// Replicas is the initial node count, which is how many of the
	// deployment's tasks run at once. Zero means one. A negative value
	// starts the deployment with no nodes: its tasks wait in the backlog
	// until Scale adds some.
	Replicas int `json:"replicas,omitempty"`
	// Policies grant each tool its workspace areas and spawn targets. A
	// tool not listed here executes with no workspace access and no
	// ability to spawn (default deny). Compiled into an NGAC graph;
	// mutually exclusive with Access.
	Policies []sandbox.Policy `json:"policies,omitempty"`
	// Access is the full NGAC form: define user attributes grouping
	// agents, object attributes over workspace areas and spawn targets,
	// associations, and prohibitions. Use it when flat per-tool policies
	// cannot express the relationships you need.
	Access *ngac.Spec `json:"access,omitempty"`
}

// PDP compiles the spec's access definition into the decision point that
// governs its nodes. Exactly one of Policies or Access may be set; an empty
// spec yields a deny-all PDP.
func (s ServiceSpec) PDP() (sandbox.PDP, error) {
	if s.Access != nil {
		if len(s.Policies) > 0 {
			return nil, errors.New("controlplane: define access with either policies or access, not both")
		}
		return ngac.Build(*s.Access)
	}
	return sandbox.FromPolicies(s.Policies), nil
}

// NodeStatus reports one node's identity and condition.
type NodeStatus struct {
	ID     string `json:"id"`
	Health string `json:"health"`
	// Busy reports whether the node is running a task.
	Busy bool `json:"busy"`
}

// DeploymentStatus reports a deployment's current shape.
type DeploymentStatus struct {
	Name string `json:"name"`
	// Replicas is the node count the deployment is sized to.
	Replicas int `json:"replicas"`
	// Nodes lists the nodes it holds. After a scale-down this can briefly
	// exceed Replicas: a busy node retires when its task returns.
	Nodes []NodeStatus `json:"nodes"`
	// Pending counts tasks waiting for a free node.
	Pending int `json:"pending"`
}

// ControlPlane is the surface callers drive: deploy, scale, submit, observe.
type ControlPlane interface {
	// Deploy creates a deployment from spec and starts its nodes.
	Deploy(ctx context.Context, spec ServiceSpec) error
	// Scale sets the deployment's node count.
	Scale(ctx context.Context, name string, replicas int) error
	// Submit runs t and blocks until its result arrives or ctx ends. A
	// task that fails returns its result together with an error. Ending
	// ctx stops the wait, not the task.
	Submit(ctx context.Context, name string, t task.Task) (task.Result, error)
	// SubmitAsync accepts t and returns its task ID immediately.
	SubmitAsync(ctx context.Context, name string, t task.Task) (string, error)
	// Result returns a task's result and whether it has finished. It fails
	// with ErrUnknownTask for an ID the deployment does not know.
	Result(ctx context.Context, name, taskID string) (task.Result, bool, error)
	// Spec returns the deployment's service spec, so a caller can rebuild
	// its policy decisions (for a sub-task run on its parent's node, say).
	Spec(ctx context.Context, name string) (ServiceSpec, error)
	// Status reports one deployment.
	Status(ctx context.Context, name string) (DeploymentStatus, error)
	// List reports every deployment in lexical name order.
	List(ctx context.Context) ([]DeploymentStatus, error)
}
