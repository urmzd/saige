package batch

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"strconv"
	"time"

	"github.com/urmzd/saige/agent/types"
)

var (
	// ErrManifestMismatch reports a Submit whose requests differ from those
	// the job was first submitted with.
	ErrManifestMismatch = errors.New("batch: job exists with different requests")
	// ErrIndeterminate reports a job whose submit was interrupted and whose
	// batch the vendor cannot confirm or rule out. Check the vendor's
	// console, then call Reconcile with the batch handle or permit a
	// resubmit (D-14).
	ErrIndeterminate = errors.New("batch: submit outcome requires reconciliation")
	// ErrSubmitIncomplete reports a job whose last submit did not finish.
	// Call Submit again with the same requests to resume it.
	ErrSubmitIncomplete = errors.New("batch: job submit did not finish; call Submit again")
	// ErrJobFailed reports a job that failed as a whole.
	ErrJobFailed = errors.New("batch: job failed")
	// ErrNotSubmitted marks a Submit error that proves no batch was created,
	// such as a failed input upload before the create call. A provider
	// wraps it so a Runner fails the job instead of looking for a batch.
	ErrNotSubmitted = errors.New("batch: not submitted")
	// ErrNotEnded reports a Results call before the batch ended.
	ErrNotEnded = errors.New("batch: job has not ended")
)

// DefaultChannel is the notifier channel a Runner publishes job endings on.
const DefaultChannel = "saige.batch"

// Runner manages durable batch jobs on one BatchProvider. It is safe for
// concurrent use. See the package documentation for the guarantees.
type Runner struct {
	provider types.BatchProvider
	store    Store

	budget   *types.Budget
	pricing  *types.Pricing
	notifier types.Notifier
	channel  string
	minPoll  time.Duration
	maxPoll  time.Duration
	logger   *slog.Logger
	now      func() time.Time
	// lookupSlack widens a lookup window for clock skew between this host
	// and the vendor.
	lookupSlack time.Duration
}

// RunnerOption configures a Runner.
type RunnerOption func(*Runner)

// WithBudget reserves each request at submit and settles it on its result.
// PerCallCost and PerCallTokens of the budget's policy bound one request.
func WithBudget(b *types.Budget) RunnerOption { return func(r *Runner) { r.budget = b } }

// WithPricing sets the interactive rate card the budget is charged from;
// the batch rates are derived from it with types.Pricing.Batch. Without it
// the provider's catalog row is used when the provider reports one through
// a Capabilities method, as the adapters do.
func WithPricing(p types.Pricing) RunnerOption { return func(r *Runner) { r.pricing = &p } }

// WithNotifier publishes job endings on channel (DefaultChannel when empty)
// and lets Wait wake on them instead of waiting out its poll interval.
func WithNotifier(n types.Notifier, channel string) RunnerOption {
	return func(r *Runner) {
		r.notifier = n
		if channel != "" {
			r.channel = channel
		}
	}
}

// WithPollInterval sets Wait's first and longest poll interval. The interval
// doubles after each poll that finds the job still running. Defaults: 5s and
// 2m.
func WithPollInterval(minimum, maximum time.Duration) RunnerOption {
	return func(r *Runner) {
		if minimum > 0 {
			r.minPoll = minimum
		}
		if maximum >= r.minPoll {
			r.maxPoll = maximum
		}
	}
}

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) RunnerOption {
	return func(r *Runner) {
		if l != nil {
			r.logger = l
		}
	}
}

