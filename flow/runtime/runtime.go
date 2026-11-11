// Package runtime is the durable turn: where an event becomes a run.
//
// It is the composition root of the three libraries flow is built on, and it
// exists because each of them owns exactly one thing and none of them owns this:
//
//	durable  the run survives a crash and replays from its ledger
//	sandbox  the turn executes inside a sandbox that enforces the agent's policy
//	saige     the turn is an agent loop (through pkg/turn/native)
//	pkg/bus   every event fans out live to observers and sinks
//
// # Idempotency
//
// A run accepted from an event is keyed by the event and its subscriber (see
// RunKey); a child run, by its trigger's id (see RunID). Slack retries its event,
// GitHub redelivers its webhook, a cron fires twice on a leader flap, and
// durable treats starting a run that already exists as a no-op returning the existing
// run. A redelivery therefore JOINS the run it duplicated instead of starting a
// second one, and no component downstream has to deduplicate anything.
//
// # Pinning, and why it is the most important line in this package
//
// Accept resolves the agent's latest definition ONCE and writes its pin
// (pkg/definition.Pin) into the run's input. Every execution of that run,
// forever, verifies that pin. Nothing inside the workflow body ever asks for
// "latest".
//
// This is not a nicety. durable replays a workflow from the top: on the second
// attempt the body re-executes and its recorded calls are matched by name and
// input hash. If the body resolved "latest" and someone revised the agent
// between attempts, the replayed call would carry a different input, the hashes
// would not match, and durable would fail the run with ErrNonDeterministic —
// after the model had already been paid for the first attempt. Worse, if it did
// somehow proceed, half a conversation would be answered by one definition and
// half by another, and the transcript would be a lie.
//
// The pin carries saige's resolved digest, the exact sub-agent versions and
// the policy hash, so a definition whose content moved under its version
// fails terminally instead of executing something the run never agreed to.
//
// # History is pinned the same way, for the same reason
//
// With a thread store (WithThreads) a run continues the conversation of the
// thread its trigger belongs to (see thread.KeyOf). The journal of that thread
// keeps growing while a run waits in the queue and between its attempts, so
// "the history" is exactly as unstable as "the latest version", and it gets
// exactly the same treatment: Accept reads the journal's head ONCE and writes
// that sequence number into the run's input, and every execution loads the
// entries at or below it. Nothing inside the workflow body ever asks the
// journal for its latest entries. The journal is append-only, so the entries
// at or below a sequence number are the same bytes forever, and an attempt
// made a day later asks the model what the first attempt asked.
//
// A journal that no longer holds what a run pinned fails the run terminally,
// as a definition whose digest moved does. The alternative is an agent that
// silently forgot half a conversation.
//
// A completed run appends its exchange to the journal: the request as the
// model was shown it, and the final answer. The append is idempotent on the
// run id, so a replayed turn appends nothing the second time. It happens
// before the completed event is published, and its failure fails the attempt:
// the agent's result is memoized, so the retry costs one write and no model
// call, and the alternative is a thread that looks answered on the surface
// and has no memory of it.
//
// Without a thread store none of this happens and a run's input is byte for
// byte what it was before the store existed.
//
// # Every event is published twice, to two different systems
//
//	durable.Emit    the run's own durable journal: replayable, per-run,
//	                 attempt-tagged, and readable by the engine that owns the
//	                 run. It survives the process. It is how an interrupted
//	                 stream can be reconstructed and how a resuming attempt can
//	                 see what a previous one already said.
//	bus.Publish      the live cross-run bus: ordered per run, fanned out to
//	                 every prefix subscriber, filtered per kind, and delivered
//	                 to routes. It is the product surface.
//
// Neither substitutes for the other. The journal is scoped to one run and known
// only to its engine, so a dashboard watching every agent cannot read it. The bus
// is a broker with retention, so it is not a durable record of what a run did.
// Emit is best-effort here (it is a no-op outside a run and when the queue has no
// delta log), while a failed Publish fails the turn: a run whose events nobody
// can see has not done its job.
//
// # A run can be stopped
//
// Stop asks a run to end before it has finished. The request is recorded on
// the run in the ledger and announced on the bus, the attempt executing the run
// hears it, and the run ends with lifecycle.stopped: over, not failed, never
// retried, and remembered by its thread with what it had said so far. See
// stop.go.
//
// # A run can park on a person
//
// A turn that needs a person (an approval, a choice, a reply) ends at the
// question. The run publishes it on lifecycle.parked, waits on a durable
// event record holding no worker, and continues the turn with the first
// answer recorded (Answer), the question's default once it expires, or ends
// stopped if it is stopped while it waits. See park.go.
//
// # Usage
//
// Every model call is published as usage.call as it happens, and a run's
// total, delegations included and every call of every attempt counted once,
// is on its terminal event and its Output. Every attempt's calls are kept on
// the run in the ledger before it returns, so a retried or re-executed
// attempt's calls are counted; a replay counts nothing.
// See usage.go.
//
// # Single goroutine
//
// durable v1 workflow bodies are single-goroutine. Nothing in this package
// forks the workflow, and every durable call — Call, Emit — happens on the
// goroutine the worker invoked. Parallelism lives inside the sandbox task,
// where it cannot confuse replay.
package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/urmzd/saige/durable"
	"github.com/urmzd/saige/durable/run"

	"github.com/urmzd/saige/flow/definition"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/event/legacy"
	oldnames "github.com/urmzd/saige/flow/internal/legacy"
	"github.com/urmzd/saige/flow/session"
	"github.com/urmzd/saige/flow/thread"
	turnrt "github.com/urmzd/saige/flow/turn"
	"github.com/urmzd/saige/flow/turn/native"

	"github.com/urmzd/saige/flow/attachment"
)

// Activity names. They are the correlation contract with the ledger: records
// belong to a name, so a breaking change to an input or output type is a new
// name, never a redefinition of an existing one.
const (
	// ActivityTurn is the run's entry point: one durable turn.
	ActivityTurn = "fiber.turn.v2"
	// ActivityAgent is the nested activity that executes the agent. It is
	// separate from the turn so that its result is memoized on its own: a
	// replay of the turn re-publishes lifecycle events but must never pay for
	// the model a second time.
	ActivityAgent = "fiber.turn.agent.v2"
	// ActivityResume continues a turn that parked on a question, with the
	// answer. It is its own activity because its input is not Input: it
	// carries the parked turn and the answer, both read from records, so a
	// replay hashes the same input and reads the same result.
	ActivityResume = "fiber.turn.resume.v2"
)

// The activity names before the event union. A run recorded under one is
// failed terminally (legacy.go): its input is a tag, not an event.
const (
	ActivityTurnV1   = "fiber.turn"
	ActivityAgentV1  = "fiber.turn.agent"
	ActivityResumeV1 = "fiber.turn.resume"
)

// ErrInvalid reports an event or configuration this package cannot run.
var ErrInvalid = errors.New("runtime: invalid")

// Bus is the live fan-out this package publishes to. It is the publishing half
// of pkg/bus.Bus, narrowed to what a run needs: a run produces events and never
// subscribes to them.
type Bus interface {
	Publish(ctx context.Context, e event.Envelope) error
}

// Definitions pins agent definitions and loads them back. A
// *definition.Resolver implements it.
//
// Resolve is called once, at accept time, and is the only place "latest" is
// ever asked for. ResolveAt pins one exact version, for a delegation that
// runs the sub-agent version its parent pinned. Verify is called on every
// execution of a run with the pin it accepted; it reloads exactly that and
// fails terminally when it no longer holds.
type Definitions interface {
	Resolve(ctx context.Context, tenant, name string) (definition.Pin, error)
	ResolveAt(ctx context.Context, tenant, name, version string) (definition.Pin, error)
	Verify(ctx context.Context, tenant string, pin definition.Pin) (definition.Loaded, error)
}

