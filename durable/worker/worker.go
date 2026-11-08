// Package worker implements durable's consumer: the loop that claims runs
// from the queue, re-executes them under replay to their frontier, and
// persists exactly one outcome per claim. The worker is a loop, not a
// service — it exposes no listener and accepts no inbound calls. Every arrow
// points outward: to the ledger for truth, to the queue for flow. That is
// what lets any number of workers run anywhere without coordination beyond
// the two interfaces.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime"
	"sync"
	"time"

	"github.com/urmzd/saige/durable/ledger"
	"github.com/urmzd/saige/durable/queue"
	"github.com/urmzd/saige/durable/replay"
	"github.com/urmzd/saige/durable/run"
)

const (
	defaultLeaseTTL     = 30 * time.Second
	defaultJanitorEvery = 30 * time.Second
	defaultMaxAttempts  = 5

	backoffBase = 250 * time.Millisecond
	backoffCap  = 30 * time.Second

	// janitorBatch bounds one janitor sweep; the next tick takes the rest.
	janitorBatch = 256

	// claimRetryDelay paces a claim slot after an unexpected Claim error.
	claimRetryDelay = 250 * time.Millisecond
)

type options struct {
	leaseTTL       time.Duration
	heartbeatEvery time.Duration
	backoff        func(attempt int) time.Duration
	concurrency    int
	janitorEvery   time.Duration
	codec          run.Codec
	registry       *run.Registry
	logger         *slog.Logger
}

// Option configures a Worker.
type Option func(*options)

// WithLeaseTTL sets the claim lease duration (default 30s). Heartbeats renew
// it; a worker that stops heartbeating loses the run after at most this long.
func WithLeaseTTL(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.leaseTTL = d
		}
	}
}

// WithHeartbeatEvery sets the lease renewal interval (default LeaseTTL/3).
func WithHeartbeatEvery(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.heartbeatEvery = d
		}
	}
}

// WithBackoff sets the retry delay as a function of the failure count just
// reached. The default is exponential — 250ms·2^(n−1) capped at 30s — with
// ±20% jitter so a synchronized fleet spreads its retries.
func WithBackoff(fn func(attempt int) time.Duration) Option {
	return func(o *options) {
		if fn != nil {
			o.backoff = fn
		}
	}
}

// WithConcurrency sets the number of claim slots (default GOMAXPROCS).
func WithConcurrency(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.concurrency = n
		}
	}
}

// WithJanitorEvery sets the repair-loop interval (default 30s); 0 disables
// the janitor on this worker.
func WithJanitorEvery(d time.Duration) Option {
	return func(o *options) {
		if d < 0 {
			d = 0
		}
		o.janitorEvery = d
	}
}

// WithCodec overrides the default JSON codec. It must match the codec the
// submitting client used.
func WithCodec(c run.Codec) Option {
	return func(o *options) {
		if c != nil {
			o.codec = c
		}
	}
}

// WithRegistry sets the registry workflow names are resolved against
// (default run.DefaultRegistry).
func WithRegistry(r *run.Registry) Option {
	return func(o *options) {
		if r != nil {
			o.registry = r
		}
	}
}

// WithLogger sets the logger for warnings (default slog.Default()).
func WithLogger(l *slog.Logger) Option {
	return func(o *options) {
		if l != nil {
			o.logger = l
		}
	}
}

// Worker consumes runs. Each claim is driven through a fixed protocol —
// claim → load → replay to frontier → exactly one of Complete+Settle,
// Settle (park), or Release (retry) — with lease expiry covering the paths
// where the worker itself dies. Create with New, start with Run.
type Worker struct {
	lgr    ledger.Ledger
	q      queue.Queue
	deltas queue.DeltaLog // nil when q lacks the capability
	opts   options
}