// NewRunner returns a runner for jobs on p, recorded in store.
func NewRunner(p types.BatchProvider, store Store, opts ...RunnerOption) *Runner {
	r := &Runner{provider: p, store: store, channel: DefaultChannel,
		minPoll: 5 * time.Second, maxPoll: 2 * time.Minute, logger: slog.Default(),
		now: time.Now, lookupSlack: 5 * time.Minute}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Store returns the runner's job store.
func (r *Runner) Store() Store { return r.store }

// Provider returns the runner's batch provider.
func (r *Runner) Provider() types.BatchProvider { return r.provider }

func (r *Runner) identity() (string, string) {
	return types.ProviderName(asProvider(r.provider)), types.ProviderModel(asProvider(r.provider))
}

// asProvider lets the name and model helpers read a BatchProvider that also
// reports them.
func asProvider(p types.BatchProvider) types.Provider {
	if pp, ok := p.(types.Provider); ok {
		return pp
	}
	return namedOnly{p}
}

type namedOnly struct{ p types.BatchProvider }

func (namedOnly) ChatStream(context.Context, []types.Message, []types.ToolDef) (<-chan types.Delta, error) {
	return nil, errors.New("batch provider")
}

func (n namedOnly) Name() string {
	if np, ok := n.p.(interface{ Name() string }); ok {
		return np.Name()
	}
	return "unknown"
}

func (n namedOnly) Model() string {
	if mp, ok := n.p.(interface{ Model() string }); ok {
		return mp.Model()
	}
	return ""
}

// Submit starts job id with reqs, or resumes it. The job record is saved
// before the vendor sees anything, so calling Submit again after a crash,
// with the same ID and requests, resumes the batch the first call created
// instead of creating another. Requests that differ from the recorded
// manifest fail with ErrManifestMismatch.
//
// A submit that was interrupted before its batch ID was saved is resolved
// with types.BatchFinder where the provider implements it; otherwise the job
// becomes JobIndeterminate and Submit returns ErrIndeterminate.
func (r *Runner) Submit(ctx context.Context, id string, reqs []types.BatchRequest) (Job, error) {
	if id == "" {
		return Job{}, errors.New("batch: job ID required")
	}
	if err := CheckIDs(reqs); err != nil {
		return Job{}, err
	}
	provider, model := r.identity()
	manifest, err := Manifest(provider, model, reqs)
	if err != nil {
		return Job{}, err
	}
	now := r.now()
	ids := make([]string, len(reqs))
	for i, q := range reqs {
		ids[i] = q.CustomID
	}
	job := Job{ID: id, Provider: provider, Model: model, Manifest: manifest, CustomIDs: ids,
		Prefix: wirePrefix(id, manifest), State: JobSubmitting, CreatedAt: now, UpdatedAt: now}
	stored, created, err := r.store.Create(ctx, job)
	if err != nil {
		return Job{}, err
	}
	if !created && stored.Manifest != manifest {
		return stored, fmt.Errorf("%w: %s", ErrManifestMismatch, id)
	}
	if err := r.reserve(stored); err != nil {
		if created {
			stored.State, stored.Error = JobFailed, err.Error()
			_, _ = r.update(ctx, stored)
		}
		return stored, err
	}
	switch stored.State {
	case JobSubmitting:
		if !created && !stored.Resubmit {
			return r.recoverSubmit(ctx, stored, reqs)
		}
		return r.send(ctx, stored, reqs)
	case JobSubmitted:
		if _, ok := r.provider.(Ephemeral); ok {
			if _, err := r.provider.Status(ctx, *stored.Handle); errors.Is(err, types.ErrBatchNotFound) {
				r.logger.Info("batch lost with its process; resubmitting", "job", id)
				return r.send(ctx, stored, reqs)
			}
		}
		return stored, nil
	case JobIndeterminate:
		return stored, fmt.Errorf("%w: job %s", ErrIndeterminate, id)
	case JobFailed:
		r.release(stored, nil)
		return stored, fmt.Errorf("%w: %s: %s", ErrJobFailed, id, stored.Error)
	case JobCollected:
		r.release(stored, nil)
	}
	return stored, nil
}

// recoverSubmit resolves a job whose submit started without saving a batch
// ID: the vendor may or may not have created the batch.
func (r *Runner) recoverSubmit(ctx context.Context, job Job, reqs []types.BatchRequest) (Job, error) {
	if job.Attempts == 0 {
		// The record was saved but no submit started.
		return r.send(ctx, job, reqs)
	}
	finder, ok := r.provider.(types.BatchFinder)
	if !ok {
		return r.indeterminate(ctx, job, "the provider cannot look up a batch after a crash")
	}
	wire := make([]string, len(job.CustomIDs))
	for i := range wire {
		wire[i] = wireID(job.Prefix, i)
	}
	h, found, err := finder.FindBatch(ctx, types.BatchQuery{Tag: job.Prefix, Since: job.SubmitStartedAt.Add(-r.lookupSlack),
		Count: len(job.CustomIDs), CustomIDs: wire})
	switch {
	case errors.Is(err, types.ErrBatchAmbiguous):
		return r.indeterminate(ctx, job, err.Error())
	case err != nil:
		return job, err
	case found:
		job.Handle, job.State, job.Error = &h, JobSubmitted, ""
		r.logger.Info("batch found after an interrupted submit", "job", job.ID, "batch", h.ID)
		return r.update(ctx, job)
	default:
		return r.send(ctx, job, reqs)
	}
}

func (r *Runner) indeterminate(ctx context.Context, job Job, why string) (Job, error) {
	job.State, job.Error = JobIndeterminate, why
	stored, err := r.update(ctx, job)
	if err != nil {
		return job, err
	}
	return stored, fmt.Errorf("%w: job %s: %s", ErrIndeterminate, job.ID, why)
}

// send records the attempt, then submits.
func (r *Runner) send(ctx context.Context, job Job, reqs []types.BatchRequest) (Job, error) {
	job.State, job.Resubmit, job.Error = JobSubmitting, false, ""
	job.Attempts++
	job.SubmitStartedAt = r.now()
	job.Handle = nil
	job, err := r.update(ctx, job)
	if err != nil {
		return job, err
	}
	wire := make([]types.BatchRequest, len(reqs))
	for i, q := range reqs {
		q.CustomID = wireID(job.Prefix, i)
		wire[i] = q
	}
	h, err := r.provider.Submit(ctx, wire, types.BatchSubmitOptions{Tag: job.Prefix})
	if err != nil {
		if definite(err) {
			job.State, job.Error = JobFailed, err.Error()
			r.release(job, nil)
			if stored, uerr := r.update(ctx, job); uerr == nil {
				job = stored
			}
			return job, err
		}
		// The vendor may have created the batch. Leave the job submitting
		// for recoverSubmit to resolve on the next Submit.
		return job, fmt.Errorf("%w: %w", ErrSubmitIncomplete, err)
	}
	job.Handle, job.State = &h, JobSubmitted
	job.Status = types.BatchStatus{State: types.BatchPending, Counts: types.BatchCounts{Total: len(reqs)}}
	return r.update(ctx, job)
}

// definite reports a submit error that proves no batch was created: a local
// validation error, or a rejection by the vendor.
func definite(err error) bool {
	if errors.Is(err, types.ErrInvalidModelConfig) || errors.Is(err, ErrDuplicateID) || errors.Is(err, ErrNotSubmitted) {
		return true
	}
	var pe *types.ProviderError
	if errors.As(err, &pe) {
		switch pe.Kind {
		case types.ErrorKindInvalidRequest, types.ErrorKindAuth, types.ErrorKindRateLimit, types.ErrorKindContextLength:
			return true
		}
	}
	return false
}

func (r *Runner) update(ctx context.Context, job Job) (Job, error) {
	job.UpdatedAt = r.now()
	return r.store.Update(ctx, job)
}

// Get returns the job record.
func (r *Runner) Get(ctx context.Context, id string) (Job, error) { return r.store.Get(ctx, id) }

// Refresh polls the vendor once for an open job and saves what it reports.
// A job that reached a terminal state is published on the notifier.
func (r *Runner) Refresh(ctx context.Context, id string) (Job, error) {
	for {
		job, err := r.store.Get(ctx, id)
		if err != nil {
			return job, err
		}
		switch job.State {
		case JobSubmitting:
			return job, fmt.Errorf("%w: %s", ErrSubmitIncomplete, id)
		case JobIndeterminate:
			return job, fmt.Errorf("%w: job %s", ErrIndeterminate, id)
		case JobSubmitted:
		default:
			return job, nil
		}
		st, err := r.provider.Status(ctx, *job.Handle)
		if err != nil {
			return job, err
		}
		job.Status = st
		if st.State.Terminal() {
			job.State = JobEnded
			if st.State == types.BatchFailed {
				job.State, job.Error = JobFailed, st.Error
				r.release(job, nil)
			}
		}
		stored, err := r.update(ctx, job)
		if errors.Is(err, ErrJobConflict) {
			continue // another poller saved first; read again
		}
		if err != nil {
			return job, err
		}
		if !stored.State.Open() {
			r.publish(ctx, stored)
		}
		return stored, nil
	}
}

func (r *Runner) publish(ctx context.Context, job Job) {
	if r.notifier == nil {
		return
	}
	if err := r.notifier.Publish(ctx, r.channel, []byte(job.ID)); err != nil {
		r.logger.Warn("batch: notify", "job", job.ID, "error", err)
	}
}

// Wait polls until the job leaves the vendor: ended, failed or collected.
// It polls with backoff and wakes early when the notifier reports the job.
func (r *Runner) Wait(ctx context.Context, id string) (Job, error) {
	var wake <-chan types.Notification
	if r.notifier != nil {
		ch, cancel, err := r.notifier.Subscribe(ctx, r.channel)
		if err == nil {
			defer cancel()
			wake = ch
		}
	}
	interval := r.minPoll
	for {
		job, err := r.Refresh(ctx, id)
		if err != nil || !job.State.Open() {
			return job, err
		}
		var finished <-chan struct{}
		if f, ok := r.provider.(finisher); ok && job.Handle != nil {
			finished = f.finished(*job.Handle)
		}
		timer := time.NewTimer(interval)
	wait:
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				return job, ctx.Err()
			case <-timer.C:
				break wait
			case <-finished:
				timer.Stop()
				break wait
			case n, ok := <-wake:
				if !ok {
					wake = nil
					continue
				}
				if string(n.Payload) == id {
					timer.Stop()
					break wait
				}
			}
		}
		interval = min(interval*2, r.maxPoll)
	}
}

