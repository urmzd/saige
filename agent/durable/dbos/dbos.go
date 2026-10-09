// Package dbos provides a DBOS Transact-backed durable StepRunner and a workflow
// entrypoint for SAIGE's agent loop. Running an agent through it makes each LLM
// call and tool execution a durable, memoized step: a crashed or restarted
// process resumes the run from its last completed step instead of repeating
// (and re-billing) the work.
//
// This is an optional, heavy, Postgres-coupled integration kept out of the core
// agent package: the same isolation pattern as agent/pgstore and
// agent/provider/*. The core agent package never imports dbos; it depends only
// on the tiny types.StepRunner seam, for which this package supplies a
// DBOS-backed implementation.
//
// Durable runs execute tool calls SEQUENTIALLY, unlike the non-durable path,
// which fans tools out across goroutines. This is deliberate: DBOS correlates
// each RunStep with the workflow's calling context and replays steps in
// recorded order, so deterministic step ordering in the workflow goroutine is
// what makes crash recovery exact. The trade-off is latency on turns with many
// tool calls: a durable run pays the sum of its tools' latencies rather than
// the max.
//
// Approvals are answered with Engine.Decide and listed with
// Engine.PendingApproval. This package does not implement
// types.InterruptRouter: a decision travels as a durable message that leaves
// no record once the run consumes it, so a repeated or changed reply could not
// be told apart from a first one. Use the local durable engine for a router
// with idempotent replies.
package dbos

import (
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/agent"
	_ "github.com/urmzd/saige/agent/internal/durablecodec"
	"github.com/urmzd/saige/agent/types"
)

func init() {
	// Steps and workflow inputs/outputs memoize via the gob serializer
	// (configured in NewEngine). Register the concrete types behind the sealed
	// Message and Content interfaces so they round-trip on replay. gob ignores
	// json struct tags, so ToolResultBlock.Data and FileContent.Data bytes are
	// preserved through durable replay (unlike the tree's JSON persistence).
	// The workflow input/output themselves travel through the serializer as
	// interface values, so the wrapper types need registration too.
	gob.Register(RunInput{})
	gob.Register(RunOutput{})
	gob.Register(approvalMessage{})
	gob.Register(types.ApprovalRequest{})

}

// Runner adapts a workflow-bound dbos.DBOSContext to types.StepRunner by mapping
// each RunStep to dbos.RunAsStep. It must be constructed inside a workflow (with
// the DBOSContext passed to the workflow function), because RunAsStep requires a
// workflow context.
type Runner struct{ dctx dbos.DBOSContext }

var _ types.StepRunner = (*Runner)(nil)

// NewRunner wraps a workflow-bound DBOS context as a types.StepRunner.
func NewRunner(dctx dbos.DBOSContext) *Runner { return &Runner{dctx: dctx} }

// RunStep maps to dbos.RunAsStep: the step's result is checkpointed on first
// execution and returned from the record (without re-running fn) on replay.
func (r *Runner) RunStep(_ context.Context, name string, fn func(ctx context.Context) (types.StepResult, error)) (types.StepResult, error) {
	return dbos.RunAsStep(r.dctx, dbos.Step[types.StepResult](fn), dbos.WithStepName(name))
}

// ErrApprovalExpired reports that no decision arrived before the approval
// timeout. The run fails closed: the tool does not execute.
var ErrApprovalExpired = errors.New("dbos: approval request expired")

// approvalTopic is the Send/Recv topic that carries the decision for one
// approval request.
func approvalTopic(requestID string) string { return "saige.approval:" + requestID }

// PendingApprovalEvent is the workflow event key under which a run publishes
// the approval request it is waiting on. It holds an empty request when the
// run is not waiting.
const PendingApprovalEvent = "saige.approval.pending"

// approvalMessage is the decision sent to a waiting workflow. Decided is
// false only in the zero value Recv returns on timeout.
type approvalMessage struct {
	Decided  bool
	Decision types.ApprovalDecision
}

// ApprovalRunner is a Runner that also resolves tool approvals durably. A
// waiting run publishes its request under PendingApprovalEvent and blocks in
// a durable dbos.Recv until Engine.Decide sends a decision or the timeout
// passes. A recovered workflow replays the recorded decision instead of
// asking again. It requires sequential tool execution in the workflow
// goroutine, which Runner already guarantees.
type ApprovalRunner struct {
	*Runner
	timeout time.Duration
}

