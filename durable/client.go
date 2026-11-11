package durable

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/urmzd/saige/durable/ledger"
	"github.com/urmzd/saige/durable/queue"
	"github.com/urmzd/saige/durable/run"
)

// Client submits runs and observes them. It is the producer side of the two
// interfaces; a worker embedded in the same process consumes from the same
// pair.
type Client struct {
	lgr         ledger.Ledger
	q           queue.Queue
	codec       run.Codec
	maxAttempts int
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithCodec overrides the default JSON codec. Client and worker must agree;
// the recorded ContentType makes a mismatch fail loudly.
func WithCodec(c run.Codec) ClientOption { return func(cl *Client) { cl.codec = c } }

// WithDefaultMaxAttempts sets the retry budget applied to runs that don't
// set their own (default 5).
func WithDefaultMaxAttempts(n int) ClientOption { return func(cl *Client) { cl.maxAttempts = n } }

// New composes a durable client from any Ledger and Queue implementation.
// Nothing is created, nothing is migrated: the implementations own storage.
func New(lgr ledger.Ledger, q queue.Queue, opts ...ClientOption) *Client {
	c := &Client{lgr: lgr, q: q, codec: run.JSONCodec{}, maxAttempts: 5}
	for _, o := range opts {
		o(c)
	}
	return c
}

type startCfg struct {
	runID       string
	maxAttempts int
	parent      string
	accept      func(context.Context, run.Run) error
}

// StartOption configures one submission.
type StartOption func(*startCfg)

// WithRunID supplies the run ID — which doubles as the submit idempotency
// key. Starting an existing ID is a no-op returning a handle to the existing
// run; natural keys ("order-1234") make resubmission safe by construction.
func WithRunID(id string) StartOption { return func(c *startCfg) { c.runID = id } }

// WithMaxAttempts overrides the run's retry budget: the maximum number of
// FAILED executions (retryable errors, crashes discovered by lease expiry)
// before the run is terminally failed. Waiting never counts — park/resume
// cycles and janitor backstop polls are free.
func WithMaxAttempts(n int) StartOption { return func(c *startCfg) { c.maxAttempts = n } }

// WithParent records lineage (forks and child runs read it; informational
// in v1).
func WithParent(runID string) StartOption { return func(c *startCfg) { c.parent = runID } }

// WithAcceptor registers the run through accept instead of the client's
// ledger. accept must write the run to that same ledger, idempotently by its
// ID, as Ledger.Accept does: it is the seam an outbox uses to commit the run
// together with the event that announces it, in one database transaction.
// Enqueue still follows a nil return, and a run accepted but never enqueued
// is recovered by the janitor as always.
func WithAcceptor(accept func(ctx context.Context, r run.Run) error) StartOption {
	return func(c *startCfg) { c.accept = accept }
}

// Start durably submits a run of a: ledger Accept, then queue Enqueue.
// A nil error means accepted-and-queued — "acknowledged means durable".
// Start is a free function because Go methods cannot introduce type
// parameters.
func Start[I, O any](ctx context.Context, c *Client, a *ActivityFn[I, O], in I, opts ...StartOption) (*Handle[O], error) {
	cfg := startCfg{maxAttempts: c.maxAttempts}
	for _, o := range opts {
		o(&cfg)
	}
	input, err := c.codec.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("durable: marshal input for %q: %w", a.Name(), err)
	}
	id := cfg.runID
	if id == "" {
		id = run.NewID()
	}
	r := run.Run{
		ID:          id,
		Name:        a.Name(),
		Input:       input,
		Status:      run.StatusPending,
		MaxAttempts: cfg.maxAttempts,
		ParentRunID: cfg.parent,
		CreatedAt:   time.Now(),
	}
	accept := c.lgr.Accept
	if cfg.accept != nil {
		accept = cfg.accept
	}
	if err := accept(ctx, r); err != nil {
		return nil, fmt.Errorf("durable: accept %s: %w", id, err)
	}
	if err := c.q.Enqueue(ctx, id, 0); err != nil {
		// Degraded ack: the run is durable in the ledger and the janitor
		// will re-enqueue it even if the caller never retries — so the
		// handle is returned ALONGSIDE the error. Dropping it would strand
		// the caller without the run ID of a run that will execute.
		return &Handle[O]{c: c, id: id}, fmt.Errorf("durable: enqueue %s (run is accepted; janitor will recover): %w", id, err)
	}
	return &Handle[O]{c: c, id: id}, nil
}

