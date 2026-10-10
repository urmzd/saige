package batch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"iter"
	"sync"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// Local is a types.BatchProvider that runs each request through an ordinary
// provider, at most Concurrency at a time. It gives runtimes without a batch
// API, such as Ollama, the same interface, so a job written for a vendor
// batch runs unchanged against a local model.
//
// A local batch lives in this process. After a restart its handle is
// unknown (Status returns types.ErrBatchNotFound) and a Runner submits the
// requests again, which repeats the calls: cheap for a local model, billed
// again for a remote one wrapped in Local.
type Local struct {
	provider    types.Provider
	concurrency int

	mu   sync.Mutex
	jobs map[string]*localJob
}

var (
	_ types.BatchProvider = (*Local)(nil)
	_ Ephemeral           = (*Local)(nil)
	_ interactive         = (*Local)(nil)
)

// finisher reports a batch's end as a channel.
type finisher interface {
	finished(types.BatchHandle) <-chan struct{}
}

// interactive marks a batch provider billed at interactive rates.
type interactive interface{ interactivePricing() }

// Ephemeral is implemented by batch providers whose batches do not outlive
// the process. A Runner resubmits a job whose ephemeral batch was lost.
type Ephemeral interface {
	Ephemeral() bool
}

// NewLocal returns a local batch provider over p. Concurrency below 1 means 4.
func NewLocal(p types.Provider, concurrency int) *Local {
	if concurrency < 1 {
		concurrency = 4
	}
	return &Local{provider: p, concurrency: concurrency, jobs: map[string]*localJob{}}
}

// Ephemeral implements Ephemeral.
func (l *Local) Ephemeral() bool { return true }

// Name returns the wrapped provider's name.
func (l *Local) Name() string { return types.ProviderName(l.provider) }

// Model returns the wrapped provider's model.
func (l *Local) Model() string { return types.ProviderModel(l.provider) }

// Capabilities forwards the wrapped provider's declaration, so a Runner
// prices the batch from the same catalog row. A local batch makes ordinary
// calls, so it is priced at the interactive rates, never the batch ones.
func (l *Local) Capabilities() types.ModelCapabilities {
	caps, _ := types.ProviderCapabilities(l.provider)
	return caps
}

func (l *Local) interactivePricing() {}

// finished returns a channel closed when the batch ends, so a Runner waits
// on a local batch without polling.
func (l *Local) finished(h types.BatchHandle) <-chan struct{} {
	j, err := l.job(h)
	if err != nil {
		return nil
	}
	return j.finish
}

type localJob struct {
	cancel  context.CancelFunc
	created time.Time

	mu      sync.Mutex
	results []types.BatchResult
	done    []bool
	ended   time.Time
	cancels bool
	finish  chan struct{}
}

// Submit implements types.BatchProvider. A request the wrapped provider
// cannot express is rejected before any call: a schema needs
// types.StructuredOutputProvider, options need types.OptionsProvider, and
// the two cannot be combined, since no streaming entry point carries both.
func (l *Local) Submit(ctx context.Context, reqs []types.BatchRequest, _ types.BatchSubmitOptions) (types.BatchHandle, error) {
	if err := CheckIDs(reqs); err != nil {
		return types.BatchHandle{}, err
	}
	for i, r := range reqs {
		if err := l.check(r); err != nil {
			return types.BatchHandle{}, fmt.Errorf("request %d (%s): %w", i, r.CustomID, err)
		}
	}
	id := "local_" + randomID()
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	job := &localJob{cancel: cancel, created: time.Now(), results: make([]types.BatchResult, len(reqs)),
		done: make([]bool, len(reqs)), finish: make(chan struct{})}
	l.mu.Lock()
	l.jobs[id] = job
	l.mu.Unlock()

	go func() {
		defer cancel()
		sem := make(chan struct{}, l.concurrency)
		var wg sync.WaitGroup
		for i, r := range reqs {
			select {
			case sem <- struct{}{}:
			case <-runCtx.Done():
			}
			if runCtx.Err() != nil {
				break
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				res := l.run(runCtx, r)
				job.mu.Lock()
				job.results[i], job.done[i] = res, true
				job.mu.Unlock()
			}()
		}
		wg.Wait()
		job.mu.Lock()
		for i, r := range reqs {
			if !job.done[i] {
				job.results[i] = types.BatchResult{CustomID: r.CustomID, Outcome: types.BatchCanceledOutcome,
					Err: &types.BatchRequestError{Outcome: types.BatchCanceledOutcome}}
				job.done[i] = true
			}
		}
		job.ended = time.Now()
		job.mu.Unlock()
		close(job.finish)
	}()
	return types.BatchHandle{Provider: l.Name(), Model: l.Model(), ID: id}, nil
}

