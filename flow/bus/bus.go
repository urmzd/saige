// Package bus is the broker: every event, from every surface and every run,
// is published once on a channel, and every subscriber that asked for that
// channel and type receives it. It is the single coupling point between
// producers (ingress adapters, the runtime) and consumers (egress adapters,
// an operator tailing one run, a dashboard).
//
// The bus carries event.Envelope and nothing else. What it deliberately does
// not do is as much of the design as what it does:
//
//   - It does not deliver. Turning an event into a comment, a Jira transition
//     or a webhook POST is an egress adapter's concern.
//   - It does not authorize. A Subscription names its tenant, which the
//     caller takes from its principal and never from a request.
//   - It is not the record. durable's ledger is truth. The bus retains a
//     bounded, recent window so a consumer can reconnect without missing the
//     last few seconds.
//
// # Subscriptions are default deny
//
// A Subscription names a tenant, channel prefixes and type prefixes. An empty
// tenant, an empty channel list or an empty type list matches nothing: there
// is no "everything" subscription. Prefixes are segment aware (see
// event.ChannelHasPrefix and event.HasPrefix), opt-in types are selected only
// by a prefix that names their family (event.Selects), and a control channel
// (control://...) is matched only by a subscription that names it exactly.
//
// # Run events and the rest
//
// A run event (type family "run") carries a run id and a sequence (Seq > 0)
// that orders it within the run. Re-publishing a sequence the run has already
// reached is an idempotent no-op: durable replays a workflow from the top, and
// within a run sequences only move forward. Every other event, a control
// request or an ingress event, is deduplicated by (tenant, id).
//
// Subscription.From is a resume cursor over run sequences: catch-up skips a
// run's events at or below it. Subscribe delivers what is retained first, in
// order, and then continues live with no gap and no duplicate at the seam.
//
// # Hops
//
// Every event carries Hops: how many events led to it. One over the limit
// (WithMaxHops, DefaultMaxHops) is refused, so two agents cannot answer each
// other forever.
//
// # Delivery
//
// At-least-once. A consumer may see the same event more than once, so
// consumers must be idempotent: remember the last sequence processed per run,
// and the ids of other events.
//
// # Slow consumers
//
// A slow consumer never slows a publisher and never slows another consumer.
// Memory terminates a subscriber whose buffer fills (its Recv drains what it
// holds and then fails with ErrSlowConsumer); Postgres holds the events
// already and simply stops reading. Recovery is always to resubscribe
// from the last sequence processed.
//
// # Retention
//
// Each run keeps a ring of its most recent events, and so does each channel
// for the events that are not run events (WithRetention). Postgres forgets
// everything WithIndexTTL after it was published.
//
// # Consumer groups
//
// A plain subscription is read by every process that opens it: an event
// stream to a browser, a control reader. A consumer that acts on an event
// once for the whole deployment (delivery to a surface, folding run events
// into the platform, waking agents) reads through a consumer group instead
// (Consume). A group keeps one cursor per tenant and one lease per tenant:
// exactly one member at a time holds a tenant's lease and reads from its
// committed cursor, and the others wait. A member renews its lease while it
// lives; when it stops (Close releases at once, a crash lets the lease
// expire) another member takes over and reads from the last cursor
// committed, so the events after it may be delivered again. Delivery stays
// at-least-once, and group consumers stay idempotent by (run, seq) and by
// event id.
//
// Each tenant's events have a position (Delivery.Pos) that only grows, and
// Commit takes one. A group seen for the first time starts at the oldest
// event retained, exactly as a plain subscription does.
//
// # The outbox
//
// A producer whose state lives on the bus's database publishes the event
// that announces a change in the same transaction as the change
// (TxPublisher), so the two commit together. fiber_events is the outbox:
// Postgres consumers read it directly, with no relay.
//
// # Backends
//
// Memory serves one process. Postgres (fiber_events and its index tables,
// with LISTEN/NOTIFY) is shared by every process on it. bustest holds both
// to one contract, consumer groups included.
package bus

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urmzd/saige/flow/event"
)

// Errors returned by every backend. Callers match with errors.Is.
var (
	// ErrInvalid reports an event or subscription the bus cannot carry.
	ErrInvalid = errors.New("bus: invalid")
	// ErrClosed reports a Recv on a stream that was closed, either by Close
	// or by cancelling the context it was subscribed with.
	ErrClosed = errors.New("bus: stream closed")
	// ErrSlowConsumer reports a subscriber terminated for not keeping up.
	// Recovery is to resubscribe from the last sequence processed.
	ErrSlowConsumer = errors.New("bus: slow consumer")
	// ErrLeaseLost reports a consumer group member whose lease on a tenant
	// another member took over. Its stream has ended; recovery is to
	// Consume again, which waits for the lease.
	ErrLeaseLost = errors.New("bus: consumer group lease lost")
)