// New composes a worker over any Ledger and Queue implementation. The
// queue's DeltaLog capability is discovered by type assertion; without it,
// attempt dividers are skipped and streams are simply not produced.
func New(lgr ledger.Ledger, q queue.Queue, opts ...Option) *Worker {
	o := options{
		leaseTTL:     defaultLeaseTTL,
		concurrency:  runtime.GOMAXPROCS(0),
		janitorEvery: defaultJanitorEvery,
		codec:        run.JSONCodec{},
		registry:     run.DefaultRegistry,
	}
	for _, opt := range opts {
		opt(&o)
	}
	if o.heartbeatEvery <= 0 {
		o.heartbeatEvery = o.leaseTTL / 3
	}
	if o.backoff == nil {
		o.backoff = defaultBackoff
	}
	if o.logger == nil {
		o.logger = slog.Default()
	}
	deltas, _ := q.(queue.DeltaLog)
	return &Worker{lgr: lgr, q: q, deltas: deltas, opts: o}
}

// Run blocks, consuming runs until ctx is done. It starts Concurrency claim
// loops plus, unless disabled, one janitor goroutine. On cancellation it
// stops claiming, finishes or releases every in-flight run (a claim that
// lands as it stops is released at once, unexecuted), waits for all
// goroutines, and returns ctx.Err().
func (w *Worker) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for range w.opts.concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.claimLoop(ctx)
		}()
	}
	if w.opts.janitorEvery > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.janitor(ctx)
		}()
	}
	wg.Wait()
	return ctx.Err()
}

// claimLoop is one claim slot: claim, process, repeat until ctx is done.
func (w *Worker) claimLoop(ctx context.Context) {
	for {
		it, _, err := w.q.Claim(ctx, w.opts.leaseTTL)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			w.opts.logger.Warn("durable/worker: claim failed", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(claimRetryDelay):
			}
			continue
		}
		if ctx.Err() != nil {
			// The claim landed as the worker stopped: a queue waits for a
			// claim it already sent rather than abandon it, so the lease is
			// ours. Hand it straight back, burning no budget, so the next
			// worker claims the run now instead of after the lease lapses.
			w.release(context.WithoutCancel(ctx), it, 0, false)
			return
		}
		w.process(ctx, it)
	}
}

