// Package sandbox runs any turn.Runtime inside playscape: each turn is a
// task in the deployment of its pinned definition, under that definition's
// policy (Policy.Spawn and Policy.WorkspaceAreas).
//
// A sandbox tool is a function over bytes: it cannot stream, and the node
// running it holds neither the caller's context nor the run's replay frame.
// So the task sends its bodies through an in-process channel registered under
// the turn's key, and the caller drains that channel on its own goroutine.
// That keeps every emit on the caller's goroutine, which durable requires.
// The handoff is in-process by construction.
//
// sandbox's queue is at-most-once. A lost task is a failed turn, and
// retrying it is the caller's job: durability is never borrowed from
// sandbox.
package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urmzd/saige/sandbox/controlplane"
	psandbox "github.com/urmzd/saige/sandbox/sandbox"
	"github.com/urmzd/saige/sandbox/task"
	"github.com/urmzd/saige/sandbox/tool"
	"github.com/urmzd/saige/sandbox/workspace"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/turn"
)

// Defaults.
const (
	// DefaultBuffer is the per-turn body buffer between the task and the
	// caller. A full buffer blocks the task: it is backpressure, never a
	// drop, because a stream with a hole in it is a different conversation.
	DefaultBuffer = 256
	// DefaultPollInterval is how often a turn checks for its task's result.
	DefaultPollInterval = 5 * time.Millisecond
)

// ErrInvalid reports a sandbox that cannot run a turn. Callers match with
// errors.Is.
var ErrInvalid = errors.New("sandbox: invalid")

// Plane is where turns run: a sandbox control plane and the tool registry
// its nodes resolve from.
type Plane struct {
	Control controlplane.ControlPlane
	Tools   *tool.Registry
}

// Option configures the sandbox.
type Option func(*sandboxed)

// WithReplicas sets how many execution nodes each deployment starts with.
func WithReplicas(n int) Option {
	return func(s *sandboxed) {
		if n > 0 {
			s.replicas = n
		}
	}
}

// WithBuffer sets the per-turn body buffer (default DefaultBuffer).
func WithBuffer(n int) Option {
	return func(s *sandboxed) {
		if n > 0 {
			s.buffer = n
		}
	}
}

// WithPollInterval sets how often a turn checks for its task's result.
func WithPollInterval(d time.Duration) Option {
	return func(s *sandboxed) {
		if d > 0 {
			s.poll = d
		}
	}
}

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option {
	return func(s *sandboxed) {
		if l != nil {
			s.logger = l
		}
	}
}

