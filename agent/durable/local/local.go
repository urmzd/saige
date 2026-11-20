// Package local runs durable, resumable agent runs on one machine, in a
// private directory. It is the durable engine (github.com/urmzd/saige/durable)
// with its file ledger and file queue, and agent runs on it through
// agent/durable/anchor: every model call and tool execution is a memoized
// step, a crashed run resumes at its first unrecorded step, and a step that
// may have taken effect without a recorded result waits for Reconcile.
//
// Run executes a run in the calling process. One process at a time executes
// a run: another gets ErrBusy, and a crashed process gives its runs back at
// once. Each run is an open conversation: Append (or a Run with longer
// input) adds messages the run answers after the earlier ones.
//
// The directory needs a filesystem with reliable advisory locks and atomic
// rename and link. Run directories written by earlier releases (one
// state.json each) are migrated once, the first time the engine opens the
// directory; each keeps its old snapshot as state.json.migrated.
package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/urmzd/saige/durable/adapters/file/fileledger"
	"github.com/urmzd/saige/durable/adapters/file/filequeue"
	"github.com/urmzd/saige/durable/run"
	"github.com/urmzd/saige/durable/worker"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/durable/anchor"
	"github.com/urmzd/saige/agent/internal/durablecodec"
	"github.com/urmzd/saige/agent/types"
)

// Engine errors. They are anchor's, so an error from either engine matches.
var (
	ErrBusy          = anchor.ErrBusy
	ErrIndeterminate = anchor.ErrIndeterminate
	ErrConflict      = anchor.ErrConflict
	ErrClosed        = anchor.ErrClosed
	ErrNoNotifier    = anchor.ErrNoNotifier
	ErrSignal        = anchor.ErrSignal
)

// Run statuses, as State reports them.
const (
	statusReady     = "ready"     // has work: new input, or a reply since it suspended
	statusRunning   = "running"   // a process executes it now
	statusSuspended = "suspended" // waits for a decision or a reply
	statusCompleted = "completed" // answered all its input
	statusFailed    = "failed"    // failed, or waits for Reconcile
	statusCancelled = "cancelled" // ended by Cancel
)

// SignalChannel is the notifier channel an Engine with a Notifier announces
// changes on. Signal and its kinds are anchor's.
const SignalChannel = anchor.SignalChannel

// Signal kinds.
const (
	SignalReply  = anchor.SignalReply
	SignalInput  = anchor.SignalInput
	SignalCancel = anchor.SignalCancel
	SignalRun    = anchor.SignalRun
)

// Signal is the payload published on SignalChannel.
type Signal = anchor.Signal

// workflowPrefix names the workflow a revision's runs belong to.
const workflowPrefix = "saige.durable.local/"

// Factory must return a fresh agent and fresh budget for every replay. The
// revision binds its model profiles, tool implementations and policy settings.
// Completed steps reconstruct conversation state without repeating operations.
type Factory func() *agent.Agent

// Engine runs durable agent runs on files in a private directory.
type Engine struct {
	Directory string
	// ApprovalTTL is the expiry of an approval or interrupt posted without
	// its own deadline. Zero means 24 hours.
	ApprovalTTL time.Duration
	// Notifier, when set, announces replies, appended input, cancellation
	// and finished runs on SignalChannel, so Await in this or another
	// process wakes without polling.
	Notifier types.Notifier

	mu        sync.Mutex
	core      *anchor.Engine
	queue     *filequeue.Queue
	workflows map[string]*anchor.Workflow
	factories sync.Map // run ID -> Factory, while Run executes the run here
}

// New uses a host-owned private directory. A zero TTL defaults to 24 hours.
func New(directory string) *Engine { return &Engine{Directory: directory, ApprovalTTL: 24 * time.Hour} }

