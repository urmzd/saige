package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urmzd/saige/sandbox/controlplane"
	dispatchsandbox "github.com/urmzd/saige/sandbox/sandbox"
	"github.com/urmzd/saige/sandbox/task"
	"github.com/urmzd/saige/sandbox/tool"
	"github.com/urmzd/saige/sandbox/workspace"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
	"github.com/urmzd/saige/flow/definition"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/isolation"
	turnrt "github.com/urmzd/saige/flow/turn"
	"github.com/urmzd/saige/flow/turn/native"

	saigetypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/flow/attachment"
)

// Sandbox defaults.
const (
	// DefaultBuffer is the per-turn chunk buffer between the tool producing
	// events and the activity publishing them.
	DefaultBuffer = 256
	// DefaultPollInterval is how often a turn checks whether its sandbox
	// task has reported a result.
	DefaultPollInterval = 5 * time.Millisecond
	// AnswerArea is the workspace area a turn writes its answer artifact
	// under. A revision that does not grant it simply has no artifact: the
	// policy decides, and the denial is reported rather than swallowed.
	AnswerArea = "runs"
)

// Catalog builds the tool catalog for one turn.
//
// action is called for every connector action the agent invokes; the Sandbox
// turns each call into a run.action.taken.v1 event on the run's stream, so
// "the work got done" lands on the bus next to the conversation that decided to
// do it.
type Catalog func(req Request, action func(event.Action) error) (native.Tools, error)

// Connectors returns a Catalog resolving tool names against the actions every
// registered connector contributes, bound to the tag's own surface.
func Connectors(reg *connector.Registry) Catalog {
	return func(req Request, action func(event.Action) error) (native.Tools, error) {
		target, err := req.SourceAddress()
		if err != nil {
			return nil, err
		}
		return native.ConnectorToolsForRun(reg, target, req.RunID, action), nil
	}
}

// SourceAddress parses the request's origin surface.
func (r Request) SourceAddress() (address.Address, error) {
	if r.Source == "" {
		return address.Address{}, nil
	}
	a, err := address.Parse(r.Source)
	if err != nil {
		return address.Address{}, fmt.Errorf("runtime: run %s: source: %w", r.RunID, err)
	}
	return a, nil
}

// Sandbox executes turns as sandbox tasks, under the NGAC policy compiled from
// the pinned revision's Access grant.
//
// # What the sandbox is for
//
// A turn is arbitrary model-directed code: it calls tools, writes files, and may
// delegate to other agents. Access says what the agent may touch, and saying it
// is worth nothing unless something enforces it. sandbox is that something: the
// tool never receives the shared workspace, only a view scoped to its policy, and
// every spawn attempt is checked against the policy's allowlist before it
// reaches a queue. Default is deny, so an agent granted nothing can do nothing.
//
// # One deployment per pinned revision
//
// The policy lives on the deployment, and a run pins a revision, so the
// deployment is keyed by (agent, revision): revising an agent's Access cannot
// retroactively widen or narrow what a run already in flight is allowed to do.
// The TOOL, in contrast, is keyed by agent name alone, because Access.Spawn names
// agents and a spawn allowlist has to name something an agent can know without
// knowing which revision of it is current.
//
// # Streaming out of a sandbox
//
// A sandbox tool is a function over bytes: it has no way to stream, and the node
// running it holds neither the caller's context nor the run's replay frame. So
// the tool sends its chunks through an in-process channel registered under the
// run id, and the activity that submitted the task drains that channel on its own
// goroutine — which is what keeps every durable call (Emit, and the ledger
// writes behind it) on the single goroutine durable requires, while the agent
// loop's own parallelism stays inside the task.
//
// That handoff is in-process by construction. A remote sandbox node would have
// to publish to the bus itself; nothing here pretends otherwise.
//
// # At-most-once, on purpose
//
// sandbox's queue is at-most-once in beta. A lost task is a failed attempt, and
// the attempt is what durable already knows how to repeat. Durability is never
// borrowed from sandbox.
type Sandbox struct {
	plane   controlplane.ControlPlane
	tools   *tool.Registry
	defs    Definitions
	catalog Catalog
	// groups supplies tool groups per turn ahead of the catalog's own; nil
	// leaves them to the catalog.
	groups ToolGroupSource
	// delegator, when set, carries out a delegation for a turn that is a
	// durable run, in place of the in-turn sub-task.
	delegator      Delegator
	callTool       string
	sessionRuntime func() *Runtime
	// instances gives each turn its claim on an isolated instance; nil
	// gives none, and tools that need one refuse.
	instances RunInstances

	agentOpts []native.Option
	knowledge func(tenant string, sources []string) native.Retriever
	replicas  int
	buffer    int
	poll      time.Duration
	logger    *slog.Logger

	mu         sync.Mutex
	deployed   map[string]string // "<agent>@<rev>" -> deployment name
	registered map[string]bool   // playscape tool name -> registered
	turns      map[string]*turn  // run id or sub-task key -> the turn someone is waiting on

	// subs numbers delegated sub-tasks, so each has a key to be cancelled by.
	subs atomic.Uint64
}

// taskInput is what a sandbox task carries: the turn's Request, plus the
// sandbox's own bookkeeping, which is not part of a Request because nothing
// outside this file should ever see it. The Request's fields are embedded, so
// the task input of a durable run is the Request's JSON as it always was.
type taskInput struct {
	Request
	// Sub is the key a delegated sub-task's turn is registered under, so the
	// turn waiting on it can cancel it. Empty for a durable run, whose run id
	// is its key.
	Sub string `json:"sandbox_sub,omitempty"`
}

