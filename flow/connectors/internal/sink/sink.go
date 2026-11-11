// Package sink is the delivery engine every human-facing connector shares: one
// message per run, edited as the run proceeds.
//
// # Why one message and not many
//
// This is the most consequential product decision in the connector layer. An
// agent emits hundreds of text deltas per answer. Posting one Slack message or
// one GitHub comment per delta would make any real channel unusable within a
// single run, and because delivery is at-least-once, a redelivery would post
// every line a second time. So a run owns exactly one message on each surface
// it delivers to, and every subsequent event edits that message.
//
// Editing turns idempotency from bookkeeping into arithmetic. The engine never
// applies a diff to a surface: it re-renders the whole accumulated document (see
// internal/render) and replaces the body. The same set of events therefore
// always produces the same bytes, in any arrival order, however many times each
// one arrives — and when the re-render produces bytes identical to what was last
// written, the engine skips the API call entirely, which is the common case for
// a redelivered event.
//
// # Coalescing, and why there is no goroutine
//
// Editing per token would replace a flood of messages with a flood of API calls
// straight into a rate limit. The engine coalesces: an edit goes out at most
// once per Interval, and a run whose events have not been flushed is left dirty
// until the next event or the next Flush.
//
// Three cases jump the interval, because each is a moment a human is waiting on:
// the first render of a run (so a mention is acknowledged immediately), a
// terminal status (so the final answer is never left truncated), and a park (so
// a run waiting on input says so).
//
// The engine runs no background goroutine. It has a Flush the caller schedules,
// and a clock seam instead of a timer. A connector that owns a ticker would own
// a lifecycle, a shutdown ordering and a leak; egress already has a loop,
// and tests get determinism for free.
//
// # Durable state
//
// WithState persists both the message handle and the complete document before
// delivery returns. A new replica continues the same message and redelivered
// deltas retain their sequence deduplication. External creation and a storage
// commit are not atomic: a crash between them can still duplicate a message.
// Without WithState, state is local to this engine and its bounded cache.
package sink

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors/internal/render"
	"github.com/urmzd/saige/flow/deliverystate"
	"github.com/urmzd/saige/flow/event"
)

// Defaults for the engine.
const (
	// DefaultInterval is the coalescing window. It is chosen against the
	// tightest of the surfaces: Slack documents roughly one request per second
	// per channel for chat.update, and a second of latency on a streaming answer
	// reads as live.
	DefaultInterval = 1 * time.Second

	// DefaultMaxRuns bounds how many (run, target) pairs the engine remembers.
	// Evicting a pair forgets its message handle, so a redelivery arriving after
	// eviction posts a second message; the bound is set high enough that this
	// requires a redelivery thousands of runs stale.
	DefaultMaxRuns = 4096
)

// Surface is the native side of run-scoped rendering: post the one message a
// run owns, then edit it.
//
// handle is whatever the surface needs to address that message again — a Slack
// message timestamp, a GitHub comment id, a Jira comment id — and is opaque to
// the engine.
type Surface interface {
	Create(ctx context.Context, target address.Address, body string) (handle string, err error)
	Update(ctx context.Context, target address.Address, handle, body string) error
}

// AfterDelivery optionally indexes a delivered message outside the delivery
// store transaction. It must be idempotent: failures remain pending across
// restarts, and a crash before acknowledgement can repeat the callback.
type AfterDelivery interface {
	AfterDelivery(context.Context, address.Address, string) error
}

// Renderer turns a document into the surface's markup. It must be pure: the
// engine calls it whenever it needs bytes and compares the result with what it
// last wrote, so a renderer that varies on anything but its input (a timestamp
// of its own, say) would defeat the "skip the call when nothing changed" path.
type Renderer func(render.View) string