// process drives one claimed item through the outcome protocol. Every path
// ends in exactly one queue verb — Settle for terminal and parked runs,
// Release for retries and shutdown — and lease expiry covers a worker that
// dies mid-flight.
//
// Outcome verbs run on a context detached from cancellation: a worker
// shutting down mid-outcome must still persist the outcome, or graceful
// shutdown degrades into a crash (leases wait out their TTL, computed
// terminal results are discarded and re-executed).
func (w *Worker) process(ctx context.Context, it queue.Item) {
	outCtx := context.WithoutCancel(ctx)

	r, records, err := w.lgr.Load(ctx, it.RunID)
	switch {
	case errors.Is(err, run.ErrNotFound):
		// Queue entry without a ledger row: flow without truth is noise.
		w.opts.logger.Warn("durable/worker: claimed run has no ledger row; settling",
			"run_id", it.RunID, "attempt", it.Attempt)
		w.settle(outCtx, it)
		return
	case err != nil:
		// A load aborted by shutdown is an interruption, not a failure of
		// the run — hand it back without burning budget.
		if ctx.Err() != nil {
			w.release(outCtx, it, 0, false)
			return
		}
		w.opts.logger.Warn("durable/worker: load failed; releasing",
			"run_id", it.RunID, "attempt", it.Attempt, "error", err)
		w.release(outCtx, it, w.opts.backoff(it.Failures+1), true)
		return
	}

	// Lost-settle repair: a previous attempt completed the run but died
	// before settling. The ledger already holds the truth; clear the queue.
	if r.Status.Terminal() {
		w.settle(outCtx, it)
		return
	}

	maxFailures := r.MaxAttempts
	if maxFailures <= 0 {
		maxFailures = defaultMaxAttempts
	}

	// Poison-run bound on FAILURES, not claims: crash-looping runs burn
	// budget through expiry reclaims, but park/resume cycles and janitor
	// backstop polls settle cleanly and cost nothing — a run may wait on an
	// event forever without being terminally failed for waiting.
	if it.Failures >= maxFailures {
		w.completeFailed(outCtx, it, run.ErrMaxAttempts.Error())
		w.settle(outCtx, it)
		return
	}

	fn, ok := w.opts.registry.Lookup(r.Name)
	if !ok {
		// A differently-versioned fleet may know this workflow: retry
		// while budget remains, fail terminally once it is spent.
		if it.Failures+1 >= maxFailures {
			w.completeFailed(outCtx, it, "unknown workflow "+r.Name)
			w.settle(outCtx, it)
			return
		}
		w.release(outCtx, it, w.opts.backoff(it.Failures+1), true)
		return
	}

	// Attempt divider: consumers split superseded partial streams here.
	// Best-effort — deltas are advisory flow, never truth.
	if w.deltas != nil {
		divider := run.Delta{RunID: it.RunID, Attempt: it.Attempt, Kind: run.DeltaAttempt}
		if aerr := w.deltas.Append(ctx, divider); aerr != nil {
			w.opts.logger.Warn("durable/worker: attempt divider append failed",
				"run_id", it.RunID, "attempt", it.Attempt, "error", aerr)
		}
	}

	frame := replay.NewFrame(it.RunID, it.Attempt, w.lgr, w.opts.codec, records, w.deltas)
	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Heartbeat until execution finishes. run.ErrSuperseded cancels the
	// execution context: the zombie learns it lost and stops.
	hbStop := make(chan struct{})
	hbDone := make(chan struct{})
	go w.heartbeat(execCtx, cancel, it, hbStop, hbDone)

	out, err := invoke(replay.WithFrame(execCtx, frame), fn, r.Input)
	close(hbStop)
	<-hbDone

	// Outcome, on the cancellation-detached context. Order matters:
	// sentinels before ctx.Err() — a run that parked or finished during
	// shutdown already has its outcome.
	switch {
	case err == nil:
		w.complete(outCtx, it, run.Result{Status: run.StatusSucceeded, Output: out})
		w.settle(outCtx, it)
		if keys := frame.Unconsumed(); len(keys) > 0 {
			w.opts.logger.Warn("durable/worker: unconsumed records (calls removed by a code change?); result stands",
				"run_id", it.RunID, "keys", keys)
		}
	case errors.Is(err, run.ErrParked):
		// Off the queue, still pending, zero retry burn: anything that
		// writes the event record and enqueues the run resumes it.
		w.settle(outCtx, it)
	case errors.Is(err, run.ErrNonDeterministic) || run.IsNonRetryable(err):
		w.completeFailed(outCtx, it, err.Error())
		w.settle(outCtx, it)
	case execCtx.Err() != nil && ctx.Err() != nil:
		// Worker shutdown interrupted the run: hand it straight back,
		// burning no budget — interruption is not a failure of the code.
		w.release(outCtx, it, 0, false)
	default:
		// Retryable — including panics, which consume budget like any
		// other genuine failure.
		if it.Failures+1 >= maxFailures {
			w.completeFailed(outCtx, it, run.ErrMaxAttempts.Error()+": "+err.Error())
			w.settle(outCtx, it)
			return
		}
		w.release(outCtx, it, w.opts.backoff(it.Failures+1), true)
	}
}

// heartbeat renews the item's lease every heartbeatEvery until stop closes.
// run.ErrSuperseded cancels execution — the fencing verdict that another
// attempt owns the run now. Other errors are logged and retried: a transient
// queue hiccup must not kill a healthy run.
func (w *Worker) heartbeat(ctx context.Context, cancel context.CancelFunc, it queue.Item, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(w.opts.heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := w.q.Heartbeat(ctx, it, w.opts.leaseTTL); err != nil {
			if errors.Is(err, run.ErrSuperseded) {
				cancel()
				return
			}
			w.opts.logger.Warn("durable/worker: heartbeat failed; will retry",
				"run_id", it.RunID, "attempt", it.Attempt, "error", err)
		}
	}
}