// turn is what the goroutine waiting on a sandbox task shares with the tool
// executing it. The two never share a context (see call), so everything that
// has to cross between them crosses here.
type turn struct {
	// sink carries the turn's chunks out. For a delegated sub-task, which has
	// no stream of its own, only its usage is relayed from it onto its
	// parent's stream; the rest is dropped.
	sink chan event.Body
	// stop asks the turn to wrap up. Nil for a turn that can only be
	// cancelled.
	stop <-chan native.Stop
	// compact carries requests to compact the turn's history. Nil for a
	// turn nobody can ask.
	compact <-chan native.CompactRequest
	// holds reads the run's holds. Nil for a turn nobody can hold.
	holds turnrt.Holds
	// abort is closed when nobody is waiting on the turn any more: its
	// caller returned, was cancelled, or gave up on a stop. The tool cancels
	// its own context on it, which is the only cancellation that reaches a
	// sandbox task from outside.
	abort chan struct{}
	// stopped is the stop request the turn was handed, once it has been: how
	// a delegation the stop cancels learns that it was a stop.
	stopped atomic.Pointer[native.Stop]
	// childUsage checks and reads a child run a Delegator reports, from the
	// runtime executing the turn. Nil when the runtime offered none.
	childUsage func(context.Context, string) (event.UsageChild, error)
	// childQuestion checks and reads a child run a Delegator watches for
	// questions (WatchChildQuestions). Nil when the runtime offered none.
	childQuestion func(context.Context, string) (event.ChildParked, bool, bool, error)
	// actor is who the turn acts for, from the context the runtime executed
	// it under: the actor on the run's durable record, or a delegated
	// sub-task's parent's. The task never carries it, so nothing the task
	// input says (and nothing a model wrote into it) can name someone else.
	actor event.Actor
}

// SandboxOption configures a Sandbox.
type SandboxOption func(*Sandbox)

// WithCatalog sets how a spec's tool names are resolved. Without one, a spec
// that names a tool fails to build, which is the safe default: an agent silently
// missing its tools answers the wrong question.
func WithCatalog(c Catalog) SandboxOption {
	return func(s *Sandbox) {
		if c != nil {
			s.catalog = c
		}
	}
}

// ToolGroupSource supplies the members of a tool group grant, such as
// "mcp:github" or "mcp:github:write", for one tenant. *mcptools.Grants is the
// store-backed one: it expands a grant to the MCP provider's accepted tools
// under the grant's class.
//
// Expand is called under the turn's context, which carries the actor the run
// acts for (event.ActorFrom): the actor on the tag in the run's durable
// record, the same one approvals are decided on, or a delegated sub-task's
// parent's. A source that reaches a provider as a person, with that person's
// own credential, reads it there and nowhere else. A turn that acts for
// nobody (a schedule, a tag with no actor) carries none.
type ToolGroupSource interface {
	// Expand returns the tools grant stands for in tenant. ok is false for
	// a group this source does not supply, which is then asked of the
	// catalog. An error means the group could not be expanded now.
	Expand(ctx context.Context, tenant, grant string) (tools []saigetypes.Tool, ok bool, err error)
}

// WithToolGroupSource supplies a turn's tool groups from src, for the turn's
// tenant and under its context. It feeds the one group mechanism
// pkg/turn/native has: the turn's catalog answers native's ToolGroups face from
// src first and from the deployment catalog's own groups otherwise, and the
// runner registers the members, never the group. A group src cannot expand
// reaches the runner as native.ErrNoSuchToolGroup, which drops it as
// unavailable for that turn (or fails the build under native.WithStrictTools),
// as any unresolvable group does.
func WithToolGroupSource(src ToolGroupSource) SandboxOption {
	return func(s *Sandbox) {
		if src != nil {
			s.groups = src
		}
	}
}

// Delegator carries out one delegation: parent's turn asked target to do text.
// It returns the answer the parent's model reads as the tool's result.
//
// It is how a host makes a delegated turn a run in its own right, with an id, a
// stream and a record, where the default makes it a sub-task of the parent's
// turn whose only trace is the tool result. The host decides what that run is
// and how it is linked to parent; the runtime only stops answering the
// delegation itself.
//
// report puts an action on the parent's stream, so the host can say which run
// it started before that run has an answer.
//
// The spawn allowlist has already let the tool exist: a Delegator is called
// only for a target the parent's pinned revision names in Access.Spawn. A host
// with limits of its own enforces them here and returns the refusal as an
// error, which the model sees as the tool failing.
type Delegator func(ctx context.Context, parent Request, target, text string, report func(event.Action) error) (string, error)

// WithDelegator sets who carries out a delegation made by a turn that is a
// durable run. A turn that is itself a sub-task has no run to hang another one
// from, so it keeps delegating by sub-task. Without a Delegator every
// delegation is a sub-task, which is what it always was.
func WithDelegator(d Delegator) SandboxOption {
	return func(s *Sandbox) { s.delegator = d }
}

// RunInstances returns the claim on an isolated instance one turn of req
// makes (isolation.Run). It is called once per turn, for a durable run and
// for a delegated sub-task alike, so a called run never shares its caller's
// instance. ctx is the turn's and carries its actor. The turn's tools find
// the claim on their context (isolation.RunFrom), and the sandbox releases it
// when the turn ends, however it ends.
type RunInstances func(ctx context.Context, req Request) *isolation.Run

// WithRunInstances sets how a turn claims the instance its tools execute in.
func WithRunInstances(f RunInstances) SandboxOption {
	return func(s *Sandbox) { s.instances = f }
}

// WithCallTool replaces per-agent tag tools with one host catalog tool for
// agents granted Access.Spawn. The host must enforce the pinned spawn grant.
func WithCallTool(name string) SandboxOption {
	return func(s *Sandbox) { s.callTool = name }
}

// WithAgentOptions passes options through to every native.Runner the sandbox
// builds: the provider table, an offline script, a retriever, loop bounds.
func WithAgentOptions(opts ...native.Option) SandboxOption {
	return func(s *Sandbox) { s.agentOpts = append(s.agentOpts, opts...) }
}

// WithKnowledge grounds turns in a knowledge store.
//
// It is a factory rather than a retriever because retrieval is scoped twice
// over and neither scope is known until a run starts: to the tenant the run
// belongs to, and to the sources its pinned revision declares. One retriever
// shared by every run would have to pick a tenant, and there is no right one to
// pick. The factory may return nil, which means this run retrieves nothing.
func WithKnowledge(f func(tenant string, sources []string) native.Retriever) SandboxOption {
	return func(s *Sandbox) { s.knowledge = f }
}

// WithReplicas sets how many local execution nodes each deployment starts with.
func WithReplicas(n int) SandboxOption {
	return func(s *Sandbox) {
		if n > 0 {
			s.replicas = n
		}
	}
}

// WithBuffer sets the per-turn chunk buffer (default DefaultBuffer). It is
// backpressure, not a drop policy: a full buffer blocks the agent loop until the
// publisher catches up, because dropping an event from the middle of a stream
// would leave a sink rendering a conversation with a hole in it.
func WithBuffer(n int) SandboxOption {
	return func(s *Sandbox) {
		if n > 0 {
			s.buffer = n
		}
	}
}

