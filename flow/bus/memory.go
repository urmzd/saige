package bus

import (
	"cmp"
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/urmzd/saige/flow/event"
)

// Memory is a complete single-process bus: it is what the zero-infrastructure
// deployment runs on and what every test in this repo runs against, and it
// holds the same contract as the shared backends.
//
// One mutex guards everything. Publish takes it to append to a ring and hand
// the event to each subscriber's buffer with a non-blocking send. Subscribe
// takes it to snapshot what is retained and join the fan-out in the same
// step, which closes the catch-up/live seam.
//
// Events are stored as JSON and decoded for each delivery, so one consumer
// cannot see another's mutation, and Envelope.Private never crosses the bus.
type Memory struct {
	retention int
	backlog   int
	maxRuns   int
	opts      options

	mu    sync.Mutex
	rings map[string]*memRing
	// lru orders rings by last publish, most recent at the front.
	lru  *list.List
	subs map[*memStream]struct{}
	// tops is each tenant's last position.
	tops map[string]uint64
	// groups is each consumer group's cursor and lease, per tenant.
	groups map[groupKey]*memLease
}

// groupKey names one consumer group's state on one tenant.
type groupKey struct{ group, tenant string }

// memLease is one group's cursor and lease on one tenant.
type memLease struct {
	cursor uint64
	owner  string
	until  time.Time
}

var _ Bus = (*Memory)(nil)

// NewMemory returns an empty in-process bus. It honours WithRetention,
// WithBacklog, WithMaxRuns, WithMaxHops and WithLease.
func NewMemory(opts ...Option) *Memory {
	o := newOptions(opts)
	return &Memory{
		retention: o.retention,
		backlog:   o.backlog,
		maxRuns:   o.maxRuns,
		opts:      o,
		rings:     make(map[string]*memRing),
		lru:       list.New(),
		subs:      make(map[*memStream]struct{}),
		tops:      make(map[string]uint64),
		groups:    make(map[groupKey]*memLease),
	}
}

// stored is one retained event: its envelope, for matching, its JSON, for
// delivery, and its position among its tenant's events.
type stored struct {
	e   event.Envelope
	raw []byte
	pos uint64
}

// memRing is one run's ring, or one channel's ring of non-run events. The
// ring is fixed-length: push overwrites the oldest slot once it is full.
type memRing struct {
	key     string
	buf     []stored
	head    int
	n       int
	highest uint64              // a run's highest sequence, retained or evicted
	ids     map[string]struct{} // a channel ring's retained event ids
	elem    *list.Element
}

func (r *memRing) push(s stored) {
	slot := (r.head + r.n) % len(r.buf)
	if r.n == len(r.buf) {
		if r.ids != nil {
			delete(r.ids, r.buf[slot].e.ID)
		}
		r.buf[slot] = s
		r.head = (r.head + 1) % len(r.buf)
		return
	}
	r.buf[slot] = s
	r.n++
}

func (r *memRing) retained(dst []stored) []stored {
	for i := range r.n {
		dst = append(dst, r.buf[(r.head+i)%len(r.buf)])
	}
	return dst
}

// ringKey names the ring an event is retained in.
func ringKey(e event.Envelope) string {
	if isRunEvent(e.Type) {
		return "r\x00" + e.Tenant + "\x00" + e.Run
	}
	return "c\x00" + e.Tenant + "\x00" + e.Channel
}

// Publish implements Bus.
func (m *Memory) Publish(ctx context.Context, e event.Envelope) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := normalize(&e, m.opts); err != nil {
		return err
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("bus: encode %s %q: %w", e.Type, e.ID, err)
	}
	e.Data, e.Private = nil, nil // matching reads the header only

	m.mu.Lock()
	defer m.mu.Unlock()

	r := m.ringLocked(ringKey(e), !isRunEvent(e.Type))
	if isRunEvent(e.Type) {
		if e.Seq <= r.highest {
			return nil // already published: replay is not a duplicate
		}
		r.highest = e.Seq
	} else {
		if _, dup := r.ids[e.ID]; dup {
			return nil
		}
		r.ids[e.ID] = struct{}{}
	}
	m.tops[e.Tenant]++
	s := stored{e: e, raw: raw, pos: m.tops[e.Tenant]}
	r.push(s)
	for sub := range m.subs {
		sub.offer(s)
	}
	return nil
}

// ringLocked returns the ring for key, creating it and evicting the coldest
// ring if that takes the bus over its bound. Callers hold m.mu.
func (m *Memory) ringLocked(key string, ids bool) *memRing {
	if r := m.rings[key]; r != nil {
		m.lru.MoveToFront(r.elem)
		return r
	}
	r := &memRing{key: key, buf: make([]stored, m.retention)}
	if ids {
		r.ids = make(map[string]struct{})
	}
	m.rings[key] = r
	r.elem = m.lru.PushFront(r)
	for m.lru.Len() > m.maxRuns {
		cold := m.lru.Remove(m.lru.Back()).(*memRing)
		delete(m.rings, cold.key)
	}
	return r
}

// Subscribe implements Bus.
func (m *Memory) Subscribe(ctx context.Context, sub Subscription) (Stream, error) {
	if err := validate(sub); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return m.open(ctx, sub, 0, false), nil
}