// Sandboxed returns a Runtime that runs each turn of inner as a sandbox
// task. A plane with no control plane or no registry fails every turn.
func Sandboxed(inner turn.Runtime, plane Plane, opts ...Option) turn.Runtime {
	s := &sandboxed{
		inner:      inner,
		plane:      plane,
		replicas:   1,
		buffer:     DefaultBuffer,
		poll:       DefaultPollInterval,
		logger:     slog.Default(),
		deployed:   make(map[string]string),
		registered: make(map[string]bool),
		turns:      make(map[string]*waiting),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// ToolName is the sandbox tool a definition's turns execute as. It is also
// the definition's identity in the policy graph, and what a spawn grant names.
func ToolName(agent string) string { return "agent:" + agent }

// DeploymentName is the sandbox deployment a pinned definition runs in:
// its name, version and the first 12 characters of its digest.
func DeploymentName(def turn.Pinned) string {
	name := fmt.Sprintf("fiber-%s-v%s", def.Name, def.Version)
	if def.Digest != "" {
		d := def.Digest
		if i := strings.LastIndexByte(d, ':'); i >= 0 {
			d = d[i+1:]
		}
		if len(d) > 12 {
			d = d[:12]
		}
		name += "-" + d
	}
	return name
}

type sandboxed struct {
	inner    turn.Runtime
	plane    Plane
	replicas int
	buffer   int
	poll     time.Duration
	logger   *slog.Logger

	mu         sync.Mutex
	deployed   map[string]string
	registered map[string]bool
	turns      map[string]*waiting

	keys atomic.Uint64
}

// task is what a sandbox task carries.
type taskInput struct {
	Key   string      `json:"key"`
	Def   turn.Pinned `json:"def"`
	Input turn.Input  `json:"input"`
}

// waiting is what the caller waiting on a task shares with the tool running
// it. The two never share a context, so everything that crosses goes here.
type waiting struct {
	sink    chan event.Body
	stop    <-chan turn.Stop
	compact <-chan turn.CompactRequest
	actor   event.Actor
	private event.Private
	// abort is closed when nobody waits on the turn any more. The tool
	// cancels its own context on it: the only cancellation that reaches a
	// sandbox task from outside.
	abort chan struct{}
}

type workspaceKey struct{}

// Workspace returns the workspace a sandboxed turn may use, already scoped
// to its definition's policy, and false outside a sandbox.
func Workspace(ctx context.Context) (workspace.Workspace, bool) {
	ws, ok := ctx.Value(workspaceKey{}).(workspace.Workspace)
	return ws, ok && ws != nil
}

// Turn submits the turn as a sandbox task and emits what it streams.
func (s *sandboxed) Turn(ctx context.Context, def turn.Pinned, in turn.Input, emit func(context.Context, event.Body) error) (turn.Result, error) {
	switch {
	case s.inner == nil:
		return turn.Result{}, fmt.Errorf("%w: no runtime to sandbox", ErrInvalid)
	case s.plane.Control == nil:
		return turn.Result{}, fmt.Errorf("%w: no playscape control plane", ErrInvalid)
	case s.plane.Tools == nil:
		return turn.Result{}, fmt.Errorf("%w: no playscape tool registry", ErrInvalid)
	}
	if err := def.Validate(); err != nil {
		return turn.Result{}, err
	}
	deployment, err := s.ensure(ctx, def)
	if err != nil {
		return turn.Result{}, err
	}

	key := "t" + strconv.FormatUint(s.keys.Add(1), 10)
	sink := make(chan event.Body, s.buffer)
	actor, _ := event.ActorFrom(ctx)
	s.attach(key, &waiting{
		sink:    sink,
		stop:    turn.StopSignal(ctx),
		compact: turn.CompactSignal(ctx),
		actor:   actor,
		private: in.Trigger.Private,
		abort:   make(chan struct{}),
	})
	defer s.detach(key)

	input, err := json.Marshal(taskInput{Key: key, Def: def, Input: in})
	if err != nil {
		return turn.Result{}, fmt.Errorf("sandbox: encode turn: %w", err)
	}
	id, err := s.plane.Control.SubmitAsync(ctx, deployment, task.Task{Tool: ToolName(def.Name), Input: input})
	if err != nil {
		return turn.Result{}, fmt.Errorf("sandbox: submit to %s: %w", deployment, err)
	}
	return s.stream(ctx, deployment, id, sink, emit)
}

// stream drains the turn's bodies and returns its result. It polls for the
// result as well as reading the sink, because the paths that produce no
// bodies (an unknown tool, a dead node, a lost task) must end the turn too.
// Once a result is reported every body the task sent is already buffered, so
// a non-blocking drain finishes the stream in order.
func (s *sandboxed) stream(ctx context.Context, deployment, taskID string, sink <-chan event.Body, emit func(context.Context, event.Body) error) (turn.Result, error) {
	send := func(b event.Body) error {
		if emit == nil {
			return nil
		}
		return emit(ctx, b)
	}
	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return turn.Result{}, fmt.Errorf("sandbox: task %s: %w", taskID, ctx.Err())
		case b := <-sink:
			if err := send(b); err != nil {
				return turn.Result{}, err
			}
		case <-ticker.C:
			res, done, err := s.plane.Control.Result(ctx, deployment, taskID)
			if err != nil {
				return turn.Result{}, fmt.Errorf("sandbox: task %s: result: %w", taskID, err)
			}
			if !done {
				continue
			}
			for drained := false; !drained; {
				select {
				case b := <-sink:
					if err := send(b); err != nil {
						return turn.Result{}, err
					}
				default:
					drained = true
				}
			}
			var out taskOutput
			if len(res.Output) > 0 {
				if err := json.Unmarshal(res.Output, &out); err != nil {
					return turn.Result{}, fmt.Errorf("sandbox: task %s: decode result: %w", taskID, err)
				}
			}
			if res.Error != "" {
				return out.Result, fmt.Errorf("sandbox: task %s: %s", taskID, res.Error)
			}
			if out.Error != "" {
				return out.Result, fmt.Errorf("sandbox: task %s: %s", taskID, out.Error)
			}
			return out.Result, nil
		}
	}
}