func (l *Local) check(r types.BatchRequest) error {
	hasOpts := len(r.Options.OptionNames()) > 0 || r.Options.HasDials()
	if r.Schema != nil {
		if !types.AcceptsSchema(l.provider) {
			return types.ErrSchemaUnsupported
		}
		if hasOpts {
			return fmt.Errorf("%w: a local batch cannot send a response schema together with request options", types.ErrOptionsUnsupported)
		}
	}
	if hasOpts {
		if !types.AcceptsOptions(l.provider) {
			return types.ErrOptionsUnsupported
		}
	}
	return nil
}

func (l *Local) run(ctx context.Context, r types.BatchRequest) types.BatchResult {
	if ctx.Err() != nil {
		return types.BatchResult{CustomID: r.CustomID, Outcome: types.BatchCanceledOutcome,
			Err: &types.BatchRequestError{Outcome: types.BatchCanceledOutcome}}
	}
	var (
		stream <-chan types.Delta
		err    error
	)
	req := types.Request{Messages: r.Messages, Tools: r.Tools, Schema: r.Schema}
	if r.Schema == nil && (len(r.Options.OptionNames()) > 0 || r.Options.HasDials()) {
		req.Options = new(r.Options)
	}
	stream, err = l.provider.Stream(ctx, req)
	if err != nil {
		return types.BatchResult{CustomID: r.CustomID, Outcome: types.BatchErrored,
			Err: &types.BatchRequestError{Outcome: types.BatchErrored, Message: err.Error(), Err: err}}
	}
	res := Collect(r.CustomID, stream)
	if res.Outcome == types.BatchErrored && ctx.Err() != nil {
		res.Outcome = types.BatchCanceledOutcome
		res.Err = &types.BatchRequestError{Outcome: types.BatchCanceledOutcome}
	}
	return res
}

func (l *Local) job(h types.BatchHandle) (*localJob, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	j, ok := l.jobs[h.ID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", types.ErrBatchNotFound, h.ID)
	}
	return j, nil
}

// Status implements types.BatchProvider.
func (l *Local) Status(_ context.Context, h types.BatchHandle) (types.BatchStatus, error) {
	j, err := l.job(h)
	if err != nil {
		return types.BatchStatus{}, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	st := types.BatchStatus{CreatedAt: j.created, EndedAt: j.ended, Counts: types.BatchCounts{Total: len(j.results)}}
	for i, done := range j.done {
		if !done {
			st.Counts.Processing++
			continue
		}
		switch j.results[i].Outcome {
		case types.BatchSucceeded:
			st.Counts.Succeeded++
		case types.BatchErrored:
			st.Counts.Errored++
		case types.BatchCanceledOutcome:
			st.Counts.Canceled++
		}
	}
	switch {
	case !j.ended.IsZero() && j.cancels:
		st.State = types.BatchCanceled
	case !j.ended.IsZero():
		st.State = types.BatchEnded
	case j.cancels:
		st.State = types.BatchCanceling
	default:
		st.State = types.BatchRunning
	}
	st.VendorState = string(st.State)
	return st, nil
}

// Results implements types.BatchProvider. It waits for the batch to end.
func (l *Local) Results(ctx context.Context, h types.BatchHandle) iter.Seq2[types.BatchResult, error] {
	return func(yield func(types.BatchResult, error) bool) {
		j, err := l.job(h)
		if err != nil {
			yield(types.BatchResult{}, err)
			return
		}
		select {
		case <-j.finish:
		case <-ctx.Done():
			yield(types.BatchResult{}, ctx.Err())
			return
		}
		j.mu.Lock()
		results := append([]types.BatchResult(nil), j.results...)
		j.mu.Unlock()
		for _, r := range results {
			if !yield(r, nil) {
				return
			}
		}
	}
}

// Cancel implements types.BatchProvider. Requests not yet started are
// canceled; running requests see their context end.
func (l *Local) Cancel(_ context.Context, h types.BatchHandle) error {
	j, err := l.job(h)
	if err != nil {
		return err
	}
	j.mu.Lock()
	j.cancels = true
	j.mu.Unlock()
	j.cancel()
	return nil
}

func randomID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