// WithPollInterval sets how often a turn checks for its task's result.
func WithPollInterval(d time.Duration) SandboxOption {
	return func(s *Sandbox) {
		if d > 0 {
			s.poll = d
		}
	}
}

// WithSandboxLogger sets the logger.
func WithSandboxLogger(l *slog.Logger) SandboxOption {
	return func(s *Sandbox) {
		if l != nil {
			s.logger = l
		}
	}
}

// NewSandbox returns a turn runtime over a sandbox control plane.
//
// tools is the registry the plane's nodes resolve from; the sandbox registers one
// tool per agent into it as agents are first seen. defs is used only to resolve
// a delegation target's definition; passing nil disables delegation with an error
// the model can read.
func NewSandbox(plane controlplane.ControlPlane, tools *tool.Registry, defs Definitions, opts ...SandboxOption) (*Sandbox, error) {
	if plane == nil {
		return nil, fmt.Errorf("%w: no playscape control plane", ErrInvalid)
	}
	if tools == nil {
		return nil, fmt.Errorf("%w: no playscape tool registry", ErrInvalid)
	}
	s := &Sandbox{
		plane:      plane,
		tools:      tools,
		defs:       defs,
		replicas:   1,
		buffer:     DefaultBuffer,
		poll:       DefaultPollInterval,
		logger:     slog.Default(),
		deployed:   make(map[string]string),
		registered: make(map[string]bool),
		turns:      make(map[string]*turn),
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

var _ turnrt.Runtime = (*Sandbox)(nil)

// Turn runs one turn the runtime handed it (see RequestFrom) as a sandbox
// task. A turn with no request on its context is refused: the sandbox's
// grants come from the pinned policy on the request, and nothing else.
func (s *Sandbox) Turn(ctx context.Context, def turnrt.Pinned, in turnrt.Input, emit func(context.Context, event.Body) error) (turnrt.Result, error) {
	req, ok := RequestFrom(ctx)
	if !ok {
		return turnrt.Result{}, fmt.Errorf("%w: the sandbox runs only turns a fiber runtime hands it", ErrInvalid)
	}
	if def.Name != req.Definition.Name || def.Version != req.Definition.Version {
		return turnrt.Result{}, fmt.Errorf("%w: turn of %s@%s carries the request of %s@%s", ErrInvalid,
			def.Name, def.Version, req.Definition.Name, req.Definition.Version)
	}
	out, err := s.execute(ctx, req, emit)
	report(ctx, out)
	res := turnrt.Result{Text: out.Text, Prompt: out.Prompt, Citations: out.Citations, Stopped: out.Stopped}
	if err != nil {
		return res, err
	}
	if out.Parked != nil {
		p, perr := native.ParkedTo(*out.Parked)
		if perr != nil {
			return res, perr
		}
		res.Parked = &p
	}
	return res, nil
}

// ToolName is the sandbox tool an agent's turns execute as. It is also the
// agent's identity in the policy graph, and the object a spawn grant names.
func ToolName(agent string) string { return "agent:" + agent }

// DeploymentName is the sandbox deployment a pinned definition runs in: its
// name, version and the first 12 characters of the pin's hash, which covers
// the policy the deployment's grants are compiled from.
func DeploymentName(pin definition.Pin) string {
	h := pin.Hash
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		h = h[i+1:]
	}
	if len(h) > 12 {
		h = h[:12]
	}
	return fmt.Sprintf("fiber-%s-v%s-%s", pin.Name, pin.Version, h)
}

// DelegateToolName is the tool an agent calls to tag another agent.
func DelegateToolName(agent string) string { return "tag_" + agent }

// AnswerKey is where a run's answer artifact is written.
func AnswerKey(agent, runID string) string {
	return path.Join(AnswerArea, agent, runID, "answer")
}

// execute submits the turn as a sandbox task and publishes what it streams.
func (s *Sandbox) execute(ctx context.Context, req Request, emit func(context.Context, event.Body) error) (Outcome, error) {
	if req.RunID == "" {
		return Outcome{}, fmt.Errorf("%w: turn has no run id", ErrInvalid)
	}
	deployment, err := s.ensure(ctx, req)
	if err != nil {
		return Outcome{}, err
	}
	req.Service = deployment

	sink := make(chan event.Body, s.buffer)
	// A stop request reaches the tool as a channel, never as a call into this
	// type: the channel holds the request until the turn reads it, so a
	// request that arrives before the task has been picked up is not lost to
	// timing.
	childQuestion, _ := ctx.Value(childQuestionKey{}).(func(context.Context, string) (event.ChildParked, bool, bool, error))
	actor, _ := event.ActorFrom(ctx)
	if err := s.attach(req.RunID, &turn{sink: sink, stop: StopSignal(ctx), compact: CompactSignal(ctx), holds: turnrt.HoldsFrom(ctx), abort: make(chan struct{}), childUsage: childUsageOf(ctx), childQuestion: childQuestion, actor: actor}); err != nil {
		return Outcome{}, err
	}
	// Detaching aborts the task if it is still running, which it is on every
	// return below that is not the task's own result.
	defer s.detach(req.RunID)

	input, err := json.Marshal(taskInput{Request: req})
	if err != nil {
		return Outcome{}, fmt.Errorf("runtime: run %s: marshal request: %w", req.RunID, err)
	}
	id, err := s.plane.SubmitAsync(ctx, deployment, task.Task{Tool: ToolName(req.Agent()), Input: input})
	if err != nil {
		return Outcome{}, fmt.Errorf("runtime: run %s: submit to %s: %w", req.RunID, deployment, err)
	}

	return s.stream(ctx, req, deployment, id, sink, emit)
}

// stream drains the turn's chunks and returns its outcome.
//
// The loop watches the sink and polls for the task's result rather than waiting
// for the tool to close anything, because the paths that produce no chunks at all
// — an unknown tool, a node that died, a task the at-most-once queue lost — must
// end the turn too. Once a result has been reported, every chunk the tool sent is
// already in the buffer (it sent them all before returning, and a full buffer
// would have blocked it), so a non-blocking drain finishes the stream in order
// with nothing lost.
func (s *Sandbox) stream(ctx context.Context, req Request, deployment, taskID string, sink <-chan event.Body, emit func(context.Context, event.Body) error) (Outcome, error) {
	send := func(c event.Body) error {
		if emit == nil {
			return nil
		}
		return emit(ctx, c)
	}
	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// sandbox owns the task's lifecycle and has no cancel, so the
			// tool is told through the turn it shares with this goroutine
			// (see detach). The run is what gets retried.
			return Outcome{}, fmt.Errorf("runtime: run %s: %w", req.RunID, ctx.Err())

		case c := <-sink:
			if err := send(c); err != nil {
				return Outcome{}, err
			}

		case <-ticker.C:
			res, done, err := s.plane.Result(ctx, deployment, taskID)
			if err != nil {
				return Outcome{}, fmt.Errorf("runtime: run %s: result: %w", req.RunID, err)
			}
			if !done {
				continue
			}
			for {
				select {
				case c := <-sink:
					if err := send(c); err != nil {
						return Outcome{}, err
					}
					continue
				default:
				}
				break
			}
			if res.Error != "" {
				return Outcome{}, fmt.Errorf("runtime: run %s: task %s: %s", req.RunID, taskID, res.Error)
			}
			var out Outcome
			if err := json.Unmarshal(res.Output, &out); err != nil {
				return Outcome{}, fmt.Errorf("runtime: run %s: decode outcome: %w", req.RunID, err)
			}
			return out, nil
		}
	}
}