// Input is a run's durable input: the event that triggered it, the agent it
// runs for, and the definition pinned to it.
//
// It must be byte-identical on every attempt, because durable hashes it to
// match a replayed call against its record. Nothing derived from the moment of
// execution may appear here — not a timestamp, not an attempt number, not a
// sequence number. The attempt-varying values a turn needs are read from the
// replay frame instead (see durable.FromContext).
type Input struct {
	Budget        *session.Budget `json:"budget,omitempty"`
	RequireBudget bool            `json:"require_budget,omitempty"`
	// Session pins the continuing conversation independently of delivery.
	Session *session.Record `json:"session,omitempty"`
	// Trigger is the event the run answers, tenant set by the host.
	Trigger event.Envelope `json:"trigger"`
	// Subscriber is the agent the run is for.
	Subscriber string `json:"subscriber"`
	// Run is the run id, RunKey of the trigger and subscriber, for a run
	// accepted from an event. Empty for a child run, whose id is
	// RunID(tenant, trigger id).
	Run string `json:"run,omitempty"`
	// Pin is the definition pinned at accept time. It never changes.
	Pin definition.Pin `json:"pin"`
	// HistorySeq is the head of the trigger's thread journal, pinned at accept
	// time: the run is shown the entries with a sequence at or below it and
	// nothing newer, on every attempt. Zero means no history, which is also
	// what a runtime with no thread store always pins, so the field is
	// absent from such a run's input and that input hashes as it always did.
	//
	// It is a sequence number in the journal, not in the run's event stream,
	// and it is fixed at accept: it does not contradict the rule above.
	HistorySeq uint64 `json:"history_seq,omitempty"`
	// Parent is the run that started this one for a delegation and waits on
	// it (WithChildRuns). This run's ending answers the parent's wait. Absent
	// for every other run, so their input hashes as it always did.
	Parent *ParentRef `json:"parent,omitempty"`
	// Lineage is the run's place in the tree of runs answering one request,
	// fixed at accept like the pins, so every attempt stamps it on every
	// event the same way. Zero for a root run, whose input is then
	// byte for byte what it was before lineage existed.
	Lineage Lineage `json:"lineage,omitzero"`
}

// ParentRef names the run, and the wait in it, that a child run answers.
type ParentRef struct {
	RunID      string `json:"run_id"`
	QuestionID string `json:"question_id"`
}

// Output is a run's durable result.
type Output struct {
	// Version is the definition version that executed, which is always
	// Input.Pin.Version.
	Version string `json:"version"`
	// Text is the agent's final answer.
	Text string `json:"text"`
	// LastSeq is the sequence of the run's completed event: the cursor a
	// consumer passes back as Subscription.From to resume after this run.
	LastSeq uint64 `json:"last_seq"`
	// Attempt is the attempt that completed the run.
	Attempt int `json:"attempt"`
	// Events counts what the run published, the accepted event included.
	Events int `json:"events"`
	// Stored is the workspace key the answer artifact was written to, empty
	// when the pinned revision granted no workspace area for it.
	Stored string `json:"stored,omitempty"`
	// Stopped is set when the run ended because it was asked to stop, and
	// says why and by whom. Text is then the partial answer and LastSeq is
	// the sequence of the stopped event. The ledger records such a run as
	// succeeded: it ended, it did not fail, and it is never retried.
	Stopped *native.Stop `json:"stopped,omitempty"`
	// Usage is what the run spent: every model call of every attempt, and
	// everything it delegated, each counted once (see usage.go). It is in
	// the ledger with the rest of the output, so it survives a restart. Nil
	// when the run made no model call, and for an output recorded before
	// usage was.
	Usage *event.RunUsage `json:"usage,omitempty"`
}

// Request is one turn as the runtime runs it: the trigger's content, plus the
// exact definition to run it with. A turn.Runtime reads it from its context
// with RequestFrom.
//
// Definition is a resolved, pinned value rather than a name and a version. An
// turn runtime must not re-resolve it: it may be running in a sandbox, in another
// process, or on another machine, and "the definition to run" is not a question
// it is allowed to answer.
type Request struct {
	Budget  *turnrt.Budget  `json:"budget,omitempty"`
	Session *session.Record `json:"session,omitempty"`
	// Instructions is a trusted host-generated return contract for this run.
	Instructions string `json:"instructions,omitempty"`

	// RunID is the durable run. It is also the stream key: chunks a Sandbox
	// tool produces find their way back to this run by it.
	RunID string `json:"run_id"`
	// Lineage is the run's place in its tree, Root always set (RunID for a
	// root run). A host tool that starts a run this turn calls passes
	// Lineage.Child(RunID, call) with WithLineage.
	Lineage Lineage `json:"lineage,omitzero"`
	// Tenant is the authorization scope carried from the trigger.
	Tenant string `json:"tenant,omitempty"`
	// Attempt is the execution attempt, from 1.
	Attempt int `json:"attempt"`
	// Origin is the surface the trigger came through ("slack", "cron").
	Origin string `json:"origin,omitempty"`
	// Source is the address URI of the surface the trigger came from, and the
	// target connector actions are bound to. Empty for a trigger with no
	// surface, such as a schedule.
	Source string `json:"source,omitempty"`
	// Text is the request.
	Text string `json:"text"`
	// Meta is trigger context.
	Meta map[string]string `json:"meta,omitempty"`
	// History is the thread's conversation so far, oldest first, as pinned at
	// accept time. Like Definition it is a resolved value: a turn replays
	// it and must not go looking for a newer one.
	History []native.Message `json:"history,omitempty"`

	// Attachments are the files that arrived with the trigger, as references. A
	// delegated sub-task inherits its parent's, under the same tenant.
	Attachments []attachment.Attachment `json:"attachments,omitempty"`
	// Definition is the pinned definition to execute, compiled for a turn.
	Definition native.Revision `json:"definition"`
	// Pin is what Definition was verified against.
	Pin definition.Pin `json:"pin"`
	// Policy is the pinned definition's policy: the sandbox's NGAC grants
	// and who may answer its questions are read from it.
	Policy definition.Policy `json:"policy"`
	// Service is set by the Sandbox: the sandbox deployment this turn runs
	// in, so a delegated sub-task can be awaited in the right place.
	Service string `json:"service,omitempty"`
	// Accounting is the prefix the turn's model calls are keyed under when
	// saige reports no key of its own (see native.Turn.Accounting). Empty
	// for a durable run, which is keyed "<run>/a<attempt>"; a delegated
	// sub-task is handed its parent's prefix extended by its own name.
	Accounting string `json:"accounting,omitempty"`
	// Park lets the turn end at a question for a person (see
	// native.Turn.Park). The runtime sets it for a durable run when it can
	// record a question and its answer, which takes WithStopLedger. A
	// delegated sub-task never has it.
	Park bool `json:"park,omitempty"`
	// QuestionTTL is how long a question waits for an answer.
	QuestionTTL time.Duration `json:"question_ttl,omitempty"`
	// Resume continues a turn that parked, with the answers its pending tool
	// calls have. Like Definition and History it is a resolved value, read
	// from the run's records.
	Resume *native.Parked `json:"resume,omitempty"`
	// ChildRuns makes a delegation a child run the turn parks on, rather
	// than work it waits for (WithChildRuns). Only a durable run that can
	// park has it.
	ChildRuns bool `json:"child_runs,omitempty"`
	// Compaction, when set, is this turn's history budget in place of the
	// deployment's (native.WithCompaction): a host's per-run choice. Like
	// Definition it is a resolved value, the same on every attempt.
	Compaction *native.Compaction `json:"compaction,omitempty"`
}