// janitor is the fenced, idempotent repair loop — safe on every worker. Each
// tick walks the ENTIRE pending set older than one lease, paginating with
// the ledger's cursor: parked runs stay pending indefinitely, so a fixed
// first page would starve newer lost runs behind them. Re-enqueueing
// recovers accepted-but-never-enqueued runs, rebuilds a wiped queue from
// ledger truth, and is the backstop poll for parked runs:
// replay-to-frontier is cheap, and a still-unsatisfied run simply re-parks.
func (w *Worker) janitor(ctx context.Context) {
	t := time.NewTicker(w.opts.janitorEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		before := time.Now().Add(-w.opts.leaseTTL)
		var cursor run.PendingCursor
		for {
			batch, err := w.lgr.ListPending(ctx, before, cursor, janitorBatch)
			if err != nil {
				w.opts.logger.Warn("durable/worker: janitor list pending failed", "error", err)
				break
			}
			for _, p := range batch {
				// Enqueue is a no-op for queued and leased runs, so
				// sweeping the whole pending set is harmless.
				if qerr := w.q.Enqueue(ctx, p.ID, 0); qerr != nil {
					w.opts.logger.Warn("durable/worker: janitor enqueue failed",
						"run_id", p.ID, "error", qerr)
				}
			}
			if len(batch) < janitorBatch {
				break
			}
			last := batch[len(batch)-1]
			cursor = run.PendingCursor{CreatedAt: last.CreatedAt, ID: last.ID}
		}
	}
}

// complete writes the terminal result, tolerating run.ErrAlreadyTerminal:
// first terminal write wins, and losing that race is a success condition.
func (w *Worker) complete(ctx context.Context, it queue.Item, res run.Result) {
	err := w.lgr.Complete(ctx, it.RunID, it.Attempt, res)
	if err != nil && !errors.Is(err, run.ErrAlreadyTerminal) {
		w.opts.logger.Warn("durable/worker: complete failed; janitor will re-drive the run",
			"run_id", it.RunID, "attempt", it.Attempt, "error", err)
	}
}

func (w *Worker) completeFailed(ctx context.Context, it queue.Item, msg string) {
	w.complete(ctx, it, run.Result{Status: run.StatusFailed, Error: msg})
}

// settle removes the claim, tolerating run.ErrSuperseded: if the lease
// lapsed, the successor attempt owns the queue state now.
func (w *Worker) settle(ctx context.Context, it queue.Item) {
	if err := w.q.Settle(ctx, it); err != nil && !errors.Is(err, run.ErrSuperseded) {
		w.opts.logger.Warn("durable/worker: settle failed",
			"run_id", it.RunID, "attempt", it.Attempt, "error", err)
	}
}

// release hands the claim back, tolerating run.ErrSuperseded like settle.
// failed says whether this execution consumes retry budget.
func (w *Worker) release(ctx context.Context, it queue.Item, delay time.Duration, failed bool) {
	if err := w.q.Release(ctx, it, delay, failed); err != nil && !errors.Is(err, run.ErrSuperseded) {
		w.opts.logger.Warn("durable/worker: release failed",
			"run_id", it.RunID, "attempt", it.Attempt, "error", err)
	}
}

// invoke executes the workflow function with panic recovery: a panicking
// run is a retryable failure that burns an attempt, never a downed worker.
func invoke(ctx context.Context, fn run.WorkflowFn, input []byte) (out []byte, err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("panic: %v", v)
		}
	}()
	return fn(ctx, input)
}

// defaultBackoff is exponential — 250ms·2^(attempt−1) capped at 30s — with
// ±20% jitter.
func defaultBackoff(attempt int) time.Duration {
	d := backoffBase
	for i := 1; i < attempt && d < backoffCap; i++ {
		d *= 2
	}
	if d > backoffCap {
		d = backoffCap
	}
	jitter := 0.8 + 0.4*rand.Float64() //nolint:gosec // backoff jitter, not a secret
	return time.Duration(float64(d) * jitter)
}