var _ types.ApprovalRunner = (*ApprovalRunner)(nil)

// NewApprovalRunner wraps a workflow-bound DBOS context as a StepRunner that
// resolves approvals, waiting at most timeout for each decision.
func NewApprovalRunner(dctx dbos.DBOSContext, timeout time.Duration) *ApprovalRunner {
	return &ApprovalRunner{Runner: NewRunner(dctx), timeout: timeout}
}

// ResolveApproval publishes req and waits for its decision. It returns
// ErrApprovalExpired when the timeout passes without one.
func (r *ApprovalRunner) ResolveApproval(_ context.Context, req types.ApprovalRequest) (types.ApprovalDecision, error) {
	if err := dbos.SetEvent(r.dctx, PendingApprovalEvent, req); err != nil {
		return types.ApprovalDecision{}, fmt.Errorf("dbos: publish approval request: %w", err)
	}
	msg, err := dbos.Recv[approvalMessage](r.dctx, approvalTopic(req.ID), r.timeout)
	if err != nil && !errors.Is(err, &dbos.DBOSError{Code: dbos.TimeoutError}) {
		return types.ApprovalDecision{}, fmt.Errorf("dbos: wait for approval: %w", err)
	}
	if err := dbos.SetEvent(r.dctx, PendingApprovalEvent, types.ApprovalRequest{}); err != nil {
		return types.ApprovalDecision{}, fmt.Errorf("dbos: clear approval request: %w", err)
	}
	if !msg.Decided {
		return types.ApprovalDecision{}, fmt.Errorf("%w: %s", ErrApprovalExpired, req.ID)
	}
	return msg.Decision, nil
}

// RunInput is the single serializable workflow input.
type RunInput struct {
	Messages []types.Message
	// Branch names a branch of the factory's fresh tree to run on; empty uses
	// that tree's active branch. A branch of another process's tree does not
	// exist in the fresh tree and fails the run.
	Branch types.BranchID
}

// RunOutput is the single serializable workflow output.
type RunOutput struct {
	Final *types.AssistantMessage
}

// Engine owns a DBOS context lifecycle and registers agent-run workflows.
type Engine struct {
	dctx dbos.DBOSContext

	// ApprovalTimeout enables durable approvals when positive: registered
	// workflows run with an ApprovalRunner that waits this long for each
	// decision sent through Decide. Zero keeps approvals unsupported, which
	// is required for agents that compact automatically, because the agent
	// rejects automatic compaction under a runner that resolves approvals.
	// Set it before Launch.
	ApprovalTimeout time.Duration
}

// Factory returns a fresh Agent, with its own tree and budget, for one
// workflow execution. It is called on the first run and again on every
// recovery of that workflow. workflowID identifies the run, so a host can key
// per-run budgets or stores by it. Do not load a partially persisted tree
// into the agent: RunDurable appends the workflow input itself.
type Factory func(workflowID string) *agent.Agent

// NewEngine builds a DBOS context backed by Postgres and a gob serializer. Pass
// the SAME *pgxpool.Pool used by agent/pgstore to share one connection pool, or
// pass nil with a databaseURL to let DBOS build its own pool. SystemDBPool takes
// precedence over DatabaseURL when both are set.
func NewEngine(ctx context.Context, appName string, pool *pgxpool.Pool, databaseURL string) (*Engine, error) {
	dctx, err := dbos.NewDBOSContext(ctx, dbos.Config{
		AppName:      appName,
		SystemDBPool: pool,
		DatabaseURL:  databaseURL,
		Serializer:   dbos.NewGobSerializer(),
	})
	if err != nil {
		return nil, err
	}
	return &Engine{dctx: dctx}, nil
}

// Context exposes the underlying DBOS context for advanced use (queues, events,
// streams, manual workflow retrieval).
func (e *Engine) Context() dbos.DBOSContext { return e.dctx }

// RegisterAgent registers a durable workflow that runs the given agent.
//
// Deprecated: every workflow shares the one Agent, including its tree and
// budget, so concurrent or recovered runs interleave their conversations and
// spend from one budget. Use RegisterAgentFactory.
func (e *Engine) RegisterAgent(a *agent.Agent, name string) dbos.Workflow[RunInput, RunOutput] {
	return e.RegisterAgentFactory(func(string) *agent.Agent { return a }, name)
}