// Agent returns the agent name.
func (r Request) Agent() string { return r.Definition.Spec.Name }

// Version returns the pinned definition version.
func (r Request) Version() string { return r.Definition.Version }

// Outcome is what one turn produced.
type Outcome struct {
	// Text is the agent's final answer.
	Text string `json:"text"`
	// Prompt is the request message exactly as the model was sent it:
	// trigger metadata, retrieved passages, and the request's text. It is what
	// the thread journal records as the user's message, so that the next run
	// in the thread replays the same bytes this one sent. A turn that
	// leaves it empty has the trigger as the catalog renders it recorded
	// instead, which keeps the conversation and gives up the shared request
	// prefix.
	Prompt string `json:"prompt,omitempty"`
	// Chunks counts the event bodies produced.
	Chunks int `json:"chunks"`
	// Citations counts the retrieved passages the turn was grounded in.
	Citations int `json:"citations,omitempty"`
	// Stored is the workspace key the answer was written to.
	Stored string `json:"stored,omitempty"`
	// StoreError explains why the answer was not stored. A policy denial here
	// is not a turn failure: it means the pinned definition was not granted that
	// workspace area, which is the sandbox working as defined.
	StoreError string `json:"store_error,omitempty"`
	// Stopped is set when the turn ended because it was asked to stop. Text
	// is then what had been answered so far, which may be nothing. An
	// turn sets it instead of returning an error: a stopped turn must not
	// be retried.
	Stopped *native.Stop `json:"stopped,omitempty"`
	// StopRecorded is set by the runtime, never by a turn: a stop
	// request is recorded on the run in the ledger (see WithStopLedger),
	// whether or not the turn was stopped by it, so the workflow body claims
	// that record and the ledger has no record left over that no call read.
	StopRecorded bool `json:"stop_recorded,omitempty"`
	// Usage is set by the runtime, never by a turn: what the run spent
	// up to and including this outcome, counted from the usage events the
	// attempts published (see usage.go). Whatever a turn puts here is
	// replaced.
	Usage *event.RunUsage `json:"usage,omitempty"`
	// UsageAttempts is set by the runtime: the earlier attempts whose usage
	// records Usage counts, which the workflow body claims.
	UsageAttempts []int `json:"usage_attempts,omitempty"`
	// Parked is set when the turn ended at a question for a person. Text is
	// then what it had said so far. A turn sets it from
	// native.Result.Parked.
	Parked *native.Parked `json:"parked,omitempty"`
}

// Acceptance is what a caller learns when a run is accepted. It carries the
// tenant and channel the run publishes on, so the caller never derives them.
type Acceptance struct {
	RunID string
	// Version is the definition version this call resolved. For the
	// delivery that created the run it is the pinned version. For a
	// redelivery it is whatever is latest now, which may be newer than what
	// the run actually pinned: Accept must announce before it submits (see
	// below), so at the moment it resolves it cannot yet know the run
	// already exists. The run's own events always carry the pinned version.
	Version string
	// Tenant and Channel are where the run's events are published.
	Tenant     string
	Channel    string
	AcceptedAt time.Time
}

// DecodeEntry reads one entry of a run's durable journal, as durable.Emit
// recorded it: an event envelope, or an entry written before the event union,
// which carries a kind and is read through the legacy table.
func DecodeEntry(raw []byte) (event.Envelope, error) {
	var probe struct {
		Type string `json:"type"`
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return event.Envelope{}, fmt.Errorf("runtime: journal entry: %w", err)
	}
	if probe.Type == "" && probe.Kind != "" {
		return legacy.DecodeEntry(raw)
	}
	var e event.Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return event.Envelope{}, fmt.Errorf("runtime: journal entry: %w", err)
	}
	return e, nil
}

// Runtime accepts events and runs them durably.
type Runtime struct {
	client *durable.Client
	defs   Definitions
	bus    Bus
	rt     turnrt.Runtime
	stride uint64
	logger *slog.Logger
	now    func() time.Time

	// threads is the conversation journal; nil runs every run as a fresh
	// conversation. lister and messageID only matter when it is set.
	threads   thread.Store
	lister    thread.Lister
	messageID func(event.Envelope) string

	// control is the bus as something to subscribe to, which is how a turn
	// hears a stop request. Nil when the bus can only be published to.
	control   Subscriber
	stops     StopLedger
	stopGrace time.Duration
	stopPoll  time.Duration
	// compactRequests makes turns listen for requests to compact.
	compactRequests bool

	// waker wakes a parked run once its question is answered or it is
	// stopped. Nil leaves it to the worker's janitor.
	waker        Waker
	questionTTL  time.Duration
	roles        Roles
	reader       RecordReader
	childRuns    bool
	sessions     session.Store
	childStarter ChildStarter
	childRunID   func(Input, SessionCall) string
	// outbox commits a run's ledger row with its accepted event. Nil
	// publishes the event, then registers the run.
	outbox Outbox

	registry *run.Registry
	turn     *durable.ActivityFn[Input, Output]
	agent    *durable.ActivityFn[Input, Outcome]
	resume   *durable.ActivityFn[Continue, Outcome]
}

// Option configures a Runtime.
type Option func(*Runtime)

// WithRuntime sets what runs each turn. Without it, or with nil, a turn runs
// the native saige runtime inside a local sandbox sandbox
// (sandbox.Sandboxed(native)). A Sandbox is the runtime a deployment with
// connector tools, delegation and child runs uses.
func WithRuntime(rt turnrt.Runtime) Option {
	return func(r *Runtime) {
		if rt != nil {
			r.rt = rt
		}
	}
}

// WithStride sets the per-attempt sequence band size (default DefaultStride).
func WithStride(n uint64) Option {
	return func(r *Runtime) {
		if n > 2 {
			r.stride = n
		}
	}
}

// WithLogger sets the logger (default slog.Default()).
func WithLogger(l *slog.Logger) Option {
	return func(r *Runtime) {
		if l != nil {
			r.logger = l
		}
	}
}

// WithClock replaces the clock used to stamp events. Event timestamps are not
// part of replay determinism — the bus keeps the first write of a sequence, so a
// re-published event keeps its original stamp — but a test that asserts on order
// wants a clock it controls.
func WithClock(now func() time.Time) Option {
	return func(r *Runtime) {
		if now != nil {
			r.now = now
		}
	}
}

// WithThreads gives runs a memory: each run is shown the conversation so far
// in its trigger's thread and appends its own exchange when it completes. See
// "History is pinned the same way" in the package documentation.
//
// Without it, or with a nil store, every run starts a fresh conversation and
// nothing about a run changes.
func WithThreads(store thread.Store) Option {
	return func(r *Runtime) { r.threads = store }
}

// WithReconciler reconciles a thread with its surface before a run's history
// is pinned, so the run is shown what people said in the thread without
// tagging the agent. It has no effect without WithThreads.
//
// Reconciliation is best-effort at accept time. A surface that cannot be
// listed is logged and the run pins what the journal already holds: refusing
// an event because a history API was slow would trade a complete answer for
// no answer. The messages it missed are appended by the next accept.
//
// Pair it with WithMessageID, or the message that raised each run is
// journaled twice: once as a message someone posted, once by its run.
func WithReconciler(l thread.Lister) Option {
	return func(r *Runtime) { r.lister = l }
}