// Exec is Start + Result: synchronous composition.
func Exec[I, O any](ctx context.Context, c *Client, a *ActivityFn[I, O], in I, opts ...StartOption) (O, error) {
	h, err := Start(ctx, c, a, in, opts...)
	if err != nil {
		var zero O
		return zero, err
	}
	return h.Result(ctx)
}

// HandleFor re-attaches to an existing run — after a process restart, or
// from a different process entirely.
func HandleFor[O any](c *Client, runID string) *Handle[O] {
	return &Handle[O]{c: c, id: runID}
}

// Handle observes one run.
type Handle[O any] struct {
	c  *Client
	id string
}

// RunID returns the run's ID.
func (h *Handle[O]) RunID() string { return h.id }

// Result blocks until the run is terminal and returns its output, polling
// the ledger with backoff. A failed run returns *RunFailedError. Backends
// with the ledger.RunGetter capability are polled with a point read; others
// fall back to Load (which also prefetches the record history every poll —
// implement GetRun on custom ledgers).
func (h *Handle[O]) Result(ctx context.Context) (O, error) {
	var zero O
	getRun := func(ctx context.Context) (run.Run, error) {
		r, _, err := h.c.lgr.Load(ctx, h.id)
		return r, err
	}
	if g, ok := h.c.lgr.(ledger.RunGetter); ok {
		getRun = func(ctx context.Context) (run.Run, error) { return g.GetRun(ctx, h.id) }
	}
	delay := 10 * time.Millisecond
	for {
		r, err := getRun(ctx)
		if err != nil {
			return zero, fmt.Errorf("durable: result %s: %w", h.id, err)
		}
		switch r.Status {
		case run.StatusSucceeded:
			var out O
			if err := h.c.codec.Unmarshal(r.Output, &out); err != nil {
				return zero, &DecodeError{Name: r.Name, Err: err}
			}
			return out, nil
		case run.StatusFailed:
			return zero, &RunFailedError{RunID: h.id, Message: r.Error}
		}
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(delay):
		}
		if delay < 500*time.Millisecond {
			delay *= 2
		}
	}
}

// Signal writes an event record and wakes the run: the sugar form of
// "insert the row, enqueue the run". It targets the first unclaimed
// occurrence of name, so repeated signals satisfy successive Event calls.
func (c *Client) Signal(ctx context.Context, runID, name string, payload any) error {
	b, err := c.codec.Marshal(payload)
	if err != nil {
		return fmt.Errorf("durable: marshal signal %q: %w", name, err)
	}
	_, records, err := c.lgr.Load(ctx, runID)
	if err != nil {
		return fmt.Errorf("durable: signal %q: %w", name, err)
	}
	taken := make(map[string]bool, len(records))
	for _, rec := range records {
		taken[rec.Key] = true
	}
	for k := 0; ; k++ {
		key := "event:" + name + "#" + strconv.Itoa(k)
		if taken[key] {
			continue
		}
		_, rerr := c.lgr.Record(ctx, run.Record{
			RunID: runID, Key: key, Kind: run.KindEvent, Name: name,
			Status: run.RecordOK, Output: b,
		})
		if errors.Is(rerr, run.ErrAlreadyRecorded) {
			continue // lost a race for this occurrence; take the next
		}
		if rerr != nil {
			return fmt.Errorf("durable: signal %q: %w", name, rerr)
		}
		break
	}
	if err := c.q.Enqueue(ctx, runID, 0); err != nil {
		return fmt.Errorf("durable: signal %q enqueue (event is recorded; janitor will recover): %w", name, err)
	}
	return nil
}

// Watch streams a run's deltas (catch-up then live tail). It requires the
// queue to have the DeltaLog capability.
func (c *Client) Watch(ctx context.Context, runID string, from queue.Cursor) (queue.Stream, error) {
	dl, ok := c.q.(queue.DeltaLog)
	if !ok {
		return nil, errors.New("durable: queue backend has no delta log capability")
	}
	return dl.Watch(ctx, runID, from)
}
