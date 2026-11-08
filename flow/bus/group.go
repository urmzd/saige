package bus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// leases is what a backend keeps for its consumer groups: one cursor and one
// lease per (group, tenant). groupStream is written once on top of it, so
// every backend runs the same lease protocol.
type leases interface {
	// claim takes the lease for owner, or renews it, when it is free,
	// expired or already owner's, and returns the committed cursor. ok is
	// false when another member holds it.
	claim(ctx context.Context, group, tenant, owner string, ttl time.Duration) (cursor uint64, ok bool, err error)
	// commit moves the cursor forward to pos when owner holds the lease.
	// ok is false when it does not.
	commit(ctx context.Context, group, tenant, owner string, pos uint64) (ok bool, err error)
	// release frees the lease when owner holds it.
	release(ctx context.Context, group, tenant, owner string) error
	// read streams the events sub accepts with a position after after, in
	// position order. Cancelling ctx ends it.
	read(ctx context.Context, sub Subscription, after uint64) (positioned, error)
}

// positioned is a stream of events with their positions.
type positioned interface {
	next(ctx context.Context) (Delivery, error)
	Close() error
}

// releaseTimeout bounds the release a Close makes on its way out.
const releaseTimeout = 5 * time.Second

// groupStream is one member of a consumer group on one tenant.
type groupStream struct {
	l     leases
	group string
	owner string
	sub   Subscription
	ttl   time.Duration
	retry time.Duration

	// ctx is the context Consume was given: when it ends the stream ends,
	// and the lease is left to expire.
	ctx context.Context

	mu sync.Mutex
	// inner is the current term's reader. Recv, which serves one
	// goroutine, sets it under mu and reads it without; Close reads it
	// under mu.
	inner   positioned
	held    bool
	lost    bool
	end     context.CancelFunc // ends the current term: its reader and renewer
	renewed chan struct{}      // closed when the current term's renewer stops

	once   sync.Once
	closed chan struct{}
}

var _ GroupStream = (*groupStream)(nil)

func newGroupStream(ctx context.Context, l leases, group string, sub Subscription, ttl time.Duration) *groupStream {
	return &groupStream{
		l:      l,
		group:  group,
		owner:  newOwner(),
		sub:    sub,
		ttl:    ttl,
		retry:  max(ttl/4, 10*time.Millisecond),
		ctx:    ctx,
		closed: make(chan struct{}),
	}
}

// newOwner names one member: unique per Consume, across every process.
func newOwner() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Recv implements GroupStream.
func (g *groupStream) Recv(ctx context.Context) (Delivery, error) {
	if err := g.ended(ctx); err != nil {
		return Delivery{}, err
	}
	if g.inner == nil {
		if err := g.acquire(ctx); err != nil {
			return Delivery{}, err
		}
	}
	d, err := g.inner.next(ctx)
	// An event read after the lease was lost belongs to the next owner.
	if lost := g.ended(ctx); lost != nil {
		return Delivery{}, lost
	}
	return d, err
}

// ended reports why the stream cannot deliver, if it cannot.
func (g *groupStream) ended(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-g.closed:
		return ErrClosed
	default:
	}
	g.mu.Lock()
	lost := g.lost
	g.mu.Unlock()
	if lost {
		return ErrLeaseLost
	}
	if g.ctx.Err() != nil {
		return ErrClosed
	}
	return nil
}

// acquire waits for the lease and opens the reader at the committed cursor.
func (g *groupStream) acquire(ctx context.Context) error {
	for {
		cursor, ok, err := g.l.claim(ctx, g.group, g.sub.Tenant, g.owner, g.ttl)
		if err != nil {
			if e := g.ended(ctx); e != nil {
				return e
			}
			return err
		}
		if ok {
			return g.begin(cursor)
		}
		t := time.NewTimer(g.retry)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-g.ctx.Done():
			t.Stop()
			return ErrClosed
		case <-g.closed:
			t.Stop()
			return ErrClosed
		}
	}
}

// begin starts a term: the reader from cursor and the renewer.
func (g *groupStream) begin(cursor uint64) error {
	term, end := context.WithCancel(g.ctx)
	inner, err := g.l.read(term, g.sub, cursor)
	if err != nil {
		end()
		return err
	}
	renewed := make(chan struct{})
	g.mu.Lock()
	select {
	case <-g.closed:
		g.mu.Unlock()
		end()
		_ = inner.Close()
		return ErrClosed
	default:
	}
	g.inner, g.held, g.end, g.renewed = inner, true, end, renewed
	g.mu.Unlock()
	go g.renew(term, renewed)
	return nil
}

// renew keeps the lease while the term lasts. A lease another member took is
// lost at once; one that could not be renewed for a whole lease, because
// the backend kept failing, is given up, since another member may hold it
// by now.
func (g *groupStream) renew(term context.Context, done chan struct{}) {
	defer close(done)
	tick := time.NewTicker(g.ttl / 3)
	defer tick.Stop()
	last := time.Now()
	for {
		select {
		case <-term.Done():
			return
		case <-tick.C:
		}
		_, ok, err := g.l.claim(term, g.group, g.sub.Tenant, g.owner, g.ttl)
		switch {
		case term.Err() != nil:
			return
		case err == nil && ok:
			last = time.Now()
			continue
		case err != nil && time.Since(last) < g.ttl:
			continue
		}
		g.mu.Lock()
		g.lost, g.held = true, false
		end := g.end
		g.mu.Unlock()
		end()
		return
	}
}

// Commit implements GroupStream.
func (g *groupStream) Commit(ctx context.Context, pos uint64) error {
	if pos > maxPos {
		return fmt.Errorf("%w: position %d does not fit in a bigint", ErrInvalid, pos)
	}
	ok, err := g.l.commit(ctx, g.group, g.sub.Tenant, g.owner, pos)
	if err != nil {
		return err
	}
	if !ok {
		return ErrLeaseLost
	}
	return nil
}

// Close implements GroupStream.
func (g *groupStream) Close() error {
	g.once.Do(func() {
		g.mu.Lock()
		close(g.closed)
		held, end, renewed, inner := g.held, g.end, g.renewed, g.inner
		g.held = false
		g.mu.Unlock()
		if end != nil {
			end()
			<-renewed
		}
		if inner != nil {
			_ = inner.Close()
		}
		if held {
			ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
			defer cancel()
			_ = g.l.release(ctx, g.group, g.sub.Tenant, g.owner)
		}
	})
	return nil
}

// maxPos is the largest position a backend stores.
const maxPos = uint64(1<<63 - 1)

// emptyPositioned is the reader of a subscription that matches nothing: it
// holds the lease and delivers nothing until its term ends.
type emptyPositioned struct {
	term   context.Context
	once   sync.Once
	closed chan struct{}
}

func newEmptyPositioned(term context.Context) *emptyPositioned {
	return &emptyPositioned{term: term, closed: make(chan struct{})}
}

func (e *emptyPositioned) next(ctx context.Context) (Delivery, error) {
	select {
	case <-ctx.Done():
		return Delivery{}, ctx.Err()
	case <-e.term.Done():
		return Delivery{}, ErrClosed
	case <-e.closed:
		return Delivery{}, ErrClosed
	}
}

func (e *emptyPositioned) Close() error {
	e.once.Do(func() { close(e.closed) })
	return nil
}