// WithMessageID tells the runtime which surface message raised a run: a Slack
// timestamp, a GitHub comment id. The trigger's body carries it, and only the
// host knows which field it is, so this is a function rather than a field the
// runtime guesses at. It returns "" when the event has no such message.
//
// The id is recorded on the journal entry for the run's request, which is how
// a reconciliation recognizes that message as one the journal already holds.
func WithMessageID(fn func(event.Envelope) string) Option {
	return func(r *Runtime) { r.messageID = fn }
}

// New composes a runtime.
//
// The durable activities are registered in a registry OWNED BY THIS INSTANCE
// rather than durable's default one. Two runtimes in one process (a test suite,
// a fleet serving two tenants' agents with different turn runtimes) would otherwise
// panic on a duplicate activity name at construction. Pass Registry() to the
// worker that consumes these runs.
func New(client *durable.Client, defs Definitions, b Bus, opts ...Option) (*Runtime, error) {
	switch {
	case client == nil:
		return nil, fmt.Errorf("%w: no anchor client", ErrInvalid)
	case defs == nil:
		return nil, fmt.Errorf("%w: no definitions; a run cannot pin a definition it cannot resolve", ErrInvalid)
	case b == nil:
		return nil, fmt.Errorf("%w: no bus; a run whose events nobody can see has not done its job", ErrInvalid)
	}
	r := &Runtime{
		client:   client,
		defs:     defs,
		bus:      b,
		stride:   DefaultStride,
		logger:   slog.Default(),
		now:      time.Now,
		registry: run.NewRegistry(),

		stopGrace: DefaultStopGrace,
		stopPoll:  DefaultStopPoll,

		questionTTL: native.DefaultQuestionTTL,
	}
	// Stopping needs the reading half of the bus. It is discovered rather
	// than configured, as a connector's roles are: every backend in pkg/bus
	// has it, and a bus that does not cannot be made to by an option.
	r.control, _ = b.(Subscriber)
	for _, o := range opts {
		o(r)
	}
	if r.rt == nil {
		r.rt = defaultRuntime()
	}
	if r.control == nil {
		// Said once, here, rather than discovered at the first stop: a bus
		// wrapper that only forwards Publish quietly removes the fast path.
		r.logger.Warn("flow/runtime: the bus cannot be subscribed to, so a running turn hears a stop only through the ledger poll",
			"stop_ledger", r.stops != nil)
	}
	r.turn = durable.ActivityIn(r.registry, ActivityTurn, r.runTurn)
	r.agent = durable.ActivityIn(r.registry, ActivityAgent, r.runAgent)
	r.resume = durable.ActivityIn(r.registry, ActivityResume, r.runResume)
	// A run accepted before the event union, or before the rename, is ended
	// with a reason (legacy.go).
	for _, name := range []string{ActivityTurnV1, oldnames.FiberTurnBeforeRename} {
		durable.ActivityIn(r.registry, name, r.failRunFromBeforeUnion)
	}
	for _, name := range []string{ActivityAgentV1, ActivityResumeV1} {
		durable.ActivityIn(r.registry, name, r.failActivityFromBeforeUnion)
	}
	return r, nil
}

// Registry returns the activity registry this runtime's runs are resolved
// against. Hand it to the worker:
//
//	w := worker.New(ledger, queue, worker.WithRegistry(rt.Registry()))
func (r *Runtime) Registry() *run.Registry { return r.registry }

// Accept starts the run of subscriber answering trigger, and is what the
// dispatcher calls for each member an event wakes. The run id is
// RunKey(tenant, trigger id, subscriber), so a redelivered event joins the
// run it started and two members of one channel get two runs.
//
// Order matters, and it is not the obvious one. The definition is resolved
// first, so an event for an unknown agent is refused rather than accepted and
// then failed. Then the accepted event (sequence 1) is published on the
// trigger's channel, where egress adapters render it, and only then is the run
// submitted: submitting
// makes the run claimable, and a fast worker would otherwise publish started
// first, and the bus treats a sequence at or below a run's high-water mark as
// already seen, so an accepted event that lost that race would be dropped
// rather than delivered late.
//
// The cost of that order is a run that is announced and then fails to submit.
// It is the cheaper failure: the caller gets the error synchronously and
// retries the same event, which lands on the same run id and the same
// sequence 1.
//
// Accept is idempotent on (tenant, event id, subscriber): a redelivery cannot
// change what the run pinned, because durable returns the existing run
// untouched and the bus keeps the first write of sequence 1. The same holds
// for the history pin.
func (r *Runtime) Accept(ctx context.Context, trigger event.Envelope, subscriber string) (Acceptance, error) {
	if err := trigger.Validate(); err != nil {
		return Acceptance{}, fmt.Errorf("runtime: accept: %w: %w", ErrInvalid, err)
	}
	if event.IsControl(trigger.Channel) {
		return Acceptance{}, fmt.Errorf("runtime: accept %s: %w: a control request starts no run", trigger.ID, ErrInvalid)
	}
	if subscriber == "" {
		return Acceptance{}, fmt.Errorf("runtime: accept %s: %w: no subscriber", trigger.ID, ErrInvalid)
	}
	runID := RunKey(trigger.Tenant, trigger.ID, subscriber)
	return r.start(ctx, Input{Trigger: trigger, Subscriber: subscriber, Run: runID, Lineage: LineageFrom(ctx)}, "")
}

// AcceptOwn accepts trigger for subscriber as the run RunID(tenant,
// trigger.ID). It is for a host that raises one event per subscriber, so the
// event id alone already names the subscriber: a redelivery still joins the
// run, and a run id the host derived before the event union keeps its value.
func (r *Runtime) AcceptOwn(ctx context.Context, trigger event.Envelope, subscriber string) (Acceptance, error) {
	if err := trigger.Validate(); err != nil {
		return Acceptance{}, fmt.Errorf("runtime: accept: %w: %w", ErrInvalid, err)
	}
	if event.IsControl(trigger.Channel) {
		return Acceptance{}, fmt.Errorf("runtime: accept %s: %w: a control request starts no run", trigger.ID, ErrInvalid)
	}
	if subscriber == "" {
		return Acceptance{}, fmt.Errorf("runtime: accept %s: %w: no subscriber", trigger.ID, ErrInvalid)
	}
	return r.start(ctx, Input{Trigger: trigger, Subscriber: subscriber, Lineage: LineageFrom(ctx)}, "")
}