// Engine keeps one message per (run, target) and edits it as events arrive. It
// is safe for concurrent use, and deliveries to different runs do not block each
// other while their surfaces are being written.
type Engine struct {
	observe func(context.Context, address.Address, string, render.View) error

	surface  Surface
	render   Renderer
	interval time.Duration
	maxRuns  int
	now      func() time.Time
	store    deliverystate.Store
	scope    string

	mu    sync.Mutex
	byKey map[string]*list.Element
	order *list.List // front is most recently used
}

// Option configures an Engine.
type Option func(*Engine)

// WithObserver records the native message handle after delivery. An error
// leaves the write pending, so the host never acknowledges an unindexed answer.
func WithObserver(observe func(context.Context, address.Address, string, render.View) error) Option {
	return func(e *Engine) { e.observe = observe }
}

// WithInterval sets the coalescing window. A non-positive interval flushes on
// every event, which is what a test wants and no production surface does.
func WithInterval(d time.Duration) Option {
	return func(e *Engine) { e.interval = d }
}

// WithMaxRuns bounds how many (run, target) pairs are remembered.
func WithMaxRuns(n int) Option {
	return func(e *Engine) {
		if n > 0 {
			e.maxRuns = n
		}
	}
}

// WithClock replaces the clock. It is the seam that lets a test exercise
// coalescing without sleeping.
func WithClock(now func() time.Time) Option {
	return func(e *Engine) {
		if now != nil {
			e.now = now
		}
	}
}

// WithState uses shared state scoped to one stable connector instance. The
// store serializes surface calls across replicas. Nil retains the local cache.
func WithState(store deliverystate.Store, scope string) Option {
	return func(e *Engine) { e.store, e.scope = store, scope }
}

