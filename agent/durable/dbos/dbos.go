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
package dbos

import (
	"context"
	"encoding/gob"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/agent"
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
	gob.Register(types.StepResult{})
	gob.Register(types.SystemMessage{})
	gob.Register(types.UserMessage{})
	gob.Register(types.AssistantMessage{})
	gob.Register(types.TextContent{})
	gob.Register(types.ToolUseContent{})
	gob.Register(types.ThinkingContent{})
	gob.Register(types.ToolResultContent{})
	gob.Register(types.FileContent{})
	gob.Register(types.ConfigContent{})
	gob.Register(types.FeedbackContent{})
	gob.Register(types.HandoffContent{})
	// Tool-call Arguments are map[string]any decoded from JSON; nested arrays and
	// objects arrive as []interface{} / map[string]interface{} inside interface
	// values and must be registered or gob.Encode fails on real tool schemas.
	gob.Register([]interface{}{})
	gob.Register(map[string]interface{}{})
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

// RunInput is the single serializable workflow input.
type RunInput struct {
	Messages []types.Message
	Branch   types.BranchID
}

// RunOutput is the single serializable workflow output.
type RunOutput struct {
	Final *types.AssistantMessage
}

// Engine owns a DBOS context lifecycle and registers agent-run workflows.
type Engine struct {
	dctx dbos.DBOSContext
}

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

// RegisterAgent registers a durable workflow that runs the given agent to
// completion via Agent.RunDurable. It MUST be called before Launch. The returned
// Workflow value is passed to Run. name defaults to "saige.agent.run".
func (e *Engine) RegisterAgent(a *agent.Agent, name string) dbos.Workflow[RunInput, RunOutput] {
	if name == "" {
		name = "saige.agent.run"
	}
	wf := func(dctx dbos.DBOSContext, in RunInput) (RunOutput, error) {
		runner := NewRunner(dctx)
		final, err := a.RunDurable(context.Background(), runner, in.Messages, in.Branch)
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

// Retrieve reattaches to an in-flight or completed run by workflow ID, e.g. from
// another process, to await its result.
func (e *Engine) Retrieve(workflowID string) (dbos.WorkflowHandle[RunOutput], error) {
	return dbos.RetrieveWorkflow[RunOutput](e.dctx, workflowID)
}