// open joins the fan-out with a snapshot of what is retained after the
// position after. byPos orders the snapshot by position, the order a
// consumer group commits in; a plain subscription interleaves its rings
// deterministically instead.
func (m *Memory) open(ctx context.Context, sub Subscription, after uint64, byPos bool) *memStream {
	s := &memStream{
		m:      m,
		sub:    sub,
		after:  after,
		ch:     make(chan stored, m.backlog),
		closed: make(chan struct{}),
		hup:    make(chan struct{}),
	}

	m.mu.Lock()
	// Snapshot and join in one critical section.
	if !sub.matchesNothing() {
		keys := make([]string, 0, len(m.rings))
		for key := range m.rings {
			keys = append(keys, key)
		}
		slices.Sort(keys) // deterministic interleaving across rings
		for _, key := range keys {
			for _, e := range m.rings[key].retained(nil) {
				if e.pos > after && sub.Accepts(e.e) {
					s.catchup = append(s.catchup, e)
				}
			}
		}
		if byPos {
			slices.SortFunc(s.catchup, func(a, b stored) int { return cmp.Compare(a.pos, b.pos) })
		}
	}
	m.subs[s] = struct{}{}
	m.mu.Unlock()

	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = s.Close()
			case <-s.closed:
			}
		}()
	}
	return s
}

// Consume implements Bus.
func (m *Memory) Consume(ctx context.Context, group string, sub Subscription) (GroupStream, error) {
	if err := validateGroup(group, sub); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return newGroupStream(ctx, m, group, sub, m.opts.lease), nil
}

// claim implements leases. A group seen for the first time starts at the
// oldest event retained.
func (m *Memory) claim(ctx context.Context, group, tenant, owner string, ttl time.Duration) (uint64, bool, error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	now := m.opts.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	k := groupKey{group, tenant}
	l := m.groups[k]
	if l == nil {
		l = &memLease{}
		m.groups[k] = l
	}
	if l.owner != owner && l.owner != "" && !now.After(l.until) {
		return 0, false, nil
	}
	l.owner, l.until = owner, now.Add(ttl)
	return l.cursor, true, nil
}

// commit implements leases.
func (m *Memory) commit(ctx context.Context, group, tenant, owner string, pos uint64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.groups[groupKey{group, tenant}]
	if l == nil || l.owner != owner {
		return false, nil
	}
	l.cursor = max(l.cursor, pos)
	return true, nil
}

// release implements leases.
func (m *Memory) release(_ context.Context, group, tenant, owner string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.groups[groupKey{group, tenant}]; l != nil && l.owner == owner {
		l.owner, l.until = "", time.Time{}
	}
	return nil
}

// read implements leases.
func (m *Memory) read(ctx context.Context, sub Subscription, after uint64) (positioned, error) {
	if sub.matchesNothing() {
		return newEmptyPositioned(ctx), nil
	}
	return m.open(ctx, sub, after, true), nil
}

// memStream is one subscriber: a snapshot to drain, then a bounded buffer fed
// by publishers.
type memStream struct {
	m   *Memory
	sub Subscription
	// after skips every event at or before this position.
	after uint64

	// catchup is owned by Recv alone: it is written once, under m.mu,
	// before the stream is handed to the caller.
	catchup []stored
	ch      chan stored

	once   sync.Once
	closed chan struct{}
	// hup is closed when this subscriber overflows. over guards against
	// closing it twice; both are held under m.mu.
	hup  chan struct{}
	over bool
}

var _ Stream = (*memStream)(nil)

// offer hands e to this subscriber without ever blocking. A full buffer
// terminates the subscriber. Callers hold m.mu.
func (s *memStream) offer(e stored) {
	if s.over || e.pos <= s.after || !s.sub.Accepts(e.e) {
		return
	}
	select {
	case s.ch <- e:
	default:
		s.over = true
		close(s.hup)
	}
}

// Recv implements Stream.
func (s *memStream) Recv(ctx context.Context) (event.Envelope, error) {
	d, err := s.next(ctx)
	return d.Event, err
}

// next drains the snapshot, then the live buffer, and reports overflow only
// once the buffer it already holds is empty.
func (s *memStream) next(ctx context.Context) (Delivery, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Delivery{}, err
		}
		select {
		case <-s.closed:
			return Delivery{}, ErrClosed
		default:
		}
		if len(s.catchup) > 0 {
			e := s.catchup[0]
			s.catchup[0] = stored{}
			s.catchup = s.catchup[1:]
			return decodeStored(e)
		}
		select {
		case e := <-s.ch:
			return decodeStored(e)
		default:
		}
		select {
		case <-s.hup:
			return Delivery{}, fmt.Errorf("bus: subscription fell more than %d events behind: %w", s.m.backlog, ErrSlowConsumer)
		default:
		}
		select {
		case e := <-s.ch:
			return decodeStored(e)
		case <-s.hup:
		case <-s.closed:
		case <-ctx.Done():
			return Delivery{}, ctx.Err()
		}
	}
}

// Close implements Stream.
func (s *memStream) Close() error {
	s.once.Do(func() {
		s.m.mu.Lock()
		delete(s.m.subs, s)
		s.m.mu.Unlock()
		close(s.closed)
	})
	return nil
}

// decodeStored decodes a fresh copy of a retained event.
func decodeStored(s stored) (Delivery, error) {
	var e event.Envelope
	if err := json.Unmarshal(s.raw, &e); err != nil {
		return Delivery{}, fmt.Errorf("bus: decode %s %q: %w", s.e.Type, s.e.ID, err)
	}
	return Delivery{Event: e, Pos: s.pos}, nil
}
