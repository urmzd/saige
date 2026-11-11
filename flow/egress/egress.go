// Package egress is the delivery half of the mesh: it turns "the agent said
// something" into "the surface shows it".
//
// Every egress adapter is a bus subscriber. A Sink that does not name its own
// subscriptions receives the run events on its own scheme's channels
// (slack://, github://, jira://) and renders each one on the channel it was
// published on, which is the channel of the event that started the run. A
// Sink that implements connector.Subscriber, such as the outbound webhook,
// names its channels, its type prefixes and its target. Nothing is registered
// per run and nothing chooses a surface: delivery follows the channel.
//
// Types are type prefixes ("run"), so an opt-in family ("run.usage",
// "run.context") reaches an adapter only when it names it. A control channel
// is never delivered to a surface.
//
// # One deliverer per tenant
//
// Egress reads a tenant's events through the bus consumer group Group, so
// however many replicas run an Engine, one of them delivers a tenant's events
// at a time and the others wait to take over. The group's cursor is
// committed only once every sink an event went to has accepted it (or given
// up on it for good), so a replica that takes over starts after the last
// event fully delivered and redelivers at most what was in flight. Sinks
// are idempotent, as delivery is at-least-once.
//
// Egress owns no other durable state. The bus owns sequencing, retention and
// the group's cursor, the connector owns how a surface is written, and
// durable's ledger owns what happened. Events older than the maximum age are
// skipped, so a group seen for the first time, which starts at the oldest
// event retained, does not render a day of retained runs; and a sequence a
// run already reached is never delivered twice by one term of an Engine.
//
// # Lanes, and why they exist
//
// Events for one run going to one target are carried by a single goroutine
// reading a FIFO queue, a lane, so they arrive in sequence order. That is not
// fussiness. A Slack sink renders a run by editing one message, and two edits
// that overtake each other leave the wrong final text on the surface
// permanently. Across lanes there is no order to keep and none is imposed, so a
// stalled Jira sink cannot delay a webhook and one run's slow surface cannot
// delay another run's. A lane ends when its run ends, or when it has been idle
// for a while; the next event for the run starts a new one.
//
// # Transient and permanent
//
// A sink that returns a transient error is retried with bounded exponential
// backoff. What is never retried is a failure no amount of waiting can fix:
// connector.ErrUndeliverable, connector.ErrUnsupported, connector.ErrNotFound,
// and a panic, which is a defect in the sink rather than congestion in front
// of it. A permanent failure retires that one delivery; the lane moves on.
//
// # Failure is loud
//
// Every delivery this package gives up on is logged at error level with the
// run, the target, the sequence, the type and the attempt count, because a
// silently dropped delivery is the worst failure this package could have: the
// agent did the work, the ledger says so, and the human waiting on the surface
// sees nothing.
package egress

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/bus"
	"github.com/urmzd/saige/flow/connector"
	"github.com/urmzd/saige/flow/event"
)

// Errors reported by egress. Callers match with errors.Is.
var (
	// ErrInvalid reports a configuration egress cannot serve.
	ErrInvalid = errors.New("egress: invalid")
	// ErrPanic reports a sink that panicked while delivering.
	ErrPanic = errors.New("egress: sink panicked")
	// ErrRunning reports a second Run for a tenant that is already running.
	ErrRunning = errors.New("egress: already running")
)

// Defaults.
const (
	// DefaultBacklog is the queue depth of one lane.
	DefaultBacklog = 256
	// DefaultConcurrency bounds sink calls in flight at once.
	DefaultConcurrency = 64
	// DefaultAttempts is how many times one event is offered to a sink.
	DefaultAttempts = 4
	// DefaultBackoff is the wait before the second attempt; it doubles.
	DefaultBackoff = 100 * time.Millisecond
	// DefaultMaxBackoff caps the wait between attempts.
	DefaultMaxBackoff = 5 * time.Second
	// DefaultTimeout bounds one sink call.
	DefaultTimeout = 30 * time.Second
	// DefaultMaxAge is how old an event may be and still be delivered.
	DefaultMaxAge = 15 * time.Minute
	// DefaultIdle is how long a lane waits for its run's next event.
	DefaultIdle = time.Minute
	// DefaultMaxRuns bounds how many runs' sequence cursors are kept.
	DefaultMaxRuns = 4096
)