// Results streams the job's results with the caller's custom IDs, settling
// the budget for each. A job that ended with requests missing from the
// vendor's results reports them errored, charged as uncertain. Reading every
// result marks the job JobCollected.
func (r *Runner) Results(ctx context.Context, id string) iter.Seq2[types.BatchResult, error] {
	return func(yield func(types.BatchResult, error) bool) {
		job, err := r.store.Get(ctx, id)
		if err != nil {
			yield(types.BatchResult{}, err)
			return
		}
		switch {
		case job.State == JobFailed:
			yield(types.BatchResult{}, fmt.Errorf("%w: %s: %s", ErrJobFailed, id, job.Error))
			return
		case job.State != JobEnded && job.State != JobCollected:
			yield(types.BatchResult{}, fmt.Errorf("%w: %s is %s", ErrNotEnded, id, job.State))
			return
		}
		seen := make([]bool, len(job.CustomIDs))
		pricing := r.batchPricing()
		for res, err := range r.provider.Results(ctx, *job.Handle) {
			if err != nil {
				yield(types.BatchResult{}, err)
				return
			}
			i, ok := wireIndex(job.Prefix, res.CustomID, len(job.CustomIDs))
			if !ok {
				yield(types.BatchResult{}, fmt.Errorf("batch: job %s: result for unknown request %q", id, res.CustomID))
				return
			}
			if seen[i] {
				continue
			}
			seen[i] = true
			res.CustomID = job.CustomIDs[i]
			r.settle(job, i, pricing, res, false)
			if !yield(res, nil) {
				return
			}
		}
		for i, ok := range seen {
			if ok {
				continue
			}
			res := types.BatchResult{CustomID: job.CustomIDs[i], Outcome: types.BatchErrored,
				Err: &types.BatchRequestError{Outcome: types.BatchErrored, Message: "missing from the vendor's results"}}
			r.settle(job, i, pricing, res, true)
			if !yield(res, nil) {
				return
			}
		}
		if job.State == JobEnded {
			job.State = JobCollected
			if _, err := r.update(ctx, job); err != nil && !errors.Is(err, ErrJobConflict) {
				r.logger.Warn("batch: mark collected", "job", id, "error", err)
			}
		}
	}
}