// start resolves and pins the definition and history of in, then announces
// and submits the run, in that order (see Accept). A version pins that exact
// version, for a child run.
func (r *Runtime) start(ctx context.Context, in Input, version string) (Acceptance, error) {
	id := in.Trigger.ID
	if in.Subscriber == ReservedName {
		return Acceptance{}, fmt.Errorf("runtime: accept %s: %w: %q is the control namespace, not an agent", id, ErrInvalid, in.Subscriber)
	}
	runID := in.runID()
	if err := CheckRun(in.Subscriber, runID); err != nil {
		return Acceptance{}, fmt.Errorf("runtime: accept %s: %w", id, err)
	}
	if err := in.Lineage.validate(runID); err != nil {
		return Acceptance{}, fmt.Errorf("runtime: accept %s: %w", id, err)
	}
	if in.Parent != nil && in.Lineage.ParentRun != in.Parent.RunID {
		return Acceptance{}, fmt.Errorf("runtime: accept %s: %w: lineage names parent run %q, but the run answers %s",
			id, ErrInvalid, in.Lineage.ParentRun, in.Parent.RunID)
	}
	var (
		pin definition.Pin
		err error
	)
	if version != "" {
		pin, err = r.defs.ResolveAt(ctx, in.Trigger.Tenant, in.Subscriber, version)
	} else {
		pin, err = r.defs.Resolve(ctx, in.Trigger.Tenant, in.Subscriber)
	}
	if err != nil {
		return Acceptance{}, fmt.Errorf("runtime: accept %s: resolve agent %q: %w", id, in.Subscriber, err)
	}
	in.Pin = pin
	if err := r.openRunBudget(ctx, &in); err != nil {
		return Acceptance{}, err
	}
	if r.sessions != nil {
		if err = r.bindSession(ctx, &in); err != nil {
			return Acceptance{}, err
		}
	}

	// The second pin. It is taken here, next to the definition's, because it
	// is the same kind of fact: something that keeps moving, read once.
	if in.HistorySeq, err = r.pinHistory(ctx, in, runID); err != nil {
		return Acceptance{}, fmt.Errorf("runtime: accept %s: %w", id, err)
	}

	tenant := busTenant(in.Trigger.Tenant)
	acc := Acceptance{RunID: runID, Version: pin.Version, Tenant: tenant, Channel: in.Trigger.Channel, AcceptedAt: r.now().UTC()}

	e := r.event(in, SeqAccepted, event.Accepted{})
	startOpts := []durable.StartOption{durable.WithRunID(runID)}
	if r.outbox != nil {
		// The accepted event and the ledger row commit together: an
		// outbox write, not a publish followed by a separate insert.
		startOpts = append(startOpts, durable.WithAcceptor(func(ctx context.Context, dr run.Run) error {
			if err := r.outbox.Accept(ctx, e, dr); err != nil {
				return fmt.Errorf("publish accepted: %w", err)
			}
			return nil
		}))
	} else if err := r.bus.Publish(ctx, e); err != nil {
		return Acceptance{}, fmt.Errorf("runtime: accept %s: publish accepted: %w", id, err)
	}
	if in.Parent != nil {
		startOpts = append(startOpts, durable.WithParent(in.Parent.RunID))
	}
	if h, err := durable.Start(ctx, r.client, r.turn, in, startOpts...); err != nil {
		if h == nil && r.outbox != nil {
			// Nothing committed: no run and no accepted event.
			return Acceptance{}, fmt.Errorf("runtime: accept %s: %w", id, err)
		}
		// Start returns a handle alongside an enqueue failure: the run is
		// durable and the janitor will drive it anyway. Report the error,
		// since the caller asked for an acknowledgement it did not fully get,
		// but return the acceptance too, so it can observe the run it now owns.
		return acc, fmt.Errorf("runtime: accept %s: %w", id, err)
	}
	if r.sessions != nil && in.Session != nil {
		if err := r.sessions.BindRun(ctx, in.Trigger.Tenant, in.Session.ID, runID); err != nil {
			return acc, err
		}
	}
	return acc, nil
}

// Result blocks until a run is terminal and returns its output.
func (r *Runtime) Result(ctx context.Context, runID string) (Output, error) {
	return durable.HandleFor[Output](r.client, runID).Result(ctx)
}

// runTurn is the workflow body: one durable turn.
//
// It publishes the run's lifecycle and delegates the work. Everything in it is
// either derived from the pinned input or read from the replay frame, so a
// replay produces the same decisions; nothing here resolves, reads a clock for a
// decision, or forks. A turn that parks on a question waits for the answer as
// a durable event record and continues in a later attempt (see park.go).
func (r *Runtime) runTurn(ctx context.Context, in Input) (Output, error) {
	attempt := attemptOf(ctx)
	b, err := newBand(r.stride, attempt)
	if err != nil {
		return Output{}, durable.NonRetryable(err)
	}
	at := &attemptLog{r: r, in: in, band: b, attempt: attempt}
	// The agent activity opens the attempt when it executes for real; a
	// memoized replay publishes nothing until the body has something new to
	// say. A parked run the janitor wakes with no answer says nothing at all.
	ctx = context.WithValue(ctx, openerKey{}, at.open)

	out, err := r.agent.Call(ctx, in)
	if usageErr := r.reconcileSessionUsage(ctx, in); usageErr != nil {
		if errors.Is(usageErr, run.ErrParked) {
			return Output{}, usageErr
		}
		err = usageErr
	}
	claimed := map[int]bool{}
	var said strings.Builder
	parks := 0
	var transcript []json.RawMessage
	for err == nil && out.Parked != nil {
		if transcript, err = wholeTranscript(transcript, out.Parked); err != nil {
			break
		}
		out.Parked.Transcript, out.Parked.Since = transcript, 0
		r.claimUsage(ctx, in, out.UsageAttempts, claimed)
		said.WriteString(out.Text)
		res, werr := r.awaitAnswers(ctx, at, out)
		if werr != nil {
			return Output{}, werr
		}
		if stop := stoppedIn(res); stop != nil {
			out.Text = said.String()
			return r.stoppedWhileParked(ctx, at, out, *stop, parks)
		}
		waits := out.Parked.Waits()
		for i, w := range waits {
			if q := w.Question; q.Kind == event.QuestionHold && res[i].Answer.Decision != event.DecisionApprove {
				// A hold that was not approved stops the run, and the batch
				// it held never runs (hold.go).
				out.Text = said.String()
				return r.stoppedWhileParked(ctx, at, out, heldStop(q, *res[i].Answer), parks)
			}
		}
		parks++
		// The batch continues once, with every answer it waited on.
		at.answer, at.answers = res[0].Answer, nil
		next := *out.Parked
		next.Answers = make(map[string]event.Answer, len(out.Parked.Answers)+len(waits))
		for k, v := range out.Parked.Answers {
			next.Answers[k] = v
		}
		for i, w := range waits {
			next.Answers[w.CallID] = *res[i].Answer
			if len(waits) > 1 {
				at.answers = append(at.answers, *res[i].Answer)
			}
		}
		out, err = r.resume.Call(ctx, Continue{Input: in, Seq: parks, Parked: next})
		if usageErr := r.reconcileSessionUsage(ctx, in); usageErr != nil {
			if errors.Is(usageErr, run.ErrParked) {
				return Output{}, usageErr
			}
			err = usageErr
		}
	}
	switch {
	case errors.Is(err, run.ErrParked):
		// The run stopped at an event that has no record yet. It is not
		// failed and it has not consumed retry budget; it waits.
		at.report(ctx, event.Parked{Attempt: attempt, Reason: err.Error()})
		return Output{}, err

	case err != nil:
		// Retry budget belongs to durable, not here, so terminality is
		// reported as what this package can actually know: a non-retryable
		// error will not be attempted again, anything else may be. A run that
		// later exhausts its budget has its last failure on the bus marked
		// non-terminal; the ledger stays the authority on run status.
		terminal := run.IsNonRetryable(err) || errors.Is(err, run.ErrNonDeterministic)
		at.report(ctx, event.Failed{Attempt: attempt, Error: err.Error(), Terminal: terminal})
		if terminal {
			if e := r.closeRunBudget(ctx, in); e != nil {
				return Output{}, e
			}
			r.answerParent(ctx, in, "", nil, err)
		}
		return Output{}, err
	}
	if err := r.closeRunBudget(ctx, in); err != nil {
		return Output{}, err
	}
	// The run's answer is everything it said, across every park.
	said.WriteString(out.Text)
	out.Text = said.String()

	// The exchange joins the thread before the run is announced complete, so
	// that an event raised in reply to the answer always finds the answer in the
	// history it pins. A replay appends nothing: the journal recognizes the
	// run. A stopped run is remembered the same way, for the same reason: the
	// reply to a stopped answer is the whole point of stopping gently.
	if err := r.remember(ctx, in, out); err != nil {
		at.report(ctx, event.Failed{Attempt: attempt, Error: err.Error()})
		return Output{}, err
	}

	if out.StopRecorded {
		r.claimStop(ctx, in)
	}
	r.claimUsage(ctx, in, out.UsageAttempts, claimed)
	// accepted, opened, the agent's chunks, a parked and a resumed per
	// question, and the terminal event.
	events := out.Chunks + 3 + 2*parks
	if out.Stopped != nil {
		// The run is over and nothing failed. Returning a result rather than
		// an error is what tells the ledger so: it records the run as ended
		// and never retries it. Everything here comes from the agent's
		// recorded outcome, so a replay announces the same stop.
		if err := at.publish(ctx, event.Stopped{
			Attempt: attempt, Reason: out.Stopped.Reason, By: out.Stopped.By, Text: out.Text,
			Usage: out.Usage,
		}); err != nil {
			return Output{}, err
		}
		r.answerParent(ctx, in, out.Text, out.Stopped, nil)
		return Output{
			Version: in.Pin.Version, Text: out.Text, LastSeq: b.close, Attempt: attempt,
			Events: events, Stopped: out.Stopped, Usage: out.Usage,
		}, nil
	}

	if err := at.publish(ctx, event.Completed{
		Attempt: attempt, Text: out.Text, Usage: out.Usage,
	}); err != nil {
		return Output{}, err
	}
	r.answerParent(ctx, in, out.Text, nil, nil)
	return Output{
		Version: in.Pin.Version, Text: out.Text, LastSeq: b.close, Attempt: attempt,
		Events: events, Stored: out.Stored, Usage: out.Usage,
	}, nil
}