// RunTypes is what a sink with no subscriptions of its own receives: every
// run type that is not opt-in.
var RunTypes = []string{"run"}

// Group is the bus consumer group egress reads each tenant's events through.
const Group = "egress"

// Resolver is where egress finds its adapters. *connector.Registry is one.
type Resolver interface {
	Sinks() []connector.Sink
}

var _ Resolver = (*connector.Registry)(nil)

// Option configures an Engine.
type Option func(*Engine)

// WithBacklog sets the queue depth of one lane.
func WithBacklog(n int) Option {
	return func(e *Engine) {
		if n > 0 {
			e.backlog = n
		}
	}
}

// WithConcurrency bounds sink calls in flight at once.
func WithConcurrency(n int) Option {
	return func(e *Engine) {
		if n > 0 {
			e.concurrency = n
		}
	}
}

// WithAttempts sets how many times one event is offered to a sink.
func WithAttempts(n int) Option {
	return func(e *Engine) {
		if n > 0 {
			e.attempts = n
		}
	}
}

// WithBackoff sets the retry backoff, doubling from base to max.
func WithBackoff(base, max time.Duration) Option {
	return func(e *Engine) {
		if base > 0 {
			e.backoff = base
		}
		if max > 0 {
			e.maxBackoff = max
		}
	}
}

// WithTimeout bounds one sink call. Zero or less means no bound.
func WithTimeout(d time.Duration) Option {
	return func(e *Engine) { e.timeout = d }
}

// WithMaxAge sets how old an event may be and still be delivered.
func WithMaxAge(d time.Duration) Option {
	return func(e *Engine) {
		if d > 0 {
			e.maxAge = d
		}
	}
}

// WithIdle sets how long a lane waits for its run's next event before it
// ends.
func WithIdle(d time.Duration) Option {
	return func(e *Engine) {
		if d > 0 {
			e.idle = d
		}
	}
}

// WithClock sets the clock the maximum age is measured with.
func WithClock(now func() time.Time) Option {
	return func(e *Engine) {
		if now != nil {
			e.now = now
		}
	}
}

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option {
	return func(e *Engine) {
		if l != nil {
			e.log = l
		}
	}
}

// Engine delivers run events to egress adapters.
type Engine struct {
	bus   bus.Bus
	sinks Resolver
	log   *slog.Logger
	now   func() time.Time

	backlog     int
	concurrency int
	attempts    int
	backoff     time.Duration
	maxBackoff  time.Duration
	timeout     time.Duration
	maxAge      time.Duration
	idle        time.Duration

	sem chan struct{}

	mu      sync.Mutex
	running map[string]bool
	lanes   map[laneKey]*lane
}

// New returns an Engine over b that delivers to the sinks r resolves.
func New(b bus.Bus, r Resolver, opts ...Option) *Engine {
	if b == nil || r == nil {
		panic("egress: New requires a bus and a resolver")
	}
	e := &Engine{
		bus:         b,
		sinks:       r,
		log:         slog.New(slog.DiscardHandler),
		now:         time.Now,
		backlog:     DefaultBacklog,
		concurrency: DefaultConcurrency,
		attempts:    DefaultAttempts,
		backoff:     DefaultBackoff,
		maxBackoff:  DefaultMaxBackoff,
		timeout:     DefaultTimeout,
		maxAge:      DefaultMaxAge,
		idle:        DefaultIdle,
		running:     map[string]bool{},
		lanes:       map[laneKey]*lane{},
	}
	for _, o := range opts {
		o(e)
	}
	e.sem = make(chan struct{}, e.concurrency)
	return e
}