// Run submits (or resumes) the job, waits for it and returns its results in
// request order.
func (r *Runner) Run(ctx context.Context, id string, reqs []types.BatchRequest) ([]types.BatchResult, error) {
	if _, err := r.Submit(ctx, id, reqs); err != nil {
		return nil, err
	}
	if _, err := r.Wait(ctx, id); err != nil {
		return nil, err
	}
	return r.Collect(ctx, id, reqs)
}

// Collect reads all of a job's results, ordered as reqs.
func (r *Runner) Collect(ctx context.Context, id string, reqs []types.BatchRequest) ([]types.BatchResult, error) {
	index := make(map[string]int, len(reqs))
	for i, q := range reqs {
		index[q.CustomID] = i
	}
	out := make([]types.BatchResult, len(reqs))
	for res, err := range r.Results(ctx, id) {
		if err != nil {
			return nil, err
		}
		if i, ok := index[res.CustomID]; ok {
			out[i] = res
		}
	}
	return out, nil
}

// Cancel asks the vendor to stop the job's batch and refreshes the record.
func (r *Runner) Cancel(ctx context.Context, id string) (Job, error) {
	job, err := r.store.Get(ctx, id)
	if err != nil {
		return job, err
	}
	if job.State != JobSubmitted {
		return job, fmt.Errorf("batch: job %s is %s; only a submitted job can be canceled", id, job.State)
	}
	if err := r.provider.Cancel(ctx, *job.Handle); err != nil {
		return job, err
	}
	return r.Refresh(ctx, id)
}