// ensure deploys the service a pinned definition runs in, registering its tool
// and its delegation targets' tools, and returns the deployment name. The
// grants are read from the pinned policy (workspace areas) and the pinned
// sub-agents (spawn), and from nothing else.
func (s *Sandbox) ensure(ctx context.Context, req Request) (string, error) {
	agent := req.Definition.Spec.Name
	if req.Pin.IsZero() || req.Pin.Hash == "" || req.Pin.Name != agent || req.Pin.Version != req.Definition.Version {
		// A deployment is named by its pin, so a request without one would
		// share another definition's grants.
		return "", fmt.Errorf("%w: agent %q has no pin for version %q", ErrInvalid, agent, req.Definition.Version)
	}
	name := DeploymentName(req.Pin)

	s.mu.Lock()
	_, ok := s.deployed[name]
	s.mu.Unlock()
	if ok {
		return name, nil
	}

	if err := s.register(ToolName(agent)); err != nil {
		return "", err
	}
	spawn := make([]string, 0, len(req.Definition.Spec.Access.Spawn))
	for _, target := range req.Definition.Spec.Access.Spawn {
		// A delegation target must be dispatchable before it can be spawned,
		// and it runs under its own tool name with no grants of its own: a
		// sub-task inherits nothing.
		if err := s.register(ToolName(target)); err != nil {
			return "", err
		}
		spawn = append(spawn, ToolName(target))
	}
	areas := make([]dispatchsandbox.Area, 0, len(req.Policy.WorkspaceAreas))
	for _, prefix := range req.Policy.WorkspaceAreas {
		areas = append(areas, dispatchsandbox.Area{Prefix: prefix})
	}

	err := s.plane.Deploy(ctx, controlplane.ServiceSpec{
		Name:     name,
		Replicas: s.replicas,
		Policies: []dispatchsandbox.Policy{{
			Tool:  ToolName(agent),
			Areas: areas,
			Spawn: spawn,
		}},
	})
	// Another runtime instance may have deployed the same pinned revision
	// first; the spec is a pure function of the revision, so the existing
	// deployment is the same deployment.
	if err != nil && !errors.Is(err, controlplane.ErrExists) {
		return "", fmt.Errorf("runtime: deploy %s: %w", name, err)
	}

	s.mu.Lock()
	s.deployed[name] = name
	s.mu.Unlock()
	return name, nil
}

// register adds an agent's tool to the sandbox registry, once.
func (s *Sandbox) register(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.registered[name] {
		return nil
	}
	if err := s.tools.Register(tool.Func(name, s.call)); err != nil {
		// Registered by another Sandbox over the same registry: the tool body
		// is this package's either way, so adopt it rather than failing.
		s.logger.Debug("flow/runtime: playscape tool already registered", "tool", name, "error", err)
	}
	s.registered[name] = true
	return nil
}

// attach registers a turn someone is about to wait on, under the key its tool
// will look it up by.
func (s *Sandbox) attach(key string, t *turn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.turns[key]; exists {
		// Two attempts of one run publishing into two sinks would interleave
		// on the same topic. durable's fencing already stops a zombie; this
		// stops the in-process case it cannot see.
		return fmt.Errorf("%w: run %s is already executing in this process", ErrInvalid, key)
	}
	s.turns[key] = t
	return nil
}

// detach forgets a turn and aborts its task. For a task that has already
// reported its result the abort reaches nothing.
func (s *Sandbox) detach(key string) {
	s.mu.Lock()
	t := s.turns[key]
	delete(s.turns, key)
	s.mu.Unlock()
	if t != nil {
		close(t.abort)
	}
}

func (s *Sandbox) turn(key string) *turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turns[key]
}