// claimStop claims a recorded stop request, for the one case where it
// matters: durable replays from the records it loaded when the attempt
// began, and logs any of those that no call read. A request recorded before
// this attempt began is among them and is claimed here. One recorded during
// the attempt is not among them; it reads as an event that has not happened
// (ErrParked), which here means only that there is nothing to claim, and the
// body carries on. The claim is advisory either way: the outcome is already
// decided.
func (r *Runtime) claimStop(ctx context.Context, in Input) {
	_, err := durable.Event[json.RawMessage](ctx, stopRecordName)
	switch {
	case err == nil, errors.Is(err, run.ErrParked):
	default:
		r.logger.Warn("flow/runtime: could not claim the recorded stop request",
			"run_id", in.runID(), "error", err)
	}
}

// attemptLog publishes one attempt's lifecycle into its sequence band.
//
// The attempt is opened (lifecycle.started, or resumed after the first) by the
// first thing it publishes, not when it begins: the agent activity opens it
// when it executes for real, and the body when it announces something. An
// attempt that only replays recorded work and parks again, as a parked run
// does every time the worker's janitor polls it, publishes nothing.
type attemptLog struct {
	r       *Runtime
	in      Input
	band    band
	attempt int
	opened  bool
	// answer is the answer this attempt resumed with, carried on its
	// resumed event; answers is every answer, when its park waited on more
	// than one.
	answer  *event.Answer
	answers []event.Answer
}

type openerKey struct{}

// open publishes the attempt's opening event, once.
func (a *attemptLog) open(ctx context.Context) error {
	if a.opened {
		return nil
	}
	var body event.Body = event.Started{Attempt: a.attempt}
	if a.attempt > 1 {
		// Attempt 1 started the work; every later attempt resumed it, whether
		// it was parked on a question or interrupted by a crash. The answer,
		// when there is one, says which.
		body = event.Resumed{Attempt: a.attempt, Answer: a.answer, Answers: a.answers}
	}
	if err := a.r.publish(ctx, a.in, a.band.open, body); err != nil {
		return err
	}
	a.opened = true
	return nil
}

// publish opens the attempt if it has not been, then publishes the attempt's
// last event: it takes the band's closing sequence.
func (a *attemptLog) publish(ctx context.Context, body event.Body) error {
	if err := a.open(ctx); err != nil {
		return err
	}
	return a.r.publish(ctx, a.in, a.band.close, body)
}

// report publishes an event whose failure must not mask the failure it reports.
// A parked or failed turn already has an error on its way up; losing the
// announcement of it is worth a log line, not a different error.
func (a *attemptLog) report(ctx context.Context, body event.Body) {
	if err := a.publish(ctx, body); err != nil {
		a.r.logger.Warn("flow/runtime: could not publish lifecycle event",
			"run_id", a.in.runID(), "type", body.Type(), "seq", a.band.close, "error", err)
	}
}

// runAgent is the nested activity that executes the agent.
//
// It is where the pinned definition is verified, by its pin and never by
// "latest", and where the agent's events are published as they stream. Its
// result is memoized, which is the whole reason it is a separate activity: a replay of the
// turn must re-publish the run's lifecycle without paying for the model again.
func (r *Runtime) runAgent(ctx context.Context, in Input) (Outcome, error) {
	return r.executeAgent(ctx, in, nil)
}

// runResume is the activity that continues a parked turn with its answer.
func (r *Runtime) runResume(ctx context.Context, c Continue) (Outcome, error) {
	return r.executeAgent(ctx, c.Input, &c.Parked)
}