// Resolution is the host's decision for an indeterminate job.
type Resolution struct {
	// Handle is the batch the host found at the vendor. The job resumes it.
	Handle *types.BatchHandle
	// Resubmit permits the next Submit to send the batch again. Choose it
	// only after confirming the vendor has no batch for the job, or a
	// duplicate batch is billed.
	Resubmit bool
}

// Reconcile resolves a job left JobIndeterminate or JobSubmitting.
func (r *Runner) Reconcile(ctx context.Context, id string, res Resolution) (Job, error) {
	if (res.Handle == nil) == !res.Resubmit {
		return Job{}, errors.New("batch: reconcile needs exactly one of a handle and resubmit")
	}
	job, err := r.store.Get(ctx, id)
	if err != nil {
		return job, err
	}
	if job.State != JobIndeterminate && job.State != JobSubmitting {
		return job, fmt.Errorf("batch: job %s is %s; nothing to reconcile", id, job.State)
	}
	job.Error = ""
	if res.Handle != nil {
		h := *res.Handle
		job.Handle, job.State = &h, JobSubmitted
	} else {
		job.State, job.Resubmit = JobSubmitting, true
	}
	return r.update(ctx, job)
}

// Sweep refreshes every submitted job once and returns those that ended.
func (r *Runner) Sweep(ctx context.Context) ([]Job, error) {
	jobs, err := r.store.List(ctx, JobSubmitted)
	if err != nil {
		return nil, err
	}
	var ended []Job
	var errs []error
	for _, j := range jobs {
		got, err := r.Refresh(ctx, j.ID)
		if err != nil {
			errs = append(errs, fmt.Errorf("job %s: %w", j.ID, err))
			continue
		}
		if !got.State.Open() {
			ended = append(ended, got)
		}
	}
	return ended, errors.Join(errs...)
}

