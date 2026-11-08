package controlplane

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/urmzd/saige/sandbox/metrics"
	"github.com/urmzd/saige/sandbox/node"
	"github.com/urmzd/saige/sandbox/sandbox"
	"github.com/urmzd/saige/sandbox/task"
)

// labelDeployment is the metrics label naming a deployment.
const labelDeployment = "deployment"

const (
	// DefaultBacklog is how many tasks may wait for a free node in one
	// deployment before SubmitAsync fails with ErrFull.
	DefaultBacklog = 1024
	// DefaultResultRetention is how many finished results each deployment
	// keeps for Result and Submit; older ones are forgotten first.
	DefaultResultRetention = 1024
)

// Memory is an in-process ControlPlane. Each deployment is a bounded pool:
// Replicas nodes from a node.Factory, each running at most one task at a
// time, and a FIFO backlog of tasks waiting for one. A goroutine exists only
// while a node has work, so an idle deployment costs nothing but its nodes.
//
// Tasks run under a context of the control plane's own, never the
// submitter's, so a task outlives the call that submitted it; Close is what
// cancels it. Nothing is persisted: a task accepted by SubmitAsync runs once
// unless the process exits or Close fails it first.
type Memory struct {
	factory node.Factory
	rec     metrics.Recorder
	backlog int
	retain  int

	base   context.Context // every task runs under it
	cancel context.CancelFunc
	wg     sync.WaitGroup // one per goroutine running a node's tasks

	mu          sync.Mutex
	closed      bool
	deployments map[string]*deployment
}

// Option configures a Memory.
type Option func(*Memory)

// WithBacklog sets how many tasks may wait for a free node in one
// deployment; beyond that SubmitAsync fails with ErrFull. n <= 0 keeps
// DefaultBacklog.
func WithBacklog(n int) Option {
	return func(m *Memory) {
		if n > 0 {
			m.backlog = n
		}
	}
}

// WithResultRetention sets how many finished results each deployment keeps;
// Result fails with ErrUnknownTask for one it has forgotten. n <= 0 keeps
// DefaultResultRetention.
func WithResultRetention(n int) Option {
	return func(m *Memory) {
		if n > 0 {
			m.retain = n
		}
	}
}

type deployment struct {
	spec ServiceSpec
	pdp  sandbox.PDP
	want int // the node count it is sized to

	nodes   []*slot          // every node held, in creation order
	idle    []*slot          // the free ones, longest free first; empty while tasks are pending
	pending []task.Task      // the backlog, oldest first
	tasks   map[string]*call // waiting, running, and retained finished tasks
	done    []string         // finished task IDs, oldest first, for retention
}

// slot is one node and whether it is running a task.
type slot struct {
	node node.Node
	busy bool
}

// call is one task's outcome, once done is closed.
type call struct {
	res  task.Result
	done chan struct{}
}

var _ ControlPlane = (*Memory)(nil)