// RegisterAgentFactory registers a durable workflow that builds a fresh agent
// with factory for each execution and runs it to completion via
// Agent.RunDurable. The workflow's DBOS context is the run's context, so
// Shutdown and a workflow timeout (dbos.WithTimeout on the context that starts
// the run) cancel in-flight provider and tool calls.
// It MUST be called before Launch. The returned Workflow value is passed to
// Run. name defaults to "saige.agent.run".
func (e *Engine) RegisterAgentFactory(factory Factory, name string) dbos.Workflow[RunInput, RunOutput] {
	if name == "" {
		name = "saige.agent.run"
	}
	wf := func(dctx dbos.DBOSContext, in RunInput) (RunOutput, error) {
		workflowID, err := dbos.GetWorkflowID(dctx)
		if err != nil {
			return RunOutput{}, err
		}
		a := factory(workflowID)
		if a == nil {
			return RunOutput{}, errors.New("dbos: factory returned nil agent")
		}
		var runner types.StepRunner = NewRunner(dctx)
		if e.ApprovalTimeout > 0 {
			runner = NewApprovalRunner(dctx, e.ApprovalTimeout)
		}
		final, err := a.RunDurable(dctx, runner, in.Messages, in.Branch)
		return RunOutput{Final: final}, err
	}
	dbos.RegisterWorkflow(e.dctx, dbos.Workflow[RunInput, RunOutput](wf), dbos.WithWorkflowName(name))
	return wf
}

// Launch starts the engine and recovers any PENDING workflows (resuming each
// from its last completed step). Call after all RegisterAgent calls.
func (e *Engine) Launch() error { return dbos.Launch(e.dctx) }

// Shutdown gracefully stops the engine.
func (e *Engine) Shutdown(timeout time.Duration) { dbos.Shutdown(e.dctx, timeout) }

// Run starts a durable agent run and returns a handle. workflowID is the
// idempotency key: a second call with the same ID returns a handle to the
// existing run instead of executing again, so derive it deterministically per
// conversation turn (e.g. the branch tip node ID). An empty workflowID lets DBOS
// generate one.
func (e *Engine) Run(wf dbos.Workflow[RunInput, RunOutput], in RunInput, workflowID string) (dbos.WorkflowHandle[RunOutput], error) {
	var opts []dbos.WorkflowOption
	if workflowID != "" {
		opts = append(opts, dbos.WithWorkflowID(workflowID))
	}
	return dbos.RunWorkflow(e.dctx, wf, in, opts...)
}

// Decide sends a decision for the approval request interruptID that workflow
// workflowID is waiting on. The host authenticates the decision maker. A
// decision sent before the run asks is kept until it does.
func (e *Engine) Decide(workflowID, interruptID string, decision types.ApprovalDecision) error {
	return dbos.Send(e.dctx, workflowID, approvalMessage{Decided: true, Decision: decision}, approvalTopic(interruptID))
}

// PendingApproval returns the approval request workflow workflowID is waiting
// on, waiting up to timeout for the run to publish one. ok is false, with a nil
// error, when the run is not waiting: it never asked for approval within
// timeout, or its last request was already decided.
func (e *Engine) PendingApproval(workflowID string, timeout time.Duration) (req types.ApprovalRequest, ok bool, err error) {
	return pendingResult(dbos.GetEvent[types.ApprovalRequest](e.dctx, workflowID, PendingApprovalEvent, timeout))
}

// pendingResult maps a GetEvent result to PendingApproval's. GetEvent reports
// an event that was never set as a timeout error; for PendingApproval that is
// the normal "not waiting" state, not a failure.
func pendingResult(req types.ApprovalRequest, err error) (types.ApprovalRequest, bool, error) {
	if errors.Is(err, &dbos.DBOSError{Code: dbos.TimeoutError}) {
		return types.ApprovalRequest{}, false, nil
	}
	if err != nil {
		return types.ApprovalRequest{}, false, err
	}
	return req, req.ID != "", nil
}

// Retrieve reattaches to an in-flight or completed run by workflow ID, e.g. from
// another process, to await its result.
func (e *Engine) Retrieve(workflowID string) (dbos.WorkflowHandle[RunOutput], error) {
	return dbos.RetrieveWorkflow[RunOutput](e.dctx, workflowID)
}