// taskOutput is a task's result. A failed turn still reports what it
// produced, so the error travels beside the result and not instead of it.
type taskOutput struct {
	Result turn.Result `json:"result"`
	Error  string      `json:"error,omitempty"`
}

// ensure deploys the service a pinned definition runs in and returns its
// name. Its policy is the definition's, and nothing more: a spawn target
// runs under its own tool name with no grants of its own.
func (s *sandboxed) ensure(ctx context.Context, def turn.Pinned) (string, error) {
	name := DeploymentName(def)
	s.mu.Lock()
	_, ok := s.deployed[name]
	s.mu.Unlock()
	if ok {
		return name, nil
	}
	s.register(ToolName(def.Name))
	policy := def.Spec.Access
	spawn := make([]string, 0, len(policy.Spawn))
	for _, target := range policy.Spawn {
		s.register(ToolName(target))
		spawn = append(spawn, ToolName(target))
	}
	areas := make([]psandbox.Area, 0, len(policy.WorkspaceAreas))
	for _, prefix := range policy.WorkspaceAreas {
		areas = append(areas, psandbox.Area{Prefix: prefix})
	}
	err := s.plane.Control.Deploy(ctx, controlplane.ServiceSpec{
		Name:     name,
		Replicas: s.replicas,
		Policies: []psandbox.Policy{{Tool: ToolName(def.Name), Areas: areas, Spawn: spawn}},
	})
	// The spec is a pure function of the pinned definition, so a deployment
	// that already exists is this one.
	if err != nil && !errors.Is(err, controlplane.ErrExists) {
		return "", fmt.Errorf("sandbox: deploy %s: %w", name, err)
	}
	s.mu.Lock()
	s.deployed[name] = name
	s.mu.Unlock()
	return name, nil
}

// register adds a definition's tool to the registry, once.
func (s *sandboxed) register(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.registered[name] {
		return
	}
	if err := s.plane.Tools.Register(tool.Func(name, s.call)); err != nil {
		// Registered by another sandbox over the same registry. A task names
		// its turn by key, and a key this sandbox did not attach is refused
		// below, so adopting the registration is safe.
		s.logger.Debug("flow/turn/sandbox: playscape tool already registered", "tool", name, "error", err)
	}
	s.registered[name] = true
}

func (s *sandboxed) attach(key string, w *waiting) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turns[key] = w
}

// detach forgets a turn and aborts its task if it still runs.
func (s *sandboxed) detach(key string) {
	s.mu.Lock()
	w := s.turns[key]
	delete(s.turns, key)
	s.mu.Unlock()
	if w != nil {
		close(w.abort)
	}
}

func (s *sandboxed) waiting(key string) *waiting {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turns[key]
}

// call is the sandbox tool: one turn of the inner runtime, inside the
// sandbox. Everything it needs travels in the task input or in the turn it
// was submitted for, never in the node's context.
func (s *sandboxed) call(ctx context.Context, rt tool.Runtime, input []byte) ([]byte, error) {
	var t taskInput
	if err := json.Unmarshal(input, &t); err != nil {
		return nil, fmt.Errorf("sandbox: decode turn: %w", err)
	}
	// A task whose turn is gone was abandoned before a node picked it up,
	// and running it would spend a model call nobody reads. A task this
	// sandbox did not submit has no turn either, and is refused the same.
	w := s.waiting(t.Key)
	if w == nil {
		return nil, fmt.Errorf("sandbox: turn %s is no longer awaited", t.Key)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func(abort, done <-chan struct{}) {
		select {
		case <-abort:
			cancel()
		case <-done:
		}
	}(w.abort, ctx.Done())

	// The turn acts for whoever its caller acts for, whatever the node's
	// context carried, so a task cannot inherit someone it was not given.
	ctx = event.WithActor(ctx, w.actor)
	ctx = turn.WithStopSignal(ctx, w.stop)
	ctx = turn.WithCompactSignal(ctx, w.compact)
	if rt != nil {
		ctx = context.WithValue(ctx, workspaceKey{}, rt.Workspace())
	}
	t.Input.Trigger.Private = w.private

	send := func(_ context.Context, b event.Body) error {
		select {
		case w.sink <- b:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	res, err := s.inner.Turn(ctx, t.Def, t.Input, send)
	out := taskOutput{Result: res}
	if err != nil {
		out.Error = err.Error()
	}
	return json.Marshal(out)
}