// NewMemory returns an empty control plane that creates nodes with factory
// and records metrics to rec (nil, or metrics.Nop(), disables them).
func NewMemory(factory node.Factory, rec metrics.Recorder, opts ...Option) *Memory {
	if rec == nil {
		rec = metrics.Nop()
	}
	base, cancel := context.WithCancel(context.Background())
	m := &Memory{
		factory:     factory,
		rec:         rec,
		backlog:     DefaultBacklog,
		retain:      DefaultResultRetention,
		base:        base,
		cancel:      cancel,
		deployments: make(map[string]*deployment),
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Deploy implements ControlPlane.
func (m *Memory) Deploy(ctx context.Context, spec ServiceSpec) error {
	if spec.Name == "" {
		return fmt.Errorf("controlplane: deployment name required")
	}
	pdp, err := spec.PDP()
	if err != nil {
		return err
	}
	replicas := spec.Replicas
	switch {
	case replicas == 0:
		replicas = 1
	case replicas < 0:
		replicas = 0 // tasks wait until Scale adds nodes
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if _, exists := m.deployments[spec.Name]; exists {
		return fmt.Errorf("%w: %q", ErrExists, spec.Name)
	}
	d := &deployment{spec: spec, pdp: pdp, tasks: make(map[string]*call)}
	if _, err := m.resize(ctx, d, replicas); err != nil {
		// d holds no work yet, so its nodes can go at once.
		for _, s := range d.nodes {
			_ = s.node.Close(ctx)
		}
		return err
	}
	m.deployments[spec.Name] = d
	return nil
}

// Scale implements ControlPlane. Growing starts nodes at once and hands them
// waiting tasks. Shrinking closes free nodes at once; a busy node finishes
// its task and then retires, so Scale never cancels work.
func (m *Memory) Scale(ctx context.Context, name string, replicas int) error {
	if replicas < 0 {
		return fmt.Errorf("controlplane: replicas must be >= 0, got %d", replicas)
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	d, ok := m.deployments[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	retired, err := m.resize(ctx, d, replicas)
	m.mu.Unlock()
	for _, n := range retired {
		if cerr := n.Close(ctx); cerr != nil && err == nil {
			err = fmt.Errorf("controlplane: scale %q: close node %s: %w", name, n.ID(), cerr)
		}
	}
	return err
}

// resize sizes d to want nodes and returns the free nodes it retired, for
// the caller to close once m.mu is released. Callers hold m.mu.
func (m *Memory) resize(ctx context.Context, d *deployment, want int) ([]node.Node, error) {
	d.want = want
	defer m.gauges(d)
	name := d.spec.Name
	for len(d.nodes) < want {
		n, err := m.factory.New(ctx, node.Spec{
			Deployment: name,
			PDP:        d.pdp,
			Spawn: func(ctx context.Context, t task.Task) (string, error) {
				return m.SubmitAsync(ctx, name, t)
			},
		})
		if err != nil {
			return nil, fmt.Errorf("controlplane: scale %q: %w", name, err)
		}
		s := &slot{node: n}
		d.nodes = append(d.nodes, s)
		if t, ok, _ := m.dispatch(d, s); ok {
			m.start(d, s, t)
		}
	}
	var retired []node.Node
	for len(d.nodes) > want && len(d.idle) > 0 {
		s := d.idle[len(d.idle)-1]
		d.idle = d.idle[:len(d.idle)-1]
		d.remove(s)
		retired = append(retired, s.node)
	}
	return retired, nil
}

// dispatch decides what free node s does next: retire if d holds more nodes
// than it is sized to, else take the oldest waiting task, else wait idle.
// ok reports a task to run; retired is a node for the caller to close once
// m.mu is released. Callers hold m.mu.
func (m *Memory) dispatch(d *deployment, s *slot) (t task.Task, ok bool, retired node.Node) {
	if len(d.nodes) > d.want {
		d.remove(s)
		return task.Task{}, false, s.node
	}
	if len(d.pending) > 0 {
		t = d.pending[0]
		d.pending[0] = task.Task{}
		d.pending = d.pending[1:]
		s.busy = true
		return t, true, nil
	}
	s.busy = false
	d.idle = append(d.idle, s)
	return task.Task{}, false, nil
}

// start runs t on s, then whatever dispatch hands s next, on a goroutine
// of its own. Callers hold m.mu and have checked m.closed.
func (m *Memory) start(d *deployment, s *slot, t task.Task) {
	s.busy = true
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for {
			res := m.execute(d.spec.Name, s.node, t)
			m.mu.Lock()
			d.finish(t.ID, res, m.retain)
			next, ok, retired := m.dispatch(d, s)
			m.gauges(d)
			m.mu.Unlock()
			if retired != nil {
				// A retiring node has no caller to report to; the
				// in-process node cannot fail to close.
				_ = retired.Close(m.base)
			}
			if !ok {
				return
			}
			t = next
		}
	}()
}

// execute runs one task on n and records its metrics. A node error becomes
// the result's Error, so every task ends with a result.
func (m *Memory) execute(deployment string, n node.Node, t task.Task) task.Result {
	labels := []metrics.Label{
		{Key: labelDeployment, Value: deployment},
		{Key: "tool", Value: t.Tool},
	}
	start := time.Now()
	res, err := n.Run(m.base, t)
	m.rec.Observe("sandbox_task_seconds", time.Since(start).Seconds(), labels...)
	if err != nil {
		res = task.Result{NodeID: n.ID(), Error: err.Error()}
	}
	res.TaskID = t.ID
	status := "ok"
	if res.Error != "" {
		status = "error"
	}
	m.rec.Count("sandbox_tasks_total", 1, append(labels, metrics.Label{Key: "status", Value: status})...)
	return res
}

// SubmitAsync implements ControlPlane. A task runs at once on a free node,
// or waits in the deployment's backlog; a full backlog fails with ErrFull.
// A task ID the caller sets must not belong to a task the deployment still
// knows.
func (m *Memory) SubmitAsync(ctx context.Context, name string, t task.Task) (string, error) {
	id, _, err := m.submit(ctx, name, t)
	return id, err
}

// submit is SubmitAsync, also returning the task's call so Submit can wait
// on it even after the deployment forgets it.
func (m *Memory) submit(ctx context.Context, name string, t task.Task) (string, *call, error) {
	if t.Tool == "" {
		return "", nil, fmt.Errorf("controlplane: task tool required")
	}
	if err := ctx.Err(); err != nil {
		return "", nil, fmt.Errorf("controlplane: submit to %q: %w", name, err)
	}
	if t.ID == "" {
		t.ID = task.NewID()
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", nil, ErrClosed
	}
	d, ok := m.deployments[name]
	if !ok {
		m.mu.Unlock()
		return "", nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if _, dup := d.tasks[t.ID]; dup {
		m.mu.Unlock()
		return "", nil, fmt.Errorf("controlplane: submit to %q: task %s already submitted", name, t.ID)
	}
	if len(d.idle) == 0 && len(d.pending) >= m.backlog {
		m.mu.Unlock()
		return "", nil, fmt.Errorf("controlplane: submit to %q: %w (%d tasks waiting)", name, ErrFull, len(d.pending))
	}
	c := &call{done: make(chan struct{})}
	d.tasks[t.ID] = c
	if len(d.idle) > 0 {
		// The node free the longest, so work spreads across the pool.
		s := d.idle[0]
		d.idle = d.idle[1:]
		m.start(d, s, t)
	} else {
		d.pending = append(d.pending, t)
	}
	m.gauges(d)
	m.mu.Unlock()

	m.rec.Count("sandbox_tasks_submitted_total", 1,
		metrics.Label{Key: labelDeployment, Value: name},
		metrics.Label{Key: "tool", Value: t.Tool})
	return t.ID, c, nil
}

// Submit implements ControlPlane.
func (m *Memory) Submit(ctx context.Context, name string, t task.Task) (task.Result, error) {
	id, c, err := m.submit(ctx, name, t)
	if err != nil {
		return task.Result{}, err
	}
	select {
	case <-c.done:
	case <-ctx.Done():
		return task.Result{}, fmt.Errorf("controlplane: await %q task %s: %w", name, id, ctx.Err())
	}
	if c.res.Error != "" {
		return c.res, fmt.Errorf("controlplane: task %s: %s", id, c.res.Error)
	}
	return c.res, nil
}

// Result implements ControlPlane.
func (m *Memory) Result(_ context.Context, name, taskID string) (task.Result, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.deployments[name]
	if !ok {
		return task.Result{}, false, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	c, ok := d.tasks[taskID]
	if !ok {
		return task.Result{}, false, fmt.Errorf("%w: %s in %q", ErrUnknownTask, taskID, name)
	}
	select {
	case <-c.done:
		return c.res, true, nil
	default:
		return task.Result{}, false, nil
	}
}

// Spec implements ControlPlane.
func (m *Memory) Spec(_ context.Context, name string) (ServiceSpec, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.deployments[name]
	if !ok {
		return ServiceSpec{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return d.spec, nil
}

// Status implements ControlPlane.
func (m *Memory) Status(ctx context.Context, name string) (DeploymentStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.deployments[name]
	if !ok {
		return DeploymentStatus{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return d.status(ctx), nil
}

// List implements ControlPlane.
func (m *Memory) List(ctx context.Context) ([]DeploymentStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]DeploymentStatus, 0, len(m.deployments))
	for _, d := range m.deployments {
		out = append(out, d.status(ctx))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Close shuts the control plane down. It refuses new work, fails every task
// still waiting for a node with ErrClosed, cancels the context running tasks
// were given, and waits for them to return or for ctx to end, whichever
// comes first. Once they have returned it closes every node. If ctx ends
// first, calling Close again waits again.
func (m *Memory) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	for _, d := range m.deployments {
		for _, t := range d.pending {
			d.finish(t.ID, task.Result{TaskID: t.ID, Error: ErrClosed.Error()}, m.retain)
		}
		d.pending = nil
		m.gauges(d)
	}
	m.mu.Unlock()

	m.cancel()
	returned := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(returned)
	}()
	select {
	case <-returned:
	case <-ctx.Done():
		return fmt.Errorf("controlplane: close: %w", ctx.Err())
	}

	m.mu.Lock()
	var nodes []node.Node
	for _, d := range m.deployments {
		for _, s := range d.nodes {
			nodes = append(nodes, s.node)
		}
		d.nodes, d.idle = nil, nil
		m.gauges(d)
	}
	m.mu.Unlock()
	var errs []error
	for _, n := range nodes {
		if err := n.Close(ctx); err != nil {
			errs = append(errs, fmt.Errorf("controlplane: close node %s: %w", n.ID(), err))
		}
	}
	return errors.Join(errs...)
}

// gauges records d's node and backlog sizes. Callers hold m.mu.
func (m *Memory) gauges(d *deployment) {
	l := metrics.Label{Key: labelDeployment, Value: d.spec.Name}
	m.rec.Gauge("sandbox_nodes", float64(len(d.nodes)), l)
	m.rec.Gauge("sandbox_tasks_pending", float64(len(d.pending)), l)
}

// finish records a task's result, wakes whoever waits on it, and forgets
// the oldest results beyond retain. Callers hold the control plane's lock.
func (d *deployment) finish(id string, res task.Result, retain int) {
	c, ok := d.tasks[id]
	if !ok {
		return
	}
	c.res = res
	close(c.done)
	d.done = append(d.done, id)
	for len(d.done) > retain {
		delete(d.tasks, d.done[0])
		d.done[0] = ""
		d.done = d.done[1:]
	}
}

// remove drops s from d's nodes. Callers hold the control plane's lock and
// have already taken s out of d.idle.
func (d *deployment) remove(s *slot) {
	for i, x := range d.nodes {
		if x == s {
			d.nodes = append(d.nodes[:i], d.nodes[i+1:]...)
			return
		}
	}
}

// status builds a DeploymentStatus. Callers hold the control plane's lock.
func (d *deployment) status(ctx context.Context) DeploymentStatus {
	nodes := make([]NodeStatus, len(d.nodes))
	for i, s := range d.nodes {
		nodes[i] = NodeStatus{ID: s.node.ID(), Health: string(s.node.Health(ctx)), Busy: s.busy}
	}
	return DeploymentStatus{Name: d.spec.Name, Replicas: d.want, Nodes: nodes, Pending: len(d.pending)}
}