// Binding is one adapter's subscription, as egress serves it.
type Binding struct {
	Sink connector.Sink
	connector.Subscription
}

// Bindings returns what each sink receives: its own subscriptions when it is
// a connector.Subscriber, and otherwise RunTypes on its own scheme's
// channels, rendered on each event's channel.
func Bindings(r Resolver) []Binding {
	var out []Binding
	for _, s := range r.Sinks() {
		if sub, ok := s.(connector.Subscriber); ok {
			for _, x := range sub.Subscriptions() {
				out = append(out, Binding{Sink: s, Subscription: x})
			}
			continue
		}
		out = append(out, Binding{Sink: s, Subscription: connector.Subscription{
			Channels: []string{s.Name() + "://"},
			Types:    slices.Clone(RunTypes),
		}})
	}
	return out
}

// Run delivers tenant's run events until ctx is done, and then waits for its
// lanes to stop. It reads through the consumer group Group, so while another
// Engine (in this process or another one on the same bus) holds the tenant's
// lease, Run waits to take over. A cancelled ctx is not a failure. Another
// Run for the same tenant on this Engine at once is ErrRunning.
func (e *Engine) Run(ctx context.Context, tenant string) error {
	if tenant == "" {
		return fmt.Errorf("%w: no tenant", ErrInvalid)
	}
	e.mu.Lock()
	if e.running[tenant] {
		e.mu.Unlock()
		return ErrRunning
	}
	e.running[tenant] = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.running, tenant)
		e.mu.Unlock()
	}()

	// Fail closed: a subscription egress cannot serve is a configuration
	// error, not a sink that silently renders nothing.
	bindings := Bindings(e.sinks)
	for _, b := range bindings {
		if err := validBinding(b); err != nil {
			return fmt.Errorf("egress: sink %s: %w", b.Sink.Name(), err)
		}
	}
	sub := union(tenant, bindings)
	for {
		if ctx.Err() != nil {
			return nil
		}
		stream, err := e.bus.Consume(ctx, Group, sub)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("egress: consume %s: %w", tenant, err)
		}
		err = e.term(ctx, tenant, bindings, stream)
		_ = stream.Close()
		switch {
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, bus.ErrClosed):
			e.log.Warn("egress: bus stream ended, delivery has stopped", "tenant", tenant, "error", err)
			return nil
		case errors.Is(err, bus.ErrSlowConsumer):
			e.log.Warn("egress: fell behind the bus, resuming from the committed cursor", "tenant", tenant, "error", err)
		case errors.Is(err, bus.ErrLeaseLost):
			e.log.Info("egress: another replica took over delivery, waiting to take it back", "tenant", tenant)
		default:
			return fmt.Errorf("egress: recv for %s: %w", tenant, err)
		}
	}
}

// union is the one subscription that carries every binding's events: a
// binding's own subscription is re-checked per event (selected).
func union(tenant string, bindings []Binding) bus.Subscription {
	sub := bus.Subscription{Tenant: tenant}
	for _, b := range bindings {
		for _, c := range b.Channels {
			if !slices.Contains(sub.Channels, c) {
				sub.Channels = append(sub.Channels, c)
			}
		}
		for _, t := range b.Types {
			if !slices.Contains(sub.Types, t) {
				sub.Types = append(sub.Types, t)
			}
		}
	}
	return sub
}

// term is one hold of a tenant's lease: from taking it to losing it, or to
// the end of Run. Its lanes, its sequence cursors and its commits end with
// it, so a later term starts from the bus's cursor and nothing else.
type term struct {
	stream  bus.GroupStream
	cursors *cursors
	acks    *acks
}

