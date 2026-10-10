package duraturo

import (
	"context"
	"errors"
	"fmt"
	"time"

	dt "github.com/urmzd/duraturo"

	"github.com/urmzd/saige/agent/batch"
	"github.com/urmzd/saige/agent/internal/durablecodec"
	"github.com/urmzd/saige/agent/types"
)

// BatchEvent is the event a batch job's end is signaled on.
func BatchEvent(jobID string) string { return "saige.batch:" + jobID }

// AwaitBatch submits (or resumes) batch job jobID from a workflow body and
// parks the run until the job ends, so a batch that takes hours holds no
// worker. Run WatchBatches next to the workers: it polls the vendor and
// signals the run when its job ends.
//
// The submit and the results are recorded steps. A replay neither submits
// again nor reads the results again, and the runner's job record makes a
// crash inside the submit step resume the same batch (see batch.Runner).
// Results are settled against the runner's budget once, in the results
// step. A result error is kept as its message.
//
// An indeterminate submit returns an error matching ErrIndeterminate; under
// an Engine-registered agent workflow the run parks until the job is
// reconciled with batch.Runner.Reconcile.
func (e *Engine) AwaitBatch(ctx context.Context, jobs *batch.Runner, jobID string, reqs []types.BatchRequest) ([]types.BatchResult, error) {
	info, ok := dt.FromContext(ctx)
	if !ok {
		return nil, errors.New("AwaitBatch called outside a durable run")
	}
	if _, err := dt.Step(ctx, "saige.batch.submit:"+jobID, func(ctx context.Context) (string, error) {
		if _, err := jobs.Submit(ctx, jobID, reqs); err != nil {
			if errors.Is(err, batch.ErrIndeterminate) {
				return "", fmt.Errorf("%w: %w", ErrIndeterminate, err)
			}
			return "", err
		}
		job, err := jobs.SetOwner(ctx, jobID, info.RunID)
		if err != nil {
			return "", err
		}
		if job.State.Open() {
			return string(job.State), nil
		}
		// Already ended: no watcher will see it end, so wake the run here.
		return string(job.State), e.client.Signal(ctx, info.RunID, BatchEvent(jobID), job.Status)
	}); err != nil {
		return nil, err
	}
	if _, err := dt.Event[types.BatchStatus](ctx, BatchEvent(jobID)); err != nil {
		return nil, err
	}
	raw, err := dt.Step(ctx, "saige.batch.results:"+jobID, func(ctx context.Context) ([]byte, error) {
		results, err := jobs.Collect(ctx, jobID, reqs)
		if err != nil {
			return nil, err
		}
		recorded := make([]recordedResult, len(results))
		for i, r := range results {
			msg, err := durablecodec.NewAssistant(r.Message)
			if err != nil {
				return nil, err
			}
			recorded[i] = recordedResult{CustomID: r.CustomID, Outcome: r.Outcome, Message: msg,
				FinishReason: r.FinishReason, Usage: r.Usage}
			if r.Err != nil {
				recorded[i].Err = r.Err.Error()
			}
		}
		return encode(recorded)
	})
	if err != nil {
		return nil, err
	}
	var recorded []recordedResult
	if err := decode(raw, &recorded); err != nil {
		return nil, dt.NonRetryable(fmt.Errorf("decode batch results: %w", err))
	}
	out := make([]types.BatchResult, len(recorded))
	for i, r := range recorded {
		msg, err := r.Message.Message()
		if err != nil {
			return nil, dt.NonRetryable(fmt.Errorf("decode batch results: %w", err))
		}
		out[i] = types.BatchResult{CustomID: r.CustomID, Outcome: r.Outcome, Message: msg,
			FinishReason: r.FinishReason, Usage: r.Usage}
		if r.Err != "" {
			out[i].Err = &types.BatchRequestError{Outcome: r.Outcome, Message: r.Err}
		}
	}
	return out, nil
}

// recordedResult is the gob form of a result in the ledger. Message reads
// the form earlier releases wrote too.
type recordedResult struct {
	CustomID     string
	Outcome      types.BatchOutcome
	Message      durablecodec.Assistant
	FinishReason string
	Usage        types.UsageDelta
	Err          string
}

// WatchBatches polls the runner's open jobs every interval until ctx ends
// and signals each ended job's owner run, which AwaitBatch parked. It is
// safe to run in several processes: a run awaits each job's event once, and
// a second signal is ignored.
func (e *Engine) WatchBatches(ctx context.Context, jobs *batch.Runner, every time.Duration) error {
	return jobs.Watch(ctx, every, func(ctx context.Context, j batch.Job) {
		if j.Owner == "" {
			return
		}
		_ = e.client.Signal(ctx, j.Owner, BatchEvent(j.ID), j.Status)
	})
}