// executeAgent is the agent activity's body, for a fresh turn or for the
// continuation of a parked one.
func (r *Runtime) executeAgent(ctx context.Context, in Input, resume *native.Parked) (Outcome, error) {
	// This attempt is doing real work: it says so before anything else.
	if open, ok := ctx.Value(openerKey{}).(func(context.Context) error); ok {
		if err := open(ctx); err != nil {
			return Outcome{}, err
		}
	}
	loaded, err := r.pinned(ctx, in)
	if err != nil {
		return Outcome{}, err
	}
	rev := pinnedOf(loaded)
	attempt := attemptOf(ctx)
	b, err := newBand(r.stride, attempt)
	if err != nil {
		return Outcome{}, durable.NonRetryable(err)
	}

	history, err := r.pinnedHistory(ctx, in)
	if err != nil {
		return Outcome{}, err
	}

	text, meta := in.prompt()
	source := ""
	if a, ok := in.surface(); ok {
		source = a.String()
	}
	req := Request{
		RunID:      in.runID(),
		Lineage:    in.Lineage.of(in.runID()),
		Tenant:     in.Trigger.Tenant,
		Attempt:    attempt,
		Origin:     in.Origin(),
		Source:     source,
		Text:       text,
		Meta:       meta,
		History:    history,
		Session:    in.Session,
		Definition: rev,
		Pin:        in.Pin,
		Policy:     loaded.Agent.Policy,

		Attachments: in.request().Attachments,

		// A question can be parked on only where its answer can be
		// recorded: in the ledger the run lives in.
		Park:        r.stops != nil,
		QuestionTTL: r.questionTTL,
		Resume:      resume,
		ChildRuns:   r.childRuns && r.stops != nil,
	}

	req.Budget, err = r.reserveTurnBudget(ctx, in, attempt)
	if err != nil {
		return Outcome{}, err
	}

	// What earlier attempts spent and failed is read here, inside the
	// memoized activity, and never in the workflow body: which attempts
	// failed is exactly the kind of fact that differs between replays.
	prior, priorAttempts := r.priorUsage(ctx, req.RunID, attempt)
	meter := &meter{s: spent{Attempt: attempt}}

	// A Delegator's report of a child run is checked against the ledger and
	// its total read from there; see ReportChildUsage.
	execCtx := context.WithValue(ctx, childUsageKey{}, r.childUsage(in.Trigger.Tenant, req.RunID))
	execCtx = context.WithValue(execCtx, childQuestionKey{}, r.childQuestions(in.Trigger.Tenant, req.RunID))
	// The turn acts for the actor on the run's record, read here and not
	// from anything the turn produces, so a model cannot name someone else.
	// It travels in the context rather than in the Request: a turn
	// that acts for a person (a tool source opening a session with that
	// person's own credential) reads event.ActorFrom, and the Request,
	// which a sandbox serializes into its task, never carries it.
	execCtx = event.WithActor(execCtx, in.Actor())
	out, err := r.execute(execCtx, in, req, func(ctx context.Context, body event.Body) error {
		// A call is counted when it is reported, before it is published:
		// the tokens were spent whether or not the bus takes the event.
		meter.observe(body)
		if c, ok := body.(event.UsageCall); ok && in.Session != nil {
			if store, ok := r.sessions.(session.UsageStore); ok {
				if err := store.RecordUsage(ctx, in.Trigger.Tenant, in.Session.ID, c); err != nil {
					// The durable usage receipt repairs this projection after
					// the activity. A projection outage must not repeat paid work.
					r.logger.Warn("flow/runtime: session usage projection deferred", "run_id", req.RunID, "error", err)
				}
			}
		}
		seq, err := b.delta()
		if err != nil {
			// The band is spent. Retrying would produce the same flood, so
			// this is terminal rather than another paid attempt.
			return durable.NonRetryable(err)
		}
		return r.publish(ctx, in, seq, body)
	})
	if resume != nil {
		// The request is the parked turn's, whatever path this turn took.
		out.Prompt = resume.Prompt
		trimTranscript(out.Parked, resume)
	}
	mine := meter.snapshot()
	// The attempt spent what it spent. Its calls go on the run before the
	// outcome goes up, whatever the outcome: a failure is retried, and a
	// success whose record is never written (the ledger write fails, the
	// worker dies before it) is executed again too. Either way the attempt
	// that finally records an outcome reads these calls and counts them.
	recorded := r.recordSpent(ctx, req.RunID, mine)
	if settleErr := r.settleTurnBudget(context.WithoutCancel(ctx), in, attempt, mine, req.Budget, err != nil); settleErr != nil {
		return out, settleErr
	}
	if err != nil && req.Budget != nil {
		err = durable.NonRetryable(err)
	}
	if err != nil {
		return out, fmt.Errorf("runtime: run %s: agent %q version %s: %w", req.RunID, rev.Spec.Name, rev.Version, err)
	}
	var tally event.Tally
	for i := range prior {
		prior[i].into(&tally)
	}
	mine.into(&tally)
	if usage := tally.Usage(); !usage.Total.IsZero() {
		out.Usage = &usage
	}
	out.UsageAttempts = priorAttempts
	if recorded {
		// Claimed by a replay of this attempt's outcome; this attempt's own
		// frame never loaded it, so it has nothing to claim yet.
		out.UsageAttempts = append(out.UsageAttempts, attempt)
	}
	return out, nil
}

// pinned verifies the run's pin and loads exactly what it names.
//
// The check is the second half of pinning. A version is immutable by
// contract, so a definition whose digest, sub-agents or policy no longer match
// the pin is either corruption or a store that mutated history. Executing it
// would silently run a definition the run never agreed to, so it fails
// terminally: replaying identical code against identical records cannot fix
// it. There is no bypass for an empty digest: Verify refuses one.
func (r *Runtime) pinned(ctx context.Context, in Input) (definition.Loaded, error) {
	if in.Pin.Name != in.Subscriber {
		return definition.Loaded{}, durable.NonRetryable(fmt.Errorf(
			"%w: run for agent %q pinned definition %q", ErrInvalid, in.Subscriber, in.Pin.Name))
	}
	loaded, err := r.defs.Verify(ctx, in.Trigger.Tenant, in.Pin)
	if err != nil {
		return definition.Loaded{}, fmt.Errorf("runtime: agent %q version %s: %w", in.Subscriber, in.Pin.Version, err)
	}
	return loaded, nil
}

// pinnedOf is a verified pin as the definition a turn runs.
func pinnedOf(l definition.Loaded) native.Revision {
	return native.Revision{Name: l.Pin.Name, Version: l.Pin.Version, Digest: l.Pin.Digest, Spec: l.Agent.Spec}
}

// pinHistory returns the sequence number a run is pinned to: the head of its
// thread's journal, after the thread has been reconciled with its surface. It
// is called once, by Accept, and is the only place this package asks a
// journal where it ends.
func (r *Runtime) pinHistory(ctx context.Context, in Input, runID string) (uint64, error) {
	if r.threads == nil {
		return 0, nil
	}
	key := in.threadKey()
	if err := key.Validate(); err != nil {
		return 0, err
	}
	if r.lister != nil && !key.OneShot() {
		var skip []string
		if id := r.surfaceID(in); id != "" {
			// The message being answered is the run's request, not its
			// history.
			skip = append(skip, id)
		}
		surface, _ := in.surface()
		if err := thread.Reconcile(ctx, r.threads, r.lister, key, surface, skip...); err != nil {
			r.logger.Warn("flow/runtime: could not reconcile the thread with its surface; pinning what the journal holds",
				"run_id", runID, "thread", key.String(), "error", err)
		}
	}
	seq, err := r.threads.Head(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("pin history of %s: %w", key, err)
	}
	return seq, nil
}

// pinnedHistory loads the history a run pinned, and only that: the entries at
// or below Input.HistorySeq. It runs on every attempt and returns the same
// messages on every attempt, because the journal never changes an entry.
//
// A journal that cannot produce them is terminal, like a revision whose hash
// moved: replaying identical code against a journal that lost entries cannot
// bring them back, and running with a shorter history would be an agent that
// forgot without saying so.
func (r *Runtime) pinnedHistory(ctx context.Context, in Input) ([]native.Message, error) {
	if in.HistorySeq == 0 {
		return nil, nil
	}
	key := in.threadKey()
	if r.threads == nil {
		return nil, durable.NonRetryable(fmt.Errorf(
			"%w: run pinned %d entries of thread %s but this runtime has no thread store",
			ErrInvalid, in.HistorySeq, key))
	}
	entries, err := r.threads.History(ctx, key, in.HistorySeq)
	switch {
	case errors.Is(err, thread.ErrGap), errors.Is(err, thread.ErrInvalid):
		return nil, durable.NonRetryable(fmt.Errorf("runtime: history of %s up to %d: %w", key, in.HistorySeq, err))
	case err != nil:
		return nil, fmt.Errorf("runtime: history of %s up to %d: %w", key, in.HistorySeq, err)
	}
	return historyOf(entries), nil
}

// historyOf projects journal entries into what a turn replays: a role and a
// text, in journal order.
//
// Everything else an entry carries stays behind. Its sequence, run id and
// timestamp differ between entries and mean nothing to a model, and the one
// thing kept from the rest is who spoke, for a message that reached the
// journal by reconciliation: a thread has several people in it, and "ship it"
// reads differently depending on who said it. The label is a function of the
// stored entry alone, so it renders the same on every run.
func historyOf(entries []thread.Entry) []native.Message {
	out := make([]native.Message, 0, len(entries))
	for _, e := range entries {
		m := native.Message{Role: native.Role(e.Role), Text: e.Text}
		if e.Role == thread.RoleUser {
			m.Attachments = e.Attachments
		}
		if e.RunID == "" && e.Role == thread.RoleUser {
			if who := firstNonEmpty(e.Actor.Display, e.Actor.ID); who != "" {
				m.Text = who + ": " + e.Text
				if e.Text == "" {
					// A message that was only files: the label alone,
					// with the files after it.
					m.Text = who + ":"
				}
			}
		}
		out = append(out, m)
	}
	return out
}