// term delivers until stream ends, then stops its lanes and commits what
// they finished.
func (e *Engine) term(ctx context.Context, tenant string, bindings []Binding, stream bus.GroupStream) error {
	ctx, cancel := context.WithCancel(ctx)
	t := &term{stream: stream, cursors: newCursors(DefaultMaxRuns), acks: newAcks()}
	var lanes sync.WaitGroup
	committing := make(chan struct{})
	go func() {
		defer close(committing)
		e.commits(ctx, tenant, t)
	}()
	defer func() {
		cancel()
		lanes.Wait()
		<-committing
		// What the lanes finished on the way out is committed too, so a
		// replica that takes over does not deliver it again.
		final, stop := context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
		defer stop()
		e.commit(final, tenant, t)
	}()
	for {
		d, err := stream.Recv(ctx)
		if err != nil {
			return err
		}
		t.acks.open(d.Pos)
		if d.Event.Tenant == tenant {
			for i, b := range bindings {
				e.dispatch(ctx, &lanes, t, i, b, d)
			}
		}
		t.acks.finish(d.Pos)
	}
}

// commitTimeout bounds the commit a term makes on its way out.
const commitTimeout = 5 * time.Second

// commits commits the term's cursor whenever its lanes finish the oldest
// events in flight, coalescing a burst into one write.
func (e *Engine) commits(ctx context.Context, tenant string, t *term) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.acks.advanced:
			e.commit(ctx, tenant, t)
		}
	}
}

// commit commits every event the term has finished.
func (e *Engine) commit(ctx context.Context, tenant string, t *term) {
	pos, ok := t.acks.uncommitted()
	if !ok {
		return
	}
	err := t.stream.Commit(ctx, pos)
	switch {
	case err == nil:
		t.acks.committed(pos)
	case errors.Is(err, bus.ErrLeaseLost):
		e.log.Info("egress: delivered after another replica took over; it may deliver again", "tenant", tenant, "position", pos)
	case ctx.Err() == nil:
		e.log.Warn("egress: cursor not committed; a replica that takes over delivers again", "tenant", tenant, "position", pos, "error", err)
	}
}

func validBinding(b Binding) error {
	switch {
	case len(b.Channels) == 0:
		return fmt.Errorf("%w: no channel", ErrInvalid)
	case len(b.Types) == 0:
		return fmt.Errorf("%w: no type", ErrInvalid)
	case !b.Target.IsZero() && b.Target.Connector != b.Sink.Name():
		return fmt.Errorf("%w: target %s is not %s's", ErrInvalid, b.Target, b.Sink.Name())
	}
	return nil
}

// dispatch offers one event to its binding's lane, and holds the event's
// position open until the lane has delivered it.
func (e *Engine) dispatch(ctx context.Context, lanes *sync.WaitGroup, t *term, id int, b Binding, d bus.Delivery) {
	ev := d.Event
	if event.IsControl(ev.Channel) || !selected(b, ev) {
		return
	}
	if age := e.now().Sub(ev.Time); age > e.maxAge {
		e.log.Debug("egress: event too old to deliver", "sink", b.Sink.Name(), "run", ev.Run, "seq", ev.Seq, "age", age)
		return
	}
	target := b.Target
	if target.IsZero() {
		addr, err := address.Parse(ev.Channel)
		if err != nil || addr.Connector != b.Sink.Name() {
			// Only a sink's own scheme is rendered on the event's channel.
			return
		}
		target = addr
	}
	order := ev.Run
	if order == "" {
		order = "channel:" + ev.Channel
	}
	key := laneKey{binding: id, tenant: ev.Tenant, order: order, target: target.String()}

	e.mu.Lock()
	defer e.mu.Unlock()
	if ev.Run != "" && ev.Seq != 0 && !t.cursors.advance(key.cursor(), ev.Seq) {
		return // a redelivery or a replay: already dispatched
	}
	ln := e.lanes[key]
	if ln == nil {
		ln = &lane{
			e:      e,
			key:    key,
			sink:   b.Sink,
			target: target,
			acks:   t.acks,
			queue:  make(chan item, e.backlog),
		}
		e.lanes[key] = ln
		lanes.Add(1)
		go func() {
			defer lanes.Done()
			ln.serve(ctx)
		}()
	}
	t.acks.hold(d.Pos)
	ln.offer(item{ev: ev, pos: d.Pos})
}

