package pgkit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Listener defaults.
const (
	defaultBuffer     = 64
	defaultMinBackoff = 100 * time.Millisecond
	defaultMaxBackoff = 10 * time.Second

	// maxChannelLen is the longest channel name Postgres keeps: LISTEN
	// truncates identifiers to NAMEDATALEN-1 bytes and pg_notify refuses
	// anything longer, so a longer name would listen on one channel and
	// notify another.
	maxChannelLen = 63
)

// Notification is one message on a subscription.
type Notification struct {
	// Channel is the channel the subscription listens on.
	Channel string
	// Payload is the NOTIFY payload; empty on a Resync.
	Payload string
	// Resync marks the message a subscription gets each time its channel
	// is listened to on a fresh connection, the first time included, and
	// in place of notifications a full buffer could not hold. Anything
	// published before a Resync may not have been delivered, so the
	// subscriber re-reads the state it watches; anything published after
	// it is delivered, until the next Resync.
	Resync bool
}

// ListenerOption configures a Listener.
type ListenerOption func(*Listener) error

// WithBuffer sets each subscription's channel capacity (default 64, at
// least 2). A subscriber that falls this far behind never blocks the
// others: its newest notifications are replaced by one Resync.
func WithBuffer(n int) ListenerOption {
	return func(l *Listener) error {
		if n < 2 {
			return fmt.Errorf("pgkit: buffer must be at least 2, got %d", n)
		}
		l.buffer = n
		return nil
	}
}

// WithBackoff bounds the wait between reconnect attempts: it starts at lo
// after a connection that got as far as listening, and doubles up to hi
// while attempts keep failing. The default is 100ms to 10s.
func WithBackoff(lo, hi time.Duration) ListenerOption {
	return func(l *Listener) error {
		if lo <= 0 || hi < lo {
			return fmt.Errorf("pgkit: backoff wants 0 < lo <= hi, got %v and %v", lo, hi)
		}
		l.minBackoff, l.maxBackoff = lo, hi
		return nil
	}
}

// Listener multiplexes LISTEN/NOTIFY over one dedicated connection, for as
// many channels and subscribers as a process has. One Listener per process
// (or per pool) is the intent: hand the same one to every component that
// needs notifications.
//
// It holds a connection only while at least one subscription is live. The
// first Listen takes a connection out of the pool for good (it carries
// LISTEN state, so it is closed rather than handed back) and the end of the
// last subscription closes it. In between, the Listener LISTENs to every
// channel with a subscriber and UNLISTENs the rest. When the connection
// fails it reconnects with backoff, listens again, and sends every
// subscriber a Resync, because notifications published while it was down
// are gone.
//
// Delivery never blocks the connection: each subscription has its own
// buffered channel, and a subscriber that stops reading only loses its own
// notifications, folded into a Resync.
//
// A Listener does not own the pool. End every subscription before closing
// it. Behind a transaction-pooling proxy LISTEN does not work; treat
// notifications as a wake-up over a poll, never as the only signal.
type Listener struct {
	pool       *pgxpool.Pool
	buffer     int
	minBackoff time.Duration
	maxBackoff time.Duration

	mu    sync.Mutex
	subs  map[string]map[*subscriber]struct{}
	count int
	// gen names the live connection goroutine; starting and stopping one
	// bump it, so a goroutine that was stopped changes nothing after.
	gen   uint64
	stop  context.CancelFunc
	wake  context.CancelFunc
	dirty bool
	up    bool
}

// subscriber is one Listen call. Its channel is written only with the
// Listener's mutex held, so a send and the close at unsubscribe never race.
type subscriber struct {
	ch chan Notification
	// synced reports that the subscriber has had its Resync on the
	// current connection; until then its channel's notifications skip it.
	synced bool
}

// NewListener returns a Listener on pool. It performs no I/O: the
// connection is taken by the first Listen.
func NewListener(pool *pgxpool.Pool, opts ...ListenerOption) (*Listener, error) {
	if pool == nil {
		return nil, errors.New("pgkit: listener: pool is required")
	}
	l := &Listener{
		pool:       pool,
		buffer:     defaultBuffer,
		minBackoff: defaultMinBackoff,
		maxBackoff: defaultMaxBackoff,
	}
	for _, opt := range opts {
		if err := opt(l); err != nil {
			return nil, err
		}
	}
	return l, nil
}

// Listen subscribes to channel until ctx is done, then closes the returned
// channel. It returns at once, without waiting for the connection: the first
// message on every subscription is a Resync, sent once the channel is being
// listened to, so a caller that must not miss a notification published after
// some point waits for that Resync, then reads its state, then acts on what
// follows.
//
// channel is used exactly as given (quoted, case-sensitive), as pg_notify
// takes it: at most 63 bytes of valid UTF-8 without NUL.
func (l *Listener) Listen(ctx context.Context, channel string) (<-chan Notification, error) {
	if err := validChannel(channel); err != nil {
		return nil, fmt.Errorf("pgkit: listen: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("pgkit: listen %q: %w", channel, err)
	}
	s := &subscriber{ch: make(chan Notification, l.buffer)}

	l.mu.Lock()
	if l.subs == nil {
		l.subs = make(map[string]map[*subscriber]struct{})
	}
	set := l.subs[channel]
	if set == nil {
		set = make(map[*subscriber]struct{})
		l.subs[channel] = set
	}
	set[s] = struct{}{}
	l.count++
	if l.count == 1 {
		runCtx, cancel := context.WithCancel(context.Background())
		l.gen++
		l.stop = cancel
		go l.run(runCtx, l.gen)
	} else {
		l.poke()
	}
	l.mu.Unlock()

	context.AfterFunc(ctx, func() { l.unsubscribe(channel, s) })
	return s.ch, nil
}

// Listening reports whether the connection is up and listening to every
// channel subscribed at its last sync. It is false while nothing is
// subscribed. Callers use it to stretch a safety poll while notifications
// are doing the waking.
func (l *Listener) Listening() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.up
}