// call is the sandbox tool: one agent turn, inside the sandbox.
//
// It runs on a node's goroutine, with a context that carries neither the run's
// replay frame nor the caller's cancellation, and with a workspace already scoped
// to the policy. Everything it needs travels in the task input.
func (s *Sandbox) call(ctx context.Context, rt tool.Runtime, input []byte) ([]byte, error) {
	var task taskInput
	if err := json.Unmarshal(input, &task); err != nil {
		return nil, fmt.Errorf("runtime: sandbox: decode request: %w", err)
	}
	req := task.Request
	rev := req.Definition
	if err := rev.Validate(); err != nil {
		return nil, err
	}

	// The turn this task was submitted for. A task whose turn is gone was
	// abandoned before a node picked it up, and running it would spend a model
	// call on an answer nobody will read. A request with no key at all was
	// submitted by something that does not wait this way, and runs unwatched.
	var waited *turn
	if key := firstNonEmpty(req.RunID, task.Sub); key != "" {
		if waited = s.turn(key); waited == nil {
			return nil, fmt.Errorf("runtime: sandbox: turn %s is no longer awaited", key)
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		go func(abort, done <-chan struct{}) {
			select {
			case <-abort:
				cancel()
			case <-done:
			}
		}(waited.abort, ctx.Done())
	}

	// The turn acts for whoever the turn it was submitted for does, and for
	// nobody when it is unwatched. It is set here whatever the node's own
	// context carried, so a task cannot inherit someone it was not given.
	var actor event.Actor
	if waited != nil {
		actor = waited.actor
	}
	ctx = event.WithActor(ctx, actor)

	// A delegated sub-task's sink is read by the delegation waiting on it,
	// which relays only the sub-task's usage onto the parent's stream: its
	// answer is the tool result its parent reads, and it is not a durable run
	// of its own, so it has no stream to publish onto.
	var sink chan event.Body
	var stop <-chan native.Stop
	var compact <-chan native.CompactRequest
	var holds turnrt.Holds
	if waited != nil {
		sink = waited.sink
		compact = waited.compact
		holds = waited.holds
		if waited.stop != nil {
			// The request passes through here on its way to the agent loop,
			// so the turn remembers it before anything it cancels can ask.
			forward := make(chan native.Stop, 1)
			go func(in <-chan native.Stop, t *turn) {
				select {
				case req := <-in:
					t.stopped.Store(&req)
					forward <- req
				case <-ctx.Done():
				}
			}(waited.stop, waited)
			stop = forward
		}
	}
	send := func(c event.Body) error {
		if sink == nil {
			return nil
		}
		select {
		case sink <- c:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Every call this turn makes is keyed under one prefix, which a sub-task
	// it delegates to extends.
	req.Accounting = accountingOf(req)
	// The claim travels on a context of its own: goroutines above read
	// ctx, and it is not reassigned under them.
	tctx, release := s.claim(ctx, req)
	defer release()
	tools, granted, err := s.turnTools(tctx, rt, req, send)
	if err != nil {
		return nil, err
	}
	// The runner registers exactly the names the revision grants, so the
	// delegation tools have to be in that list. The revision here is a local
	// copy: the pinned definition itself is never edited.
	rev.Spec.Tools = granted

	// The sandbox's logger goes first so a host's own agent options can
	// replace it; it is where a granted tool the turn had to drop is
	// reported to the operator.
	opts := append(append([]native.Option{native.WithLogger(s.logger)}, s.agentOpts...), native.WithTools(tools))
	if req.Compaction != nil {
		opts = append(opts, native.WithCompaction(*req.Compaction))
	}
	if s.knowledge != nil {
		names := make([]string, 0, len(rev.Spec.Sources))
		for _, src := range rev.Spec.Sources {
			names = append(names, src.Name)
		}
		if r := s.knowledge(req.Tenant, names); r != nil {
			opts = append(opts, native.WithRetriever(r))
		}
	}
	runner, err := native.New(rev, opts...)
	if err != nil {
		return nil, err
	}

	res, err := runner.Run(tctx, native.Turn{
		Text: req.Text, Meta: req.Meta, History: req.History, Instructions: req.Instructions,
		Attachments: req.Attachments, Tenant: req.Tenant,
		Stop: stop, Compact: compact, Holds: holds, Accounting: req.Accounting,
		Park: req.Park, QuestionTTL: req.QuestionTTL, Resume: req.Resume, Budget: req.Budget,
	}, func(_ context.Context, c event.Body) error {
		return send(c)
	})
	if err != nil {
		return nil, err
	}

	out := Outcome{Text: res.Text, Prompt: res.Prompt, Chunks: res.Chunks, Citations: res.Citations, Stopped: res.Stopped, Parked: res.Parked}
	if out.Stopped != nil || out.Parked != nil {
		// Half an answer is not the run's answer artifact.
		return json.Marshal(out)
	}
	key := AnswerKey(rev.Spec.Name, req.RunID)
	if werr := rt.Workspace().Write(ctx, key, strings.NewReader(res.Text)); werr != nil {
		// The policy decided, and that is not a turn failure: an agent granted
		// no workspace area still answered. Reporting it keeps the decision
		// visible instead of pretending the artifact exists.
		out.StoreError = werr.Error()
	} else {
		out.Stored = key
	}
	return json.Marshal(out)
}

// ToolsFor reports what a turn of req would be granted, without running it:
// the tools its model would be shown and the grants it would drop, each with
// its reason. The tools are assembled exactly as Execute assembles them (the
// catalog, the tool groups for req's tenant and for the actor ctx carries, a
// delegation tool per agent in Access.Spawn) and resolved as the turn's
// runner resolves them, and none is called. No model client is built, so it
// needs no model credential. A group source may reach the server behind a
// group to expand it, as a turn does.
//
// ctx carries the actor as it does for Execute: whoever asks puts the actor
// the turn would act for on it (event.WithActor), and a context with none
// is a turn for nobody.
func (s *Sandbox) ToolsFor(ctx context.Context, req Request) (native.Granted, error) {
	rev := req.Definition
	if err := rev.Validate(); err != nil {
		return native.Granted{}, err
	}
	req.Accounting = accountingOf(req)
	// A group expansion may start a tool server, which needs the instance
	// a turn would have; it is released before this returns.
	ctx, release := s.claim(ctx, req)
	defer release()
	// Nothing runs, so there is no node runtime to hand the delegation tools
	// and nothing they report has anywhere to go.
	tools, granted, err := s.turnTools(ctx, nil, req, func(event.Body) error { return nil })
	if err != nil {
		return native.Granted{}, err
	}
	rev.Spec.Tools = granted
	opts := append(append([]native.Option{native.WithLogger(s.logger)}, s.agentOpts...), native.WithTools(tools))
	return native.ResolveTools(rev, opts...)
}

// claim puts the turn's claim on an instance on ctx, and returns how to
// release it. The release outlives the turn's cancellation, so a cancelled
// or failed turn destroys its instance too; a release that fails is logged,
// and never changes the turn's outcome.
func (s *Sandbox) claim(ctx context.Context, req Request) (context.Context, func()) {
	if s.instances == nil {
		return ctx, func() {}
	}
	run := s.instances(ctx, req)
	if run == nil {
		return ctx, func() {}
	}
	return isolation.WithRun(ctx, run), func() {
		if err := run.Release(context.WithoutCancel(ctx)); err != nil {
			s.logger.Error("flow/runtime: a run's instance was not released cleanly", "run", req.RunID, "agent", req.Agent(), "error", err)
		}
	}
}

// turnTools assembles the tools one turn may call: the deployment's catalog for
// the names the spec grants, plus one delegation tool per agent in Access.Spawn.
//
// Delegation is granted by Access.Spawn alone rather than by also listing
// "tag_<agent>" in Spec.Tools. The allowlist is the grant; requiring the same
// fact in two places would let them disagree, and the failure mode of that
// disagreement is an agent that cannot delegate despite being allowed to, or the
// reverse.
// It returns the catalog and the effective list of granted tool names, in a
// stable order so that two turns of the same revision show the model the same
// tools in the same sequence.
func (s *Sandbox) turnTools(ctx context.Context, rt tool.Runtime, req Request, send func(event.Body) error) (native.Tools, []string, error) {
	// A sub-task acts for the person its parent acts for.
	actor, _ := event.ActorFrom(ctx)
	report := func(a event.Action) error { return send(a) }

	granted := make([]string, 0, len(req.Definition.Spec.Tools)+len(req.Definition.Spec.Access.Spawn))
	seen := make(map[string]bool, cap(granted))
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		granted = append(granted, name)
	}
	for _, name := range req.Definition.Spec.Tools {
		add(name)
	}

	delegates := make(map[string]saigetypes.Tool, len(req.Definition.Spec.Access.Spawn))
	for _, target := range req.Definition.Spec.Access.Spawn {
		name := DelegateToolName(target)
		delegates[name] = s.delegateTool(rt, req, actor, target, report, send)
		if s.callTool == "" {
			add(name)
		}
	}
	if s.callTool != "" && len(req.Definition.Spec.Access.Spawn) > 0 {
		add(s.callTool)
	}

	sessionTools := map[string]saigetypes.Tool{}
	if s.sessionRuntime != nil && req.Session != nil {
		if r := s.sessionRuntime(); r != nil {
			sessionTools = r.sessionTools(req, send)
			for _, name := range sessionToolNames {
				if name != "session_call" || len(req.Definition.Spec.Access.Spawn) > 0 {
					add(name)
				}
			}
		}
	}
	var base native.Tools
	if s.catalog != nil {
		b, err := s.catalog(req, report)
		if err != nil {
			return nil, nil, err
		}
		base = b
	}

	return turnCatalog{ctx: ctx, tenant: req.Tenant, groups: s.groups, base: base, lookup: func(name string) (saigetypes.Tool, error) {
		if t, ok := sessionTools[name]; ok {
			return t, nil
		}
		if t, ok := delegates[name]; ok {
			return t, nil
		}
		if base == nil {
			return nil, fmt.Errorf("%w: %q (no tool catalog configured)", native.ErrNoSuchTool, name)
		}
		t, err := base.Lookup(name)
		if err != nil {
			return nil, err
		}
		if name == s.callTool {
			return &hostCallTool{Tool: t, sandbox: s, parent: req, send: send}, nil
		}
		return t, nil
	}}, granted, nil
}

// turnCatalog is a turn's tools: the lookup turnTools builds, plus the turn's
// tool groups, so a spec granting "mcp:<provider>" expands for the turn's
// tenant from the group source, or else against the same per-turn catalog its
// single tools resolve from. Delegation tools belong to no group: Access.Spawn
// alone grants them.
type turnCatalog struct {
	// ctx is the turn's: native asks for groups while it builds the
	// runner, inside the turn, and an expansion that dials a server must
	// stop when the turn does.
	ctx    context.Context
	tenant string
	groups ToolGroupSource
	base   native.Tools
	lookup func(name string) (saigetypes.Tool, error)
}

func (c turnCatalog) Lookup(name string) (saigetypes.Tool, error) { return c.lookup(name) }

func (c turnCatalog) ToolGroup(name string) (native.ToolGroup, error) {
	if c.groups != nil {
		tools, ok, err := c.groups.Expand(c.ctx, c.tenant, name)
		switch {
		case ok && err != nil:
			return native.ToolGroup{}, fmt.Errorf("%w: %q: %w", native.ErrNoSuchToolGroup, name, err)
		case ok:
			return native.ToolGroup{Name: name, Tools: tools}, nil
		}
	}
	groups, ok := c.base.(native.ToolGroups)
	if !ok {
		return native.ToolGroup{}, fmt.Errorf("%w: %q (this deployment's tool catalog has no groups)", native.ErrNoSuchToolGroup, name)
	}
	return groups.ToolGroup(name)
}

// delegateTool lets an agent tag another agent, gated by the spawn allowlist.
//
// The child is a sub-task of THIS turn, not a durable run: it runs in the same
// deployment, on the node this turn already holds, under its own tool name and
// the deployment's policy. Its answer comes back as this tool's result, and it
// therefore lands inside the parent activity's memoized record. That is why it
// resolves the target's current revision rather than pinning one — nothing
// replays a tool result, so there is nothing for an unpinned resolution to
// diverge from.
//
// The child is not given the parent's thread history, and that is a decision
// rather than an omission. A thread is one agent's conversation (see
// thread.KeyOf), and the child is a different agent: handing it the parent's
// journal would show it questions that were never put to it and answers it
// never gave, under its own role. What the child needs to know is what the
// parent chose to tell it, which is the text of the delegation. It has no run
// id either, so nothing it says is appended to any thread; its answer reaches
// the parent's journal only as part of whatever the parent finally answers.
//
// Stopping the parent stops the child, by cancellation: a stop cancels the
// tool calls the parent's turn has in flight, this one among them, and a
// sub-task nobody is waiting on is aborted (see the sub-task key below). The
// child is not wrapped up the way the parent is. What it had written is
// discarded with the tool call it was the result of, so there is no partial
// answer to keep.
//
// The other way to have an agent tag an agent is to raise a Tag at it, which
// creates a durable run of its own with its own pinned revision and its own
// topic. That is the right shape when the delegated work should outlive this
// turn; this one is the right shape when the parent needs the answer to continue.
//
// What the child spends is the parent's spending too. The child's model calls
// are relayed onto the parent's stream as usage.call events one level deeper,
// as they happen, so they are counted in the parent run's total (as Children)
// whether the child answers, fails or is aborted. A host Delegator reports the
// run it started with ReportChildUsage, from the context it is given, and the
// runtime reads that run's total from the ledger.
func (s *Sandbox) delegateTool(rt tool.Runtime, parent Request, actor event.Actor, target string, report func(event.Action) error, send func(event.Body) error) saigetypes.Tool {
	if parent.ChildRuns && parent.RunID != "" {
		return &childRunTool{s: s, parent: parent, target: target, send: send}
	}
	return &saigetypes.ToolFunc{
		Def: saigetypes.ToolDef{
			Name:        DelegateToolName(target),
			Description: fmt.Sprintf("Tag the %q agent with a request and wait for its answer.", target),
			Parameters: saigetypes.ParameterSchema{
				Type:     "object",
				Required: []string{"text"},
				Properties: map[string]saigetypes.PropertyDef{
					"text": {Type: "string", Description: "The request to send to " + target},
				},
			},
		},
		Fn: func(ctx context.Context, args map[string]any) (string, error) {
			text, _ := args["text"].(string)
			if strings.TrimSpace(text) == "" {
				return "", fmt.Errorf("%w: delegation to %q needs a text request", ErrInvalid, target)
			}
			// A host that gives delegated turns runs of their own takes it
			// from here. A sub-task has no run id, so it cannot be a parent
			// to one and falls through to delegating by sub-task.
			if s.delegator != nil && parent.RunID != "" {
				dctx, done := s.hostCallContext(ctx, parent, send)
				defer done()
				return s.delegator(dctx, parent, target, text, report)
			}
			if s.defs == nil {
				return "", fmt.Errorf("%w: this deployment cannot resolve agent %q", ErrInvalid, target)
			}
			loaded, err := s.subagent(ctx, parent, target)
			if err != nil {
				return "", fmt.Errorf("runtime: delegate to %q: %w", target, err)
			}
			// The sub-task is cancelled when this call stops waiting on it,
			// for whatever reason: the parent was stopped, failed, or was
			// abandoned. sandbox has no cancel of its own, so without this a
			// stopped parent would leave its helpers working on answers
			// nobody will read.
			sub := fmt.Sprintf("sub-%d", s.subs.Add(1))
			sink := make(chan event.Body, s.buffer)
			if err := s.attach(sub, &turn{sink: sink, abort: make(chan struct{}), actor: actor}); err != nil {
				return "", err
			}
			defer s.detach(sub)
			// Deferred after detach, so it runs first: everything the
			// sub-task reported before it returned is relayed before the
			// delegation returns its answer.
			defer relay(sink, send)()
			child := Request{
				// No run id: a sub-task has no stream of its own, and its
				// output would be attributed to the wrong agent on the
				// parent's topic. It reaches the transcript as this tool's
				// result, which is where the model put it.
				Tenant:  parent.Tenant,
				Attempt: parent.Attempt,
				Origin:  parent.Origin,
				Source:  parent.Source,
				Text:    text,
				// No History: see above.
				Definition: pinnedOf(loaded),
				Pin:        loaded.Pin,
				Policy:     loaded.Agent.Policy,
				Service:    parent.Service,
				// The child's calls are keyed under the parent's, so no two
				// calls of one run share a key at any depth.
				Accounting: parent.Accounting + "/" + target + "." + sub,
				// The files the parent was asked about go with the work it
				// hands off; a specialist asked to read a log it was never
				// given can only guess.
				Attachments: attachment.Inherit(parent.Attachments, nil),
			}
			input, err := json.Marshal(taskInput{Request: child, Sub: sub})
			if err != nil {
				return "", fmt.Errorf("runtime: delegate to %q: %w", target, err)
			}
			// The sub-task runs here, on the node this turn already holds,
			// rather than being spawned back into the queue: the turn does
			// nothing but wait for it, and a spawned sub-task on a deployment
			// with one node would wait behind the turn waiting for it.
			childRT, err := s.subTaskRuntime(ctx, rt, parent, target)
			if err != nil {
				// A denial is the policy speaking. The model sees it as a
				// tool error and can say so, instead of the turn dying.
				return "", fmt.Errorf("runtime: delegate to %q: %w", target, err)
			}
			res, err := s.call(ctx, childRT, input)
			if err != nil {
				return "", fmt.Errorf("runtime: delegate to %q: %w", target, err)
			}
			var out Outcome
			if err := json.Unmarshal(res, &out); err != nil {
				return "", fmt.Errorf("runtime: delegate to %q: decode outcome: %w", target, err)
			}
			return out.Text, nil
		},
	}
}

// relay forwards the usage a delegated sub-task reports on sink to its
// parent's stream, one level deeper, until the returned function is called;
// that function relays what is still buffered and waits for the relay to end.
// Everything else a sub-task streams is dropped: its answer reaches the parent
// as the delegation tool's result.
func relay(sink <-chan event.Body, send func(event.Body) error) func() {
	forward := func(c event.Body) {
		if up, ok := relayUsage(c); ok {
			// A parent that has gone away cannot be told. The child's
			// calls are then lost with the parent's own unfinished ones.
			_ = send(up)
		}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case c := <-sink:
				forward(c)
			case <-stop:
				for {
					select {
					case c := <-sink:
						forward(c)
					default:
						return
					}
				}
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// delegationContext is ctx with a cause a Delegator can read: when the
// parent's turn was stopped, the context ends with a *StoppedError, so a host
// can stop the run it started for the delegation; any other ending (a worker
// shutting down, a superseded attempt, a failed turn) keeps the cause it had.
func (s *Sandbox) delegationContext(ctx context.Context, runID string) (context.Context, func()) {
	dctx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	go func() {
		select {
		case <-ctx.Done():
			cause := context.Cause(ctx)
			if t := s.turn(runID); t != nil {
				if req := t.stopped.Load(); req != nil {
					cause = &StoppedError{Reason: req.Reason, By: req.By}
				}
			}
			cancel(cause)
		case <-dctx.Done():
		}
	}()
	return dctx, func() { cancel(nil) }
}

// subTaskRuntime gates a delegation from parent's turn to target and returns
// the sandbox runtime the sub-task executes against on the caller's node.
//
// It holds the sub-task to what a node would: the parent's identity must be
// granted the spawn by the deployment's policy, and the sub-task acts under its
// own tool name, default deny. Its workspace view and its own spawns are
// checked under that name and then under the parent's view as well, so running
// on the parent's node can never widen what either may touch.
func (s *Sandbox) subTaskRuntime(ctx context.Context, rt tool.Runtime, parent Request, target string) (tool.Runtime, error) {
	spec, err := s.plane.Spec(ctx, parent.Service)
	if err != nil {
		return nil, err
	}
	pdp, err := spec.PDP()
	if err != nil {
		return nil, err
	}
	user, child := ToolName(parent.Agent()), ToolName(target)
	if !pdp.Can(user, dispatchsandbox.OpSpawn, dispatchsandbox.SpawnObject(child)) {
		return nil, fmt.Errorf("%w: user %q may not spawn %q", dispatchsandbox.ErrDenied, user, child)
	}
	return &subTask{user: child, pdp: pdp, parent: rt, ws: dispatchsandbox.ScopePDP(rt.Workspace(), child, pdp)}, nil
}

// subTask is the tool.Runtime of a sub-task running on its parent's node.
type subTask struct {
	user   string
	pdp    dispatchsandbox.PDP
	parent tool.Runtime
	ws     workspace.Workspace
}

func (r *subTask) Workspace() workspace.Workspace { return r.ws }

func (r *subTask) Spawn(ctx context.Context, t task.Task) (string, error) {
	if !r.pdp.Can(r.user, dispatchsandbox.OpSpawn, dispatchsandbox.SpawnObject(t.Tool)) {
		return "", fmt.Errorf("%w: user %q may not spawn %q", dispatchsandbox.ErrDenied, r.user, t.Tool)
	}
	return r.parent.Spawn(ctx, t)
}

// childRunTool is the delegation tool of a durable run whose delegations are
// child runs (WithChildRuns). The turn parks at a call to it (it is
// native.Awaiting), the runtime starts the child, and the continuation runs
// the call with the child's ending as its answer.
type childRunTool struct {
	s      *Sandbox
	parent Request
	target string
	send   func(event.Body) error
}

var _ native.Awaiting = (*childRunTool)(nil)

func (t *childRunTool) Definition() saigetypes.ToolDef {
	return saigetypes.ToolDef{
		Name:        DelegateToolName(t.target),
		Description: fmt.Sprintf("Tag the %q agent with a request and wait for its answer.", t.target),
		Parameters: saigetypes.ParameterSchema{
			Type:     "object",
			Required: []string{"text"},
			Properties: map[string]saigetypes.PropertyDef{
				"text": {Type: "string", Description: "The request to send to " + t.target},
			},
		},
	}
}

// AwaitQuestion describes the wait a call parks on. A call with no request
// text is not a delegation: it runs, and tells the model what was wrong.
func (t *childRunTool) AwaitQuestion(call saigetypes.ToolCallPart) (event.Question, bool) {
	text, _ := call.Arguments["text"].(string)
	if strings.TrimSpace(text) == "" {
		return event.Question{}, false
	}
	args, _ := json.Marshal(call.Arguments)
	return event.Question{
		Prompt: text,
		Tool:   &event.QuestionTool{Name: call.Name, CallID: call.ID, Arguments: args},
	}, true
}

// Execute returns the child run's answer, and puts what the child spent on
// the parent's stream.
func (t *childRunTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	if text, _ := args["text"].(string); strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("%w: delegation to %q needs a text request", ErrInvalid, t.target)
	}
	a, ok := native.AnswerFor(ctx)
	if !ok {
		return "", fmt.Errorf("runtime: delegation to %q: the run it started has not answered", t.target)
	}
	if a.RunID != "" {
		if turn := t.s.turn(t.parent.RunID); turn != nil && turn.childUsage != nil {
			if c, err := turn.childUsage(ctx, a.RunID); err == nil {
				_ = t.send(c)
			}
		}
	}
	if a.Text == "" && a.Note != "" {
		return "", errors.New(a.Note)
	}
	return a.Text, nil
}

// hostCallContext gives a unified host tool the same stop propagation, child
// accounting and question forwarding as the built-in delegation tools.
func (s *Sandbox) hostCallContext(ctx context.Context, parent Request, send func(event.Body) error) (context.Context, func()) {
	dctx, done := s.delegationContext(ctx, parent.RunID)
	dctx = context.WithValue(dctx, usageReporterKey{}, func(ctx context.Context, runID string) error {
		t := s.turn(parent.RunID)
		if t == nil || t.childUsage == nil {
			return fmt.Errorf("%w: this runtime cannot vouch for child runs", ErrNotAChild)
		}
		c, err := t.childUsage(ctx, runID)
		if err != nil {
			return err
		}
		return send(c)
	})
	dctx = context.WithValue(dctx, childQuestionKey{}, func(ctx context.Context, runID string) error {
		t := s.turn(parent.RunID)
		if t == nil || t.childQuestion == nil {
			return fmt.Errorf("%w: this runtime cannot vouch for child runs", ErrNotAChild)
		}
		return watchChild(ctx, t.childQuestion, runID, func(cq event.ChildParked) error {
			return send(cq)
		})
	})

	return dctx, done
}

type hostCallTool struct {
	saigetypes.Tool
	sandbox *Sandbox
	parent  Request
	send    func(event.Body) error
}

func (t *hostCallTool) Execute(ctx context.Context, args map[string]any) (string, error) {
	ctx, done := t.sandbox.hostCallContext(ctx, t.parent, t.send)
	defer done()
	return t.Tool.Execute(ctx, args)
}

// Approval forwards a per-call approval to the tool the host wraps, so a host
// call tool that decides per call who must approve it still asks.
func (t *hostCallTool) Approval(call saigetypes.ToolCallPart) (native.ApprovalRequest, bool) {
	if approving, ok := t.Tool.(native.Approving); ok {
		return approving.Approval(call)
	}
	return native.ApprovalRequest{}, false
}

// ActsForPerson forwards whether the tool the host wraps runs with the
// credential of the person the turn acts for, so an approval of it says so.
func (t *hostCallTool) ActsForPerson() bool {
	p, ok := t.Tool.(native.PersonScoped)
	return ok && p.ActsForPerson()
}

func (t *hostCallTool) AwaitQuestion(call saigetypes.ToolCallPart) (event.Question, bool) {
	if awaiting, ok := t.Tool.(native.Awaiting); ok {
		return awaiting.AwaitQuestion(call)
	}
	return event.Question{}, false
}

// subagent loads the definition a delegation runs: the version the parent's
// pin names for target, verified, or the latest when the pin names none.
func (s *Sandbox) subagent(ctx context.Context, parent Request, target string) (definition.Loaded, error) {
	var (
		pin definition.Pin
		err error
	)
	if v, ok := parent.Pin.SubagentVersion(definition.NormalizeName(target)); ok {
		pin, err = s.defs.ResolveAt(ctx, parent.Tenant, target, v)
	} else {
		pin, err = s.defs.Resolve(ctx, parent.Tenant, target)
	}
	if err != nil {
		return definition.Loaded{}, err
	}
	return s.defs.Verify(ctx, parent.Tenant, pin)
}