// Bus is the broker. Implementations must pass the conformance suite in
// pkg/bus/bustest.
type Bus interface {
	// Publish records e and fans it out to every matching subscriber. A run
	// event whose sequence the run has already reached, and any other event
	// whose (tenant, id) the bus has already seen, is an idempotent no-op.
	// It never blocks on a consumer.
	Publish(ctx context.Context, e event.Envelope) error

	// Subscribe returns a stream of the events sub accepts: everything
	// retained first, then the live tail, with no gap and no duplicate at the
	// seam. Cancelling ctx closes the stream, exactly as Close does.
	Subscribe(ctx context.Context, sub Subscription) (Stream, error)

	// Consume joins the consumer group named group for sub's tenant and
	// returns its stream. The stream delivers the events sub accepts after
	// the group's committed cursor, only while this member holds the
	// group's lease on the tenant; until then Recv waits. Cancelling ctx
	// ends the stream without releasing the lease, as a crash would, so it
	// expires (WithLease); Close releases it at once.
	Consume(ctx context.Context, group string, sub Subscription) (GroupStream, error)
}

// TxPublisher is a bus that can publish inside a caller's database
// transaction: the outbox. The event row is written in tx, so a state change
// and the event that announces it commit together or not at all, and a crash
// can never leave one without the other. Postgres implements it; Memory has
// no transaction to join and does not.
//
// Everything Publish promises holds: an event the bus has already seen is an
// idempotent no-op, and the event reaches subscribers and consumer groups
// once tx commits.
type TxPublisher interface {
	PublishTx(ctx context.Context, tx pgx.Tx, e event.Envelope) error
}

// Delivery is one event read through a consumer group, with its position in
// its tenant's order.
type Delivery struct {
	Event event.Envelope
	// Pos is the event's position among its tenant's events. Positions
	// only grow, and a group stream delivers them in increasing order.
	Pos uint64
}

// GroupStream is one member's view of a consumer group on one tenant. Like
// Stream it serves a single goroutine, except that Commit and Close are safe
// from anywhere.
type GroupStream interface {
	// Recv waits until this member holds the lease, then returns the next
	// event after the committed cursor. It returns ErrLeaseLost once
	// another member took the lease over, ErrClosed after Close or after
	// the context Consume was given ended, and ErrSlowConsumer when this
	// member fell too far behind. Each of them ends the stream.
	Recv(ctx context.Context) (Delivery, error)

	// Commit records that every event up to and including pos was handled,
	// so a member that takes over starts after it. The cursor never moves
	// back. It returns ErrLeaseLost when this member no longer holds the
	// lease.
	Commit(ctx context.Context, pos uint64) error

	// Close ends the stream and releases the lease, so another member takes
	// over at once. It is idempotent.
	Close() error
}

// Stream is one consumer's view of the bus. It serves a single goroutine:
// Recv is not safe to call concurrently with itself, while Close is safe from
// anywhere and idempotent.
type Stream interface {
	// Recv returns the next event, blocking until one arrives, ctx is done,
	// or the stream ends. It returns ErrClosed after Close or after the
	// subscription's context was cancelled, and ErrSlowConsumer when this
	// consumer fell too far behind.
	Recv(ctx context.Context) (event.Envelope, error)

	// Close ends the stream and releases its resources. It is idempotent and
	// safe to call while Recv is blocked.
	Close() error
}

// Subscription is what one consumer listens to. A zero Subscription matches
// nothing: the tenant, at least one channel prefix and at least one type
// prefix are all required for anything to match.
type Subscription struct {
	// Tenant is the only tenant whose events match. The caller takes it from
	// its principal, never from a request.
	Tenant string `json:"tenant"`
	// Channels are channel prefixes (event.ChannelHasPrefix). A control
	// channel matches only a prefix equal to it.
	Channels []string `json:"channels,omitempty"`
	// Types are type prefixes (event.Selects). An opt-in type matches only a
	// prefix that names its family.
	Types []string `json:"types,omitempty"`
	// Run narrows the subscription to one run's events.
	Run string `json:"run,omitempty"`
	// From resumes after this sequence within each run: run events at or
	// below it are skipped. It does not apply to other events.
	From uint64 `json:"from,omitempty"`
}

// Accepts reports whether this subscription should receive e.
func (s Subscription) Accepts(e event.Envelope) bool {
	if s.Tenant == "" || e.Tenant != s.Tenant {
		return false
	}
	if s.Run != "" && e.Run != s.Run {
		return false
	}
	if isRunEvent(e.Type) && e.Seq <= s.From {
		return false
	}
	if !slices.ContainsFunc(s.Channels, func(p string) bool { return event.ChannelHasPrefix(e.Channel, p) }) {
		return false
	}
	return slices.ContainsFunc(s.Types, func(p string) bool { return event.Selects(p, e.Type) })
}

// matchesNothing reports a subscription that can never accept an event.
func (s Subscription) matchesNothing() bool {
	return s.Tenant == "" || len(s.Channels) == 0 || len(s.Types) == 0
}

// isRunEvent reports whether events of type t are sequenced within a run.
func isRunEvent(t string) bool { return event.Family(t) == "run" && t != event.TypeFeedback }