// remember appends a completed run's exchange to its thread: what the model
// was asked, then what it answered, as one atomic append so that no other
// run's entries can land between a question and its answer.
//
// It is called from the workflow body, so it runs again on every replay, and
// the journal makes that safe: an entry is recognized by its run id and role,
// and the first write is the one that stays.
//
// A stopped run's answer is what it had said, followed by StopMarker, and it
// is always recorded, even when the turn said nothing: see stoppedText. It is
// built from the agent's recorded outcome and a constant, so a replay offers
// the journal the same bytes it already holds.
func (r *Runtime) remember(ctx context.Context, in Input, out Outcome) error {
	if r.threads == nil {
		return nil
	}
	runID := in.runID()
	entries := make([]thread.Entry, 0, 2)
	// The request's files are journaled with it, as the references the run
	// was handed, so a later run in the thread is shown them where this run
	// was: after the request's text.
	files := in.request().Attachments
	if said := firstNonEmpty(out.Prompt, in.rendered()); strings.TrimSpace(said) != "" || len(files) > 0 {
		entries = append(entries, thread.Entry{
			Role: thread.RoleUser, Text: said, Actor: in.Actor(),
			RunID: runID, SurfaceID: r.surfaceID(in), At: in.Trigger.Time,
			Attachments: files,
		})
	}
	answer := out.Text
	if out.Stopped != nil {
		answer = stoppedText(out.Text)
	}
	// A turn that only called tools has no answer to remember, and an empty
	// assistant message is one a provider would refuse to be sent back.
	if strings.TrimSpace(answer) != "" {
		entries = append(entries, thread.Entry{
			Role: thread.RoleAssistant, Text: answer,
			Actor: event.Actor{ID: in.Subscriber, Bot: true},
			RunID: runID, At: r.now(),
		})
	}
	if len(entries) == 0 {
		return nil
	}
	key := in.threadKey()
	if _, err := r.threads.Append(ctx, key, entries...); err != nil {
		return fmt.Errorf("runtime: run %s: record the exchange in thread %s: %w", runID, key, err)
	}
	return nil
}

// surfaceID is the id of the surface message that raised the run, or "".
func (r *Runtime) surfaceID(in Input) string {
	if r.messageID == nil {
		return ""
	}
	return r.messageID(in.Trigger)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// publish sends one event to both the live bus and the run's durable journal.
//
// The bus comes first and its failure fails the turn. The journal is written
// after and best-effort: durable.Emit is a no-op outside a run and when the
// queue has no delta log, and a journal that lost an entry costs
// observability, never correctness. The one Emit error that is not advisory
// is ErrSuperseded: another attempt owns this run now, and a zombie that keeps
// publishing would interleave two attempts' output.
func (r *Runtime) publish(ctx context.Context, in Input, seq uint64, body event.Body) error {
	e := r.event(in, seq, body)
	if err := r.bus.Publish(ctx, e); err != nil {
		return fmt.Errorf("runtime: publish %s seq %d on %s: %w", e.Type, seq, e.Channel, err)
	}
	if err := durable.Emit(ctx, e); err != nil {
		if errors.Is(err, run.ErrSuperseded) {
			return fmt.Errorf("runtime: journal %s seq %d: %w", e.Type, seq, err)
		}
		r.logger.Warn("flow/runtime: journal append failed; the bus has the event",
			"run_id", e.Run, "seq", seq, "type", e.Type, "error", err)
	}
	return nil
}

// event builds one event of a run. Its channel is the channel of the event
// that triggered the run, its id is the run and sequence, and it carries the
// pinned version and digest and the run's lineage. It is one hop from the
// trigger.
func (r *Runtime) event(in Input, seq uint64, body event.Body) event.Envelope {
	runID := in.runID()
	lineage := in.Lineage.of(runID)
	e := event.Envelope{
		ID:      runID + ":" + strconv.FormatUint(seq, 10),
		Source:  "fiber://run/" + runID,
		Type:    body.Type(),
		Channel: in.Trigger.Channel,
		Time:    r.now().UTC(),
		Hops:    in.Trigger.Hops + 1,
		Tenant:  busTenant(in.Trigger.Tenant),
		Run:     runID,
		Seq:     seq,
		Agent:   in.Subscriber,
		Version: in.Pin.Version,
		Digest:  in.Pin.Digest,

		Root:           lineage.Root,
		ParentRun:      lineage.ParentRun,
		ParentToolCall: lineage.ParentToolCall,

		Data: body,
	}
	if in.Session != nil {
		e.Session = in.Session.ID
		if in.Session.Depth > 0 {
			e.ParentSession = in.Session.Key.Parent
		}
	}
	return e
}

// attemptOf reports the execution attempt, from 1.
//
// It reads the replay frame rather than the input, because the attempt is the
// one attempt-varying value a deterministic workflow is allowed to see: it is
// not part of the input and cannot change what the input hashes to. Outside a
// run there is exactly one attempt.
func attemptOf(ctx context.Context) int {
	info, ok := durable.FromContext(ctx)
	if !ok || info.Attempt < 1 {
		return 1
	}
	return info.Attempt
}

// RunKey is the run identity of one subscriber answering one event within
// one tenant: "r" and 32 hex digits of a domain-separated sha256 over the
// three, each length-prefixed. A redelivered event keeps its id and lands on
// the same run; two subscribers woken by one event get two runs. The domain
// keeps a key from ever equalling a RunID.
func RunKey(tenant, eventID, subscriber string) string {
	h := sha256.New()
	h.Write([]byte("fiber.run.key.v1\x00"))
	var n [binary.MaxVarintLen64]byte
	for _, field := range []string{tenant, eventID, subscriber} {
		h.Write(n[:binary.PutUvarint(n[:], uint64(len(field)))])
		h.Write([]byte(field))
	}
	return "r" + hex.EncodeToString(h.Sum(nil)[:16])
}

// RunID is the run identity of a child run within one tenant: its trigger's
// id, which is derived from its parent and the wait it answers, so a retried
// start joins the run the first one started. A run accepted from an event is
// keyed by RunKey instead.
//
// The tenant is part of the identity, not a property checked afterwards: two
// tenants are free to choose the same event id, and everything downstream is
// keyed by the run id alone (the ledger's idempotent start, the run's channel
// events, the thread journal). Scoping the id means there is nothing to
// collide on.
//
// The id is a hash rather than the two values joined. It is a topic segment,
// restricted to letters, digits, "-" and "_", and no separator from that
// alphabet could keep ("a_b", "c") and ("a", "b_c") apart. Each field is
// length-prefixed before hashing for the same reason. The hash is
// deterministic, so a redelivery lands on the same run.
func RunID(tenant, tagID string) string {
	h := sha256.New()
	var n [binary.MaxVarintLen64]byte
	for _, field := range []string{tenant, tagID} {
		h.Write(n[:binary.PutUvarint(n[:], uint64(len(field)))])
		h.Write([]byte(field))
	}
	return "r" + hex.EncodeToString(h.Sum(nil)[:16])
}