// selected re-checks the subscription, so a backend that delivers more than
// was asked for never reaches a sink.
func selected(b Binding, ev event.Envelope) bool {
	return slices.ContainsFunc(b.Channels, func(p string) bool { return event.ChannelHasPrefix(ev.Channel, p) }) &&
		slices.ContainsFunc(b.Types, func(p string) bool { return event.Selects(p, ev.Type) })
}

// laneKey identifies one lane: a binding, a run (or a channel for events
// outside a run) and a target.
type laneKey struct {
	binding int
	tenant  string
	order   string
	target  string
}

func (k laneKey) cursor() string {
	return fmt.Sprintf("%d\x00%s\x00%s\x00%s", k.binding, k.tenant, k.order, k.target)
}

// terminal reports a type after which a run publishes nothing more.
func terminal(t string) bool {
	switch t {
	case event.TypeCompleted, event.TypeFailed, event.TypeStopped:
		return true
	}
	return false
}

func (e *Engine) acquire(ctx context.Context) error {
	select {
	case e.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Engine) release() { <-e.sem }

func (e *Engine) backoffFor(attempt int) time.Duration {
	wait := e.backoff
	for i := 1; i < attempt; i++ {
		if wait >= e.maxBackoff/2 {
			return e.maxBackoff
		}
		wait *= 2
	}
	return wait
}

// acks tracks which of a term's events are still being delivered, so the
// cursor is committed only past events every lane has finished. Positions
// are opened in the order the group delivers them, which is increasing.
type acks struct {
	mu       sync.Mutex
	order    []uint64
	pending  map[uint64]int
	done     uint64 // every event at or before it is finished
	commit   uint64 // the last position committed
	advanced chan struct{}
}

func newAcks() *acks {
	return &acks{pending: map[uint64]int{}, advanced: make(chan struct{}, 1)}
}

// open starts tracking pos, held once by the reader until it has offered
// the event to every lane.
func (a *acks) open(pos uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.order = append(a.order, pos)
	a.pending[pos] = 1
}

// hold adds one lane delivering pos.
func (a *acks) hold(pos uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending[pos]++
}

// finish releases one hold on pos and moves done past every finished event
// at the front.
func (a *acks) finish(pos uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending[pos]--
	moved := false
	for len(a.order) > 0 && a.pending[a.order[0]] <= 0 {
		delete(a.pending, a.order[0])
		a.done = a.order[0]
		a.order = a.order[1:]
		moved = true
	}
	if moved {
		select {
		case a.advanced <- struct{}{}:
		default:
		}
	}
}

// uncommitted returns the position to commit, if it moved since the last
// commit.
func (a *acks) uncommitted() (uint64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.done, a.done > a.commit
}

// committed records a commit.
func (a *acks) committed(pos uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.commit = max(a.commit, pos)
}

// cursors is the highest sequence dispatched per lane cursor, bounded by
// evicting the least recently advanced.
type cursors struct {
	max   int
	order *list.List
	byKey map[string]*list.Element
}

type cursor struct {
	key string
	seq uint64
}

func newCursors(max int) *cursors {
	return &cursors{max: max, order: list.New(), byKey: map[string]*list.Element{}}
}

// advance records seq for key and reports whether it is new.
func (c *cursors) advance(key string, seq uint64) bool {
	if el, ok := c.byKey[key]; ok {
		cur := el.Value.(*cursor)
		if seq <= cur.seq {
			return false
		}
		cur.seq = seq
		c.order.MoveToFront(el)
		return true
	}
	c.byKey[key] = c.order.PushFront(&cursor{key: key, seq: seq})
	for c.order.Len() > c.max {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.byKey, oldest.Value.(*cursor).key)
	}
	return true
}