// unsubscribe ends one subscription and, with it the last, the connection.
func (l *Listener) unsubscribe(channel string, s *subscriber) {
	l.mu.Lock()
	defer l.mu.Unlock()
	set := l.subs[channel]
	if _, ok := set[s]; !ok {
		return
	}
	delete(set, s)
	close(s.ch)
	if len(set) == 0 {
		delete(l.subs, channel)
	}
	l.count--
	if l.count > 0 {
		l.poke()
		return
	}
	l.stop()
	l.stop, l.wake = nil, nil
	l.gen++
	l.up, l.dirty = false, false
}

// poke asks the connection goroutine to sync its LISTENs and send pending
// Resyncs. Called with mu held.
func (l *Listener) poke() {
	l.dirty = true
	if l.wake != nil {
		l.wake()
	}
}

// run owns the connection for one generation, reconnecting until ctx ends.
func (l *Listener) run(ctx context.Context, gen uint64) {
	backoff := l.minBackoff
	for {
		if l.session(ctx, gen) {
			backoff = l.minBackoff
		}
		l.mu.Lock()
		if l.gen == gen {
			l.up, l.wake = false, nil
		}
		l.mu.Unlock()

		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff = min(2*backoff, l.maxBackoff)
	}
}

// session holds one connection until it fails or ctx ends, and reports
// whether it got as far as listening.
func (l *Listener) session(ctx context.Context, gen uint64) (listened bool) {
	pc, err := l.pool.Acquire(ctx)
	if err != nil {
		return false
	}
	conn := pc.Hijack()
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = conn.Close(closeCtx)
		cancel()
	}()

	// Every subscriber is owed a Resync on this connection.
	l.mu.Lock()
	if l.gen != gen {
		l.mu.Unlock()
		return false
	}
	for _, set := range l.subs {
		for s := range set {
			s.synced = false
		}
	}
	l.mu.Unlock()

	listening := make(map[string]bool)
	for {
		l.mu.Lock()
		if l.gen != gen {
			l.mu.Unlock()
			return listened
		}
		want := make(map[string]bool, len(l.subs))
		for c := range l.subs {
			want[c] = true
		}
		l.dirty = false
		l.mu.Unlock()

		if err := syncListens(ctx, conn, listening, want); err != nil {
			return listened
		}

		waitCtx, cancel := context.WithCancel(ctx)
		l.mu.Lock()
		if l.gen != gen {
			l.mu.Unlock()
			cancel()
			return listened
		}
		l.up, listened = true, true
		for c, set := range l.subs {
			if !listening[c] {
				continue // subscribed after the snapshot; the next round
			}
			for s := range set {
				if !s.synced {
					s.synced = true
					s.offer(Notification{Channel: c, Resync: true})
				}
			}
		}
		l.wake = cancel
		if l.dirty {
			cancel()
		}
		l.mu.Unlock()

		n, err := conn.WaitForNotification(waitCtx)
		woken := waitCtx.Err() != nil
		cancel()
		l.mu.Lock()
		if l.gen == gen {
			l.wake = nil
		}
		l.mu.Unlock()

		if n != nil {
			l.dispatch(gen, n)
		}
		// A poke interrupts the wait with a deadline, which leaves the
		// connection usable; anything else ends the session.
		if err != nil && (ctx.Err() != nil || !woken || conn.IsClosed()) {
			return listened
		}
	}
}

// dispatch hands one notification to its channel's synced subscribers.
func (l *Listener) dispatch(gen uint64, n *pgconn.Notification) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.gen != gen {
		return
	}
	for s := range l.subs[n.Channel] {
		if s.synced {
			s.offer(Notification{Channel: n.Channel, Payload: n.Payload})
		}
	}
}

// offer queues n without blocking, keeping the last free slot for a Resync.
// The buffer only ever fills with a Resync, so when it is full the last
// queued message is a Resync, and that Resync already covers n: everything
// n announces was committed before n arrived, so a re-read after the Resync
// sees it. Called with the Listener's mutex held.
func (s *subscriber) offer(n Notification) {
	switch free := cap(s.ch) - len(s.ch); {
	case free > 1, free == 1 && n.Resync:
		s.ch <- n
	case free == 1:
		s.ch <- Notification{Channel: n.Channel, Resync: true}
	}
}

// syncListens issues LISTEN and UNLISTEN so conn listens to exactly want,
// recording the result in listening.
func syncListens(ctx context.Context, conn *pgx.Conn, listening, want map[string]bool) error {
	for c := range want {
		if listening[c] {
			continue
		}
		if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{c}.Sanitize()); err != nil {
			return err
		}
		listening[c] = true
	}
	for c := range listening {
		if want[c] {
			continue
		}
		if _, err := conn.Exec(ctx, "UNLISTEN "+pgx.Identifier{c}.Sanitize()); err != nil {
			return err
		}
		delete(listening, c)
	}
	return nil
}

// validChannel rejects names LISTEN and pg_notify would not agree on.
func validChannel(c string) error {
	switch {
	case c == "":
		return errors.New("channel is required")
	case len(c) > maxChannelLen:
		return fmt.Errorf("channel %q is longer than %d bytes", c, maxChannelLen)
	case !utf8.ValidString(c):
		return fmt.Errorf("channel %q is not valid UTF-8", c)
	case strings.ContainsRune(c, 0):
		return fmt.Errorf("channel %q contains NUL", c)
	}
	return nil
}