// Defaults for the options below.
const (
	// DefaultRetention is the ring depth per run, and per channel for the
	// events that are not run events.
	DefaultRetention = 1024
	// DefaultBacklog is the per-subscriber buffer depth.
	DefaultBacklog = 256
	// DefaultMaxRuns bounds how many rings Memory retains at once.
	DefaultMaxRuns = 4096
	// DefaultPollInterval is the Postgres reader's fallback poll while no
	// LISTEN connection is up.
	DefaultPollInterval = 250 * time.Millisecond
	// DefaultIndexTTL is how long an event stays retained on Postgres after
	// it was published.
	DefaultIndexTTL = 24 * time.Hour
	// DefaultMaxHops is the most hops an event may carry.
	DefaultMaxHops = 8
	// DefaultLease is how long a consumer group member holds a tenant's
	// lease without renewing it. A member renews three times per lease, so
	// it bounds how long a tenant waits after its member crashed.
	DefaultLease = 15 * time.Second
)

// options configures a backend.
type options struct {
	retention int
	backlog   int
	maxRuns   int
	maxHops   int
	poll      time.Duration
	indexTTL  time.Duration
	lease     time.Duration
	now       func() time.Time
}

func defaults() options {
	return options{
		retention: DefaultRetention,
		backlog:   DefaultBacklog,
		maxRuns:   DefaultMaxRuns,
		maxHops:   DefaultMaxHops,
		poll:      DefaultPollInterval,
		indexTTL:  DefaultIndexTTL,
		lease:     DefaultLease,
		now:       time.Now,
	}
}

func newOptions(opts []Option) options {
	o := defaults()
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// Option configures a bus backend. Non-positive and empty values are ignored
// so that a caller can pass a zero-valued config through without erasing the
// defaults.
type Option func(*options)

// WithRetention sets how many events each run, and each channel's non-run
// events, retain (default 1024).
func WithRetention(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.retention = n
		}
	}
}

// WithBacklog sets the per-subscriber buffer depth (default 256).
func WithBacklog(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.backlog = n
		}
	}
}

// WithMaxRuns bounds how many rings Memory retains, evicting the one whose
// last event is oldest (default 4096). Postgres ignores it.
func WithMaxRuns(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.maxRuns = n
		}
	}
}

// WithMaxHops sets the most hops an event may carry (default 8).
func WithMaxHops(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.maxHops = n
		}
	}
}

// WithPollInterval sets the Postgres reader's fallback poll while no LISTEN
// connection is up (default 250ms). Memory ignores it.
func WithPollInterval(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.poll = d
		}
	}
}

// WithIndexTTL sets how long Postgres keeps an event and what they
// know about its run or channel after it was published (default 24h).
// Memory ignores it.
func WithIndexTTL(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.indexTTL = d
		}
	}
}

// WithLease sets how long a consumer group member holds a tenant's lease
// without renewing it (default 15s). A shorter lease hands a crashed
// member's tenants over sooner and costs more renewals.
func WithLease(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.lease = d
		}
	}
}

// normalize validates e and stamps its time. Every backend runs it before
// touching storage, which keeps ErrInvalid identical across them.
func normalize(e *event.Envelope, o options) error {
	switch {
	case e.Tenant == "":
		return fmt.Errorf("%w: event %q has no tenant", ErrInvalid, e.ID)
	case e.ID == "":
		return fmt.Errorf("%w: event has no id", ErrInvalid)
	case e.Type == "":
		return fmt.Errorf("%w: event %q has no type", ErrInvalid, e.ID)
	case e.Channel == "":
		return fmt.Errorf("%w: event %q has no channel", ErrInvalid, e.ID)
	case isRunEvent(e.Type) && e.Run == "":
		return fmt.Errorf("%w: run event %q names no run", ErrInvalid, e.ID)
	case isRunEvent(e.Type) && e.Seq == 0:
		return fmt.Errorf("%w: run event %q has no sequence", ErrInvalid, e.ID)
	case !isRunEvent(e.Type) && e.Seq != 0:
		return fmt.Errorf("%w: %s event %q is not sequenced", ErrInvalid, e.Type, e.ID)
	case e.Hops > o.maxHops:
		return fmt.Errorf("%w: event %q has %d hops, over the limit of %d", ErrInvalid, e.ID, e.Hops, o.maxHops)
	}
	if e.Time.IsZero() {
		e.Time = o.now().UTC()
	}
	if err := e.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return nil
}

// validateGroup checks a consumer group can be joined: it has a name and a
// tenant to hold a lease on.
func validateGroup(group string, sub Subscription) error {
	switch {
	case group == "":
		return fmt.Errorf("%w: consumer group has no name", ErrInvalid)
	case sub.Tenant == "":
		return fmt.Errorf("%w: consumer group %q names no tenant", ErrInvalid, group)
	}
	return validate(sub)
}

// validate checks a subscription is well formed. An empty one is well formed
// and matches nothing.
func validate(sub Subscription) error {
	for _, p := range sub.Types {
		if p == "" {
			return fmt.Errorf("%w: empty type prefix", ErrInvalid)
		}
	}
	for _, p := range sub.Channels {
		if p == "" {
			return fmt.Errorf("%w: empty channel prefix", ErrInvalid)
		}
	}
	return nil
}