// open builds the engine on the directory the first time it is used, and
// migrates run directories an earlier release wrote.
func (e *Engine) open() (*anchor.Engine, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.core == nil {
		if e.Directory == "" {
			return nil, errors.New("directory required")
		}
		lgr, err := fileledger.New(e.Directory)
		if err != nil {
			return nil, err
		}
		q, err := filequeue.New(e.Directory)
		if err != nil {
			return nil, err
		}
		if err := migrate(e.Directory, lgr, q); err != nil {
			return nil, err
		}
		e.core, e.queue, e.workflows = anchor.New(lgr, q), q, map[string]*anchor.Workflow{}
		e.core.MaxAttempts = math.MaxInt32 // a local run is retried by running it again
	}
	e.core.ApprovalTTL, e.core.Notifier = e.ttl(), e.Notifier
	return e.core, nil
}

func (e *Engine) ttl() time.Duration {
	if e.ApprovalTTL <= 0 {
		return 24 * time.Hour
	}
	return e.ApprovalTTL
}

// workflow returns the open workflow of revision, registering it once.
func (e *Engine) workflow(revision string) (*anchor.Engine, *anchor.Workflow, error) {
	core, err := e.open()
	if err != nil {
		return nil, nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	w, ok := e.workflows[revision]
	if !ok {
		w = core.Register(workflowPrefix+revision, func(runID string) *agent.Agent {
			f, ok := e.factories.Load(runID)
			if !ok {
				return nil
			}
			return f.(Factory)()
		}, anchor.Open())
		e.workflows[revision] = w
	}
	return core, w, nil
}

// Run executes the run in this process and returns when it answers its
// input or stops for the host. ErrSuspended means pending approvals are
// saved; call Decide and then Run with the same identity. A crash during an
// operation leaves an indeterminate attempt: Run returns ErrIndeterminate
// and does not repeat it until Reconcile supplies its result or explicitly
// permits a retry. Another process executing the run now is ErrBusy.
//
// The run's input is an append-only log. input must agree with the log
// message by message: a prefix of the log (including nil) resumes the logged
// input, and a longer input appends its extra messages as a new segment that
// runs after the earlier ones finish. A message that differs from the log
// returns ErrConflict, as does a revision other than the run's. Append adds
// a segment without running it.
func (e *Engine) Run(ctx context.Context, id, revision string, factory Factory, input []types.Message) (*types.AssistantMessage, error) {
	if id == "" || revision == "" || factory == nil {
		return nil, errors.New("run ID, revision and factory are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	core, w, err := e.workflow(revision)
	if err != nil {
		return nil, err
	}
	if _, running := e.factories.LoadOrStore(id, factory); running {
		return nil, ErrBusy // this process executes it now
	}
	defer e.factories.Delete(id)
	final, err := core.RunHere(ctx, w, id, input, worker.WithLogger(quiet))
	if errors.Is(err, anchor.ErrFailed) {
		// Report what the run failed with, as earlier releases did.
		if s, ierr := core.Inspect(ctx, id); ierr == nil && s.Error != "" {
			return nil, fmt.Errorf("%w: %s", anchor.ErrFailed, s.Error)
		}
	}
	return final, err
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// check reads the run and refuses a revision other than its own.
func (e *Engine) check(ctx context.Context, id, revision string) (*anchor.Engine, anchor.State, error) {
	core, err := e.open()
	if err != nil {
		return nil, anchor.State{}, err
	}
	s, err := core.Inspect(ctx, id)
	if err != nil {
		return nil, anchor.State{}, notExist(err)
	}
	if s.Workflow != workflowPrefix+revision {
		return nil, anchor.State{}, ErrConflict
	}
	return core, s, nil
}

// leased reports whether a process executes the run now.
func (e *Engine) leased(ctx context.Context, id string) (bool, error) {
	if _, running := e.factories.Load(id); running {
		return true, nil
	}
	core, err := e.open()
	if err != nil {
		return false, err
	}
	leased, err := core.Leased(ctx, id)
	return leased, notExist(err)
}

// Append adds messages to a run's input log without running it. The next Run
// executes them after the logged input finishes. key makes the call
// idempotent: repeating it with the same messages is a no-op, and with
// different messages returns ErrConflict. A cancelled run returns ErrClosed,
// and a run with a live worker returns ErrBusy.
func (e *Engine) Append(id, revision, key string, msgs []types.Message) error {
	ctx := context.Background()
	core, s, err := e.check(ctx, id, revision)
	if err != nil {
		return err
	}
	if s.Cancelled {
		return ErrClosed
	}
	if leased, err := e.leased(ctx, id); err != nil {
		return err
	} else if leased {
		return ErrBusy
	}
	return core.Append(ctx, id, key, msgs)
}

// Inspect reads a snapshot of the run. A run that was never started returns
// an error matching os.ErrNotExist.
func (e *Engine) Inspect(id string) (State, error) {
	core, err := e.open()
	if err != nil {
		return State{}, err
	}
	s, err := core.Inspect(context.Background(), id)
	if err != nil {
		return State{}, notExist(err)
	}
	return e.state(s), nil
}

// Leased reports whether a worker currently executes the run. A run that was
// never started returns an error matching os.ErrNotExist.
func (e *Engine) Leased(id string) (bool, error) {
	if _, err := e.Inspect(id); err != nil {
		return false, err
	}
	return e.leased(context.Background(), id)
}

// List returns the state of every run under the engine directory for which
// keep returns true (every run when keep is nil), ordered by run ID.
func (e *Engine) List(keep func(State) bool) ([]State, error) {
	core, err := e.open()
	if err != nil {
		return nil, err
	}
	runs, err := core.List(context.Background(), func(s anchor.State) bool { return strings.HasPrefix(s.Workflow, workflowPrefix) })
	if err != nil {
		return nil, err
	}
	var out []State
	for _, s := range runs {
		st := e.state(s)
		if keep == nil || keep(st) {
			out = append(out, st)
		}
	}
	return out, nil
}

// Delete removes a finished run: completed, cancelled or failed. It fails
// with ErrBusy while a worker runs it, and with ErrConflict for a run that
// still has work (cancel a suspended run first) or a stale revision.
func (e *Engine) Delete(id, revision string) error {
	ctx := context.Background()
	core, _, err := e.check(ctx, id, revision)
	if err != nil {
		return err
	}
	if leased, err := e.leased(ctx, id); err != nil {
		return err
	} else if leased {
		return ErrBusy
	}
	return core.Delete(ctx, id)
}

// Decide accepts one authenticated host decision. key is its idempotency key.
// A conflicting retry, stale revision, cancellation, or expiry is rejected.
// An unknown interrupt matches types.ErrInterruptNotFound and an expired one
// matches types.ErrInterruptExpired as well as ErrClosed.
func (e *Engine) Decide(id, revision, interruptID, key string, decision types.ApprovalDecision) error {
	ctx := context.Background()
	core, _, err := e.check(ctx, id, revision)
	if err != nil {
		return err
	}
	return core.Decide(ctx, id, interruptID, key, decision)
}

// Cancel prevents future resumes. A currently running run returns ErrBusy;
// its owner must cancel the Run context first, then cancel here. Cancelling a
// cancelled run is a no-op, and a completed one returns ErrClosed.
func (e *Engine) Cancel(id, revision string) error {
	ctx := context.Background()
	core, s, err := e.check(ctx, id, revision)
	if err != nil {
		return err
	}
	switch {
	case s.Cancelled:
		return nil
	case s.Idle, s.Status == run.StatusSucceeded:
		return ErrClosed
	}
	if leased, err := e.leased(ctx, id); err != nil {
		return err
	} else if leased {
		return ErrBusy
	}
	return core.Cancel(ctx, id, "")
}

// Reconcile resolves an uncertain operation after the host checks its external
// effect. A supplied result is memoized; nil explicitly permits another attempt.
// No automatic retry is safe for an operation with an unknown side effect.
func (e *Engine) Reconcile(id, revision, name string, result *types.StepResult) error {
	ctx := context.Background()
	core, _, err := e.check(ctx, id, revision)
	if err != nil {
		return err
	}
	return core.Reconcile(ctx, id, name, result)
}

// Await blocks until ready reports true for the run's state, reading the
// state again each time a signal for the run arrives. It checks once after
// subscribing, so a change made before the call is not missed, and waits
// for a run that does not exist yet. It needs Engine.Notifier; every process
// that changes the run must use a notifier that reaches this one.
//
// A worker that serves input from other processes waits with Resumable:
//
//	for {
//		if _, err := engine.Await(ctx, id, local.Resumable); err != nil {
//			return err
//		}
//		_, err := engine.Run(ctx, id, revision, factory, nil)
//		// handle err; ErrSuspended waits for the next reply
//	}
func (e *Engine) Await(ctx context.Context, id string, ready func(State) bool) (State, error) {
	if e.Notifier == nil {
		return State{}, ErrNoNotifier
	}
	if ready == nil {
		return State{}, errors.New("await requires a ready function")
	}
	core, err := e.open()
	if err != nil {
		return State{}, err
	}
	var out State
	_, err = core.Await(ctx, id, func(s anchor.State) bool {
		out = e.state(s)
		return ready(out)
	})
	return out, err
}

// Resumable reports whether Run has work to do on s: the run has new input,
// or it suspended and every interrupt it waits on has a reply since.
func Resumable(s State) bool { return s.Status == statusReady }

// Router is the host-side types.InterruptRouter for runs of one engine.
type Router struct {
	Engine *Engine
	// RunID limits Reply to one run. When empty, Reply searches every
	// unfinished run and fails with ErrConflict if more than one holds the
	// interrupt ID.
	RunID string
}

var _ types.InterruptRouter = (*Router)(nil)

// Router returns a host-side interrupt router for the engine's runs.
func (e *Engine) Router() *Router { return &Router{Engine: e} }

func (rt *Router) router() (*anchor.Router, error) {
	core, err := rt.Engine.open()
	if err != nil {
		return nil, err
	}
	return &anchor.Router{Engine: core, RunID: rt.RunID}, nil
}

// Post records an interrupt on the run named by in.RunID from outside a
// worker. It returns types.ErrSuspended until a reply arrives, and the stored
// reply once one has.
func (rt *Router) Post(ctx context.Context, in types.Interrupt) (<-chan types.InterruptReply, error) {
	r, err := rt.router()
	if err != nil {
		return nil, err
	}
	return r.Post(ctx, in)
}

// Reply records a decision or answer. It is idempotent by (ID,
// IdempotencyKey): the same reply again is a no-op, and a different reply to
// a decided interrupt returns ErrConflict. An unknown ID matches
// types.ErrInterruptNotFound and a late reply matches
// types.ErrInterruptExpired. The next Run of the owning run picks it up.
func (rt *Router) Reply(ctx context.Context, reply types.InterruptReply) error {
	r, err := rt.router()
	if err != nil {
		return err
	}
	return r.Reply(ctx, reply)
}

// Pending lists unanswered, unexpired interrupts of runID, oldest first.
func (rt *Router) Pending(ctx context.Context, runID string) ([]types.Interrupt, error) {
	r, err := rt.router()
	if err != nil {
		return nil, err
	}
	return r.Pending(ctx, runID)
}

// notExist makes an unknown run match os.ErrNotExist, as earlier releases'
// errors did, as well as run.ErrNotFound.
func notExist(err error) error {
	if errors.Is(err, run.ErrNotFound) && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %w", os.ErrNotExist, err)
	}
	return err
}

func encode(v any) ([]byte, error) { return durablecodec.Encode(v) }

func decode(raw []byte, v any) error { return durablecodec.Decode(raw, v) }