// Watch runs Sweep every interval until ctx ends, calling onEnded for each
// job that ended. It is the background poller for jobs no caller is waiting
// on, such as those a durable workflow parked on.
func (r *Runner) Watch(ctx context.Context, every time.Duration, onEnded func(context.Context, Job)) error {
	if every <= 0 {
		every = r.minPoll
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		ended, err := r.Sweep(ctx)
		if err != nil && ctx.Err() == nil {
			r.logger.Warn("batch: sweep", "error", err)
		}
		for _, j := range ended {
			if onEnded != nil {
				onEnded(ctx, j)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// SetOwner records who waits on the job, for a watcher to wake.
func (r *Runner) SetOwner(ctx context.Context, id, owner string) (Job, error) {
	for {
		job, err := r.store.Get(ctx, id)
		if err != nil || job.Owner == owner {
			return job, err
		}
		job.Owner = owner
		stored, err := r.update(ctx, job)
		if errors.Is(err, ErrJobConflict) {
			continue
		}
		return stored, err
	}
}

// ── budget ──────────────────────────────────────────────────────────

func reservationID(job Job, i int) string { return "batch:" + job.ID + ":" + strconv.Itoa(i) }

// batchPricing is the rate card batch requests are charged at.
func (r *Runner) batchPricing() types.Pricing {
	var p types.Pricing
	switch {
	case r.pricing != nil:
		p = *r.pricing
	default:
		if cr, ok := r.provider.(interface {
			Capabilities() types.ModelCapabilities
		}); ok {
			p = cr.Capabilities().Pricing
		}
	}
	if _, ok := r.provider.(interactive); ok {
		return p
	}
	return p.Batch()
}

// reserve holds budget for every request of the job not yet settled. A
// resumed job is reserved again, since budgets are process-local.
func (r *Runner) reserve(job Job) error {
	if r.budget == nil || job.State == JobCollected || job.State == JobFailed {
		return nil
	}
	pricing := r.batchPricing()
	var held []string
	for i := range job.CustomIDs {
		id := reservationID(job, i)
		if r.budget.Receipt(id).ID != "" {
			continue // settled earlier in this process
		}
		if _, err := r.budget.Reserve(id, pricing); err != nil {
			if errors.Is(err, types.ErrReservationActive) {
				continue
			}
			for _, h := range held {
				_ = r.budget.Settle(h, job.Model, pricing, types.TokenUsage{}, false)
			}
			return fmt.Errorf("batch: reserve budget for %d requests: %w", len(job.CustomIDs), err)
		}
		held = append(held, id)
	}
	return nil
}

// release settles every unsettled reservation of the job at zero: nothing
// was billed. skip lists indices already settled.
func (r *Runner) release(job Job, skip []bool) {
	if r.budget == nil {
		return
	}
	pricing := r.batchPricing()
	for i := range job.CustomIDs {
		if skip != nil && skip[i] {
			continue
		}
		_ = r.budget.Settle(reservationID(job, i), job.Model, pricing, types.TokenUsage{}, false)
	}
}

// settle charges one result: its usage when the vendor bills it, nothing
// otherwise, and the whole reservation when the outcome is unknown.
func (r *Runner) settle(job Job, i int, pricing types.Pricing, res types.BatchResult, unknown bool) {
	if r.budget == nil {
		return
	}
	id := reservationID(job, i)
	var usage types.TokenUsage
	if res.Outcome.Billed() {
		usage = types.UsageFromDelta(res.Usage)
	}
	err := r.budget.Settle(id, job.Model, pricing, usage, unknown)
	if errors.Is(err, types.ErrUnknownReservation) {
		// Reserved by an earlier process; record the charge once here.
		if usage.Total() > 0 {
			_, err = r.budget.RecordOnce(id, job.Model, pricing, usage)
		} else {
			err = nil
		}
	}
	if err != nil {
		r.logger.Warn("batch: settle", "job", job.ID, "request", res.CustomID, "error", err)
	}
}
