// Package duraturo runs SAIGE agents as durable workflows on
// github.com/urmzd/duraturo. Each LLM call and tool execution becomes a
// memoized duraturo step: a crashed or restarted worker replays the run to
// its first unrecorded step instead of repeating (and re-billing) the work.
//
// The engine composes any duraturo ledger and queue. The in-memory pair
// (ledger.NewMemory, queue.NewMemory) is a complete single-process system;
// github.com/urmzd/duraturo/adapters/postgres runs both on Postgres tables the
// host owns. The core agent package never imports this package: it depends
// only on the types.StepRunner seam.
//
// Approvals and interrupts are durable events. A run that needs a decision
// records the interrupt and parks: it leaves the queue, stays pending, and
// holds no worker. Reply writes the reply record and enqueues the run, which
// replays to the waiting call and continues. A step whose outcome is unknown
// after a crash parks the same way until Reconcile supplies its result or
// permits a retry (ErrIndeterminate).
//
// Durable runs execute tool calls sequentially. duraturo derives record keys
// from call order inside one workflow, so steps run one at a time in a stable
// order. The cost is latency on turns with many tool calls.
package duraturo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	dt "github.com/urmzd/duraturo"
	"github.com/urmzd/duraturo/pkg/ledger"
	"github.com/urmzd/duraturo/pkg/queue"
	"github.com/urmzd/duraturo/pkg/run"
	"github.com/urmzd/duraturo/pkg/worker"

	"github.com/urmzd/saige/agent"
	"github.com/urmzd/saige/agent/internal/durablecodec"
	"github.com/urmzd/saige/agent/types"
)

var (
	// ErrIndeterminate reports a step that may have taken effect without a
	// recorded result. The run parks until Reconcile resolves the step.
	ErrIndeterminate = errors.New("step outcome requires reconciliation")
	// ErrConflict reports a changed run input, a changed interrupt on
	// replay, or a reply that differs from the recorded one.
	ErrConflict = errors.New("durable run identity or decision conflict")
	// ErrClosed reports a reply or reconciliation for a finished run, or a
	// reply after its interrupt expired.
	ErrClosed = errors.New("durable run completed or interrupt closed")
	// ErrFailed reports a run that finished with an error. The message is
	// the error the run recorded.
	ErrFailed = errors.New("durable run failed")
)

// DefaultWorkflow is the workflow name Register uses for an empty name.
const DefaultWorkflow = "saige.agent.run"

// Factory returns a fresh agent, tree and budget for one execution of a run.
// It is called on the first execution and again on every replay, so it must
// build the same configuration each time. runID identifies the run.
type Factory func(runID string) *agent.Agent

// Engine registers agent workflows and manages their runs on one ledger and
// queue. Every worker that executes the runs must be built with Worker, so
// it resolves the workflows this engine registered.
type Engine struct {
	// ApprovalTTL is the expiry of an approval or interrupt posted without
	// its own deadline. Zero means 24 hours.
	ApprovalTTL time.Duration

	lgr      ledger.Ledger
	q        queue.Queue
	client   *dt.Client
	registry *run.Registry
}

// New composes an engine from a duraturo ledger and queue. Nothing is
// created or migrated: the implementations own storage.
func New(lgr ledger.Ledger, q queue.Queue) *Engine {
	return &Engine{
		ApprovalTTL: 24 * time.Hour,
		lgr:         lgr,
		q:           q,
		client:      dt.New(lgr, q),
		registry:    run.NewRegistry(),
	}
}

// Worker returns a duraturo worker over the engine's ledger, queue and
// workflows. Run it on a goroutine; it stops when its context is done. Keep
// the default codec: the engine writes reply records as JSON.
//
// A reply that lands while its run is still parking is picked up by the
// worker's janitor (worker.WithJanitorEvery), which re-enqueues parked runs.
func (e *Engine) Worker(opts ...worker.Option) *worker.Worker {
	return worker.New(e.lgr, e.q, append([]worker.Option{worker.WithRegistry(e.registry)}, opts...)...)
}

// Workflow is a registered agent workflow.
type Workflow struct {
	fn *dt.ActivityFn[runInput, runOutput]
}

// Name returns the registered workflow name.
func (w *Workflow) Name() string { return w.fn.Name() }

// runInput is the serialized workflow input. Messages is a durablecodec
// record of the input messages.
type runInput struct {
	Messages []byte `json:"messages"`
}

// runOutput is the serialized workflow output: a durablecodec record of the
// final AssistantMessage, or empty when the run produced none.
type runOutput struct {
	Final []byte `json:"final,omitempty"`
}

// Register adds an agent workflow under name, DefaultWorkflow when empty.
// Register every workflow before starting workers. The name is the
// compatibility contract for recorded runs: give a workflow whose recorded
// steps would no longer replay a new name. Registering a name twice panics.
func (e *Engine) Register(name string, factory Factory) *Workflow {
	if name == "" {
		name = DefaultWorkflow
	}
	fn := dt.ActivityIn(e.registry, name, func(ctx context.Context, in runInput) (runOutput, error) {
		return e.execute(ctx, factory, in)
	})
	return &Workflow{fn: fn}
}