// New returns an engine that draws with r onto s.
func New(s Surface, r Renderer, opts ...Option) *Engine {
	e := &Engine{
		surface:  s,
		render:   r,
		interval: DefaultInterval,
		maxRuns:  DefaultMaxRuns,
		now:      time.Now,
		byKey:    make(map[string]*list.Element),
		order:    list.New(),
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// state is one run's rendering on one target.
type state struct {
	key        string
	durableKey deliverystate.Key
	target     address.Address

	// mu serializes rendering and writing for this one message, so two
	// concurrent deliveries cannot both decide to create it. It is separate
	// from the engine lock so that a slow surface write blocks one run rather
	// than every run.
	mu     sync.Mutex
	doc    *render.Doc
	handle string
	// written is the body last accepted by the surface. Comparing against it is
	// what makes a redelivery free.
	written      string
	dirty        bool
	afterPending bool
	lastFlush    time.Time
}

// Deliver applies e and writes the run's message if it is due.
func (eng *Engine) Deliver(ctx context.Context, target address.Address, e event.Envelope) error {
	if eng.render == nil || eng.surface == nil {
		return fmt.Errorf("sink: engine is not configured with a surface and a renderer")
	}
	k := deliverystate.Key{Tenant: e.Tenant, Scope: eng.scope, Run: e.Run, Channel: e.Channel, Target: target.String()}
	st := eng.state(messageKey(e.Tenant, e.Run, e.Channel, target.String()), target, k)
	if eng.store != nil {
		// Commit the event before touching the external API. If that call
		// reaches its deadline, its transaction can roll back without losing
		// the dirty document that a fresh replica must recover.
		due := false
		if err := eng.persist(ctx, st, func(current *state) error {
			due = eng.apply(current, e)
			return nil
		}); err != nil {
			return err
		}
		if !due {
			return eng.finish(ctx, st)
		}
		if err := eng.persist(ctx, st, func(current *state) error {
			// Another replica may have flushed or advanced this document
			// between the two transactions. Always use the latest state.
			if current.dirty && eng.due(current, current.doc.View()) {
				return eng.write(ctx, current, current.doc.View())
			}
			return nil
		}); err != nil {
			return err
		}
		return eng.finish(ctx, st)
	}

	st.mu.Lock()
	err := eng.deliver(ctx, st, e)
	st.mu.Unlock()
	if err != nil {
		return err
	}
	return eng.finish(ctx, st)
}

func (eng *Engine) deliver(ctx context.Context, st *state, e event.Envelope) error {
	if !eng.apply(st, e) {
		return nil
	}
	return eng.write(ctx, st, st.doc.View())
}

// apply records the event and reports whether the document is due for writing.
func (eng *Engine) apply(st *state, e event.Envelope) bool {
	changed := st.doc.Apply(e)
	if !changed && !st.dirty {
		return false
	}
	st.dirty = true
	return eng.due(st, st.doc.View())
}

// Flush writes every run whose events have not reached its surface yet. A
// caller with its own ticker calls this to bound how long a dirty run can sit;
// without it, a run that stops emitting is written no later than its terminal
// event, which is the case that matters.
func (eng *Engine) Flush(ctx context.Context) error {
	if eng.store != nil {
		keys, err := eng.store.Pending(ctx, eng.scope, 256)
		if err != nil {
			return err
		}
		var errs []error
		for _, k := range keys {
			target, err := address.Parse(k.Target)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			st := eng.state(messageKey(k.Tenant, k.Run, k.Channel, k.Target), target, k)
			if err := eng.persist(ctx, st, func(current *state) error {
				if current.dirty {
					return eng.write(ctx, current, current.doc.View())
				}
				return nil
			}); err != nil {
				errs = append(errs, err)
				continue
			}
			if err := eng.finish(ctx, st); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
	eng.mu.Lock()
	states := make([]*state, 0, len(eng.byKey))
	for _, el := range eng.byKey {
		states = append(states, el.Value.(*state))
	}
	eng.mu.Unlock()

	var errs []error
	for _, st := range states {
		st.mu.Lock()
		if st.dirty {
			if err := eng.write(ctx, st, st.doc.View()); err != nil {
				errs = append(errs, err)
			}
		}
		st.mu.Unlock()
		if err := eng.finish(ctx, st); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// due decides whether this event's render goes out now or waits for the next
// one. See the package doc for the three cases that jump the interval.
func (eng *Engine) due(st *state, v render.View) bool {
	switch {
	case st.handle == "":
		return true
	case v.Terminal(), v.Status == render.StatusParked:
		return true
	case eng.interval <= 0:
		return true
	default:
		return !eng.now().Before(st.lastFlush.Add(eng.interval))
	}
}

// write renders the document and puts it on the surface.
//
// On failure the state stays dirty and written stays as it was, so the next
// delivery or Flush retries. That is the only error policy that is compatible
// with at-least-once: a dropped edit would leave the surface showing a prefix of
// the answer forever.
func (eng *Engine) write(ctx context.Context, st *state, v render.View) error {
	body := eng.render(v)
	if body == "" {
		st.dirty = false
		return nil
	}
	if st.handle != "" && body == st.written {
		st.dirty = false
		return nil
	}

	if st.handle == "" {
		handle, err := eng.surface.Create(ctx, st.target, body)
		if err != nil {
			return fmt.Errorf("sink: create message for run %s on %s: %w", v.RunID, st.target, err)
		}
		st.handle = handle
	} else if err := eng.surface.Update(ctx, st.target, st.handle, body); err != nil {
		return fmt.Errorf("sink: update message %s for run %s on %s: %w", st.handle, v.RunID, st.target, err)
	}

	if eng.observe != nil {
		if err := eng.observe(ctx, st.target, st.handle, v); err != nil {
			return err
		}
	}
	st.written = body
	st.dirty = false
	_, st.afterPending = eng.surface.(AfterDelivery)
	st.lastFlush = eng.now()
	return nil
}

func (eng *Engine) finish(ctx context.Context, st *state) error {
	after, ok := eng.surface.(AfterDelivery)
	if !ok {
		return nil
	}
	st.mu.Lock()
	pending, handle, written := st.afterPending, st.handle, st.written
	st.mu.Unlock()
	if !pending || handle == "" {
		return nil
	}
	if err := after.AfterDelivery(ctx, st.target, handle); err != nil {
		return err
	}
	ack := func(current *state) error {
		// A newer delivery may have completed while this callback ran. Its
		// distinct write still requires its own post-delivery work.
		if current.handle == handle && current.written == written {
			current.afterPending = false
		}
		return nil
	}
	if eng.store != nil {
		return eng.persist(ctx, st, ack)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return ack(st)
}

// state returns the run's rendering state, creating it if this is the first
// event, and evicting the least recently used one when the bound is reached.
func (eng *Engine) state(k string, target address.Address, durableKey deliverystate.Key) *state {
	eng.mu.Lock()
	defer eng.mu.Unlock()

	if el, ok := eng.byKey[k]; ok {
		eng.order.MoveToFront(el)
		return el.Value.(*state)
	}
	st := &state{key: k, target: target, durableKey: durableKey, doc: render.New()}
	eng.byKey[k] = eng.order.PushFront(st)
	for eng.order.Len() > eng.maxRuns {
		oldest := eng.order.Back()
		eng.order.Remove(oldest)
		delete(eng.byKey, oldest.Value.(*state).key)
	}
	return st
}

// Handle returns the native message identifier a run owns on channel, and
// whether the engine has one. It exists for tests and for operators asking
// "which message is this run writing to"; the delivery path does not use it.
func (eng *Engine) Handle(runID, channel string) (string, bool) {
	eng.mu.Lock()
	var st *state
	for _, el := range eng.byKey {
		candidate := el.Value.(*state)
		if candidate.durableKey.Run == runID && candidate.durableKey.Channel == channel {
			if st != nil {
				eng.mu.Unlock()
				return "", false
			}
			st = candidate
		}
	}
	eng.mu.Unlock()
	if st == nil {
		return "", false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.handle, st.handle != ""
}

// messageKey includes tenant and destination even for the local cache.
func messageKey(tenant, runID, channel, target string) string {
	data, _ := json.Marshal([]string{tenant, runID, channel, target})
	return string(data)
}

type savedState struct {
	Format          int
	Doc             *render.Doc
	Handle, Written string
	Dirty           bool
	AfterPending    bool
	LastFlush       time.Time
}

// persist always reloads under the store's cross-replica lock. The cache is
// only a local Handle lookup, never an authority after a handoff.
func (eng *Engine) persist(ctx context.Context, st *state, apply func(*state) error) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	var deliveryErr error
	err := eng.store.Update(ctx, st.durableKey, func(data []byte) ([]byte, bool, error) {
		saved := savedState{Format: 1, Doc: render.New()}
		if len(data) > 0 {
			if err := json.Unmarshal(data, &saved); err != nil {
				return nil, false, fmt.Errorf("sink: restore state: %w", err)
			}
			if saved.Format != 1 || saved.Doc == nil {
				return nil, false, fmt.Errorf("sink: unsupported state format %d", saved.Format)
			}
		}
		current := &state{key: st.key, target: st.target, durableKey: st.durableKey, doc: saved.Doc, handle: saved.Handle, written: saved.Written, dirty: saved.Dirty, afterPending: saved.AfterPending, lastFlush: saved.LastFlush}
		deliveryErr = apply(current)
		data, err := json.Marshal(savedState{Format: 1, Doc: current.doc, Handle: current.handle, Written: current.written, Dirty: current.dirty, AfterPending: current.afterPending, LastFlush: current.lastFlush})
		if err != nil {
			return nil, false, err
		}
		st.doc, st.handle, st.written, st.dirty, st.lastFlush = current.doc, current.handle, current.written, current.dirty, current.lastFlush
		st.afterPending = current.afterPending
		return data, current.dirty || current.afterPending, nil
	})
	return errors.Join(err, deliveryErr)
}