// execute is the workflow body. It runs on every attempt, from the top: the
// runner replays recorded steps and executes the first unrecorded one.
func (e *Engine) execute(ctx context.Context, factory Factory, in runInput) (runOutput, error) {
	info, ok := dt.FromContext(ctx)
	if !ok {
		return runOutput{}, errors.New("agent workflow called outside a durable run")
	}
	var msgs []types.Message
	if err := decode(in.Messages, &msgs); err != nil {
		return runOutput{}, dt.NonRetryable(fmt.Errorf("decode input: %w", err))
	}
	if factory == nil {
		return runOutput{}, dt.NonRetryable(errors.New("factory required"))
	}
	a := factory(info.RunID)
	if a == nil {
		return runOutput{}, dt.NonRetryable(errors.New("factory returned nil agent"))
	}
	r := &runner{e: e, runID: info.RunID, budget: a.Budget(), posted: map[string]posted{}}
	final, err := a.RunDurable(ctx, r, msgs, "")
	if err != nil {
		return runOutput{}, outcome(ctx, err)
	}
	var out runOutput
	if final != nil {
		raw, err := encode(*final)
		if err != nil {
			return runOutput{}, dt.NonRetryable(fmt.Errorf("encode result: %w", err))
		}
		out.Final = raw
	}
	return out, nil
}

// outcome maps an agent error to the worker's protocol. A run waiting for a
// reply or a reconciliation parks. A worker shutdown hands the run back
// without using retry budget. Anything else fails the run: replaying the same
// records would reach the same error.
func outcome(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, run.ErrParked):
		return err
	case errors.Is(err, types.ErrSuspended), errors.Is(err, ErrIndeterminate):
		return fmt.Errorf("%w: %w", err, run.ErrParked)
	case ctx.Err() != nil:
		return err
	default:
		return dt.NonRetryable(err)
	}
}

// Start submits a run of w with input. id is the idempotency key: starting
// an existing run with the same workflow and input is a no-op, and a
// different workflow or input returns ErrConflict.
func (e *Engine) Start(ctx context.Context, w *Workflow, id string, input []types.Message) error {
	if w == nil || id == "" {
		return errors.New("workflow and run ID required")
	}
	raw, normalized, err := normalize(input)
	if err != nil {
		return err
	}
	if _, err := dt.Start(ctx, e.client, w.fn, runInput{Messages: raw}, dt.WithRunID(id)); err != nil {
		return err
	}
	// Accept keeps the first submission, so compare against what it stored.
	stored, err := e.getRun(ctx, id)
	if err != nil {
		return err
	}
	if stored.Name != w.Name() {
		return fmt.Errorf("%w: run %s belongs to workflow %s", ErrConflict, id, stored.Name)
	}
	var in runInput
	if err := json.Unmarshal(stored.Input, &in); err != nil {
		return fmt.Errorf("decode stored input: %w", err)
	}
	var logged []types.Message
	if err := decode(in.Messages, &logged); err != nil {
		return fmt.Errorf("decode stored input: %w", err)
	}
	if !reflect.DeepEqual(logged, normalized) {
		return fmt.Errorf("%w: run %s was started with different input", ErrConflict, id)
	}
	return nil
}

// Run starts the run and waits for it. See Start and Wait.
func (e *Engine) Run(ctx context.Context, w *Workflow, id string, input []types.Message) (*types.AssistantMessage, error) {
	if err := e.Start(ctx, w, id, input); err != nil {
		return nil, err
	}
	return e.Wait(ctx, id)
}

// Wait blocks until run id finishes or stops for the host. It returns the
// final message of a completed run, types.ErrSuspended while an interrupt
// waits for a reply, ErrIndeterminate while a step waits for Reconcile, and
// ErrFailed for a failed run. A worker must be running for the run to move.
func (e *Engine) Wait(ctx context.Context, id string) (*types.AssistantMessage, error) {
	delay := 5 * time.Millisecond
	for {
		r, records, err := e.lgr.Load(ctx, id)
		if err != nil {
			return nil, err
		}
		switch r.Status {
		case run.StatusSucceeded:
			return finalMessage(r.Output)
		case run.StatusFailed:
			return nil, fmt.Errorf("%w: %s", ErrFailed, r.Error)
		}
		s := inspect(r, records, time.Now())
		if len(s.Pending) > 0 {
			return nil, types.ErrSuspended
		}
		if len(s.Uncertain) > 0 {
			return nil, fmt.Errorf("%w: %s", ErrIndeterminate, s.Uncertain[0].Step)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, 250*time.Millisecond)
	}
}

func finalMessage(output []byte) (*types.AssistantMessage, error) {
	var out runOutput
	if err := json.Unmarshal(output, &out); err != nil {
		return nil, fmt.Errorf("decode result: %w", err)
	}
	if len(out.Final) == 0 {
		return nil, nil
	}
	var msg types.AssistantMessage
	if err := decode(out.Final, &msg); err != nil {
		return nil, fmt.Errorf("decode result: %w", err)
	}
	return &msg, nil
}

// getRun reads one run, without its records when the ledger supports it.
func (e *Engine) getRun(ctx context.Context, id string) (run.Run, error) {
	if g, ok := e.lgr.(ledger.RunGetter); ok {
		return g.GetRun(ctx, id)
	}
	r, _, err := e.lgr.Load(ctx, id)
	return r, err
}

func (e *Engine) ttl() time.Duration {
	if e.ApprovalTTL <= 0 {
		return 24 * time.Hour
	}
	return e.ApprovalTTL
}

// normalize round-trips messages through their record, so values compare the
// way the run input stores them.
func normalize(msgs []types.Message) ([]byte, []types.Message, error) {
	raw, err := encode(msgs)
	if err != nil {
		return nil, nil, err
	}
	var out []types.Message
	if err := decode(raw, &out); err != nil {
		return nil, nil, err
	}
	return raw, out, nil
}

// encode and decode go through durablecodec, which records messages and
// parts in a versioned form and reads the records earlier releases wrote.
func encode(v any) ([]byte, error) { return durablecodec.Encode(v) }

func decode(raw []byte, v any) error { return durablecodec.Decode(raw, v) }
