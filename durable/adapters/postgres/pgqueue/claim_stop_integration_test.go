//go:build integration

package pgqueue_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/durable"
	"github.com/urmzd/saige/durable/adapters/postgres/pgqueue"
	"github.com/urmzd/saige/durable/ledger"
	"github.com/urmzd/saige/durable/run"
	"github.com/urmzd/saige/durable/worker"
)

// answerGate holds back, on every connection dialed through it, the first
// answer after arm that carries a row (a claim that took a lease; empty
// polls carry none). It reads that answer through its ReadyForQuery before
// holding it, so the server has finished and committed the statement while
// the client has heard nothing. A held read still honors the connection's
// read deadline, as a socket does: that deadline is how pgx abandons a
// statement whose context is cancelled.
type answerGate struct {
	armed       atomic.Bool
	held        chan struct{} // closed once an answer is held
	heldOnce    sync.Once
	release     chan struct{} // closed to deliver it
	releaseOnce sync.Once
	cut         chan struct{} // closed to break the connection instead
	cutOnce     sync.Once
}

func newAnswerGate() *answerGate {
	return &answerGate{held: make(chan struct{}), release: make(chan struct{}), cut: make(chan struct{})}
}

func (g *answerGate) arm()           { g.armed.Store(true) }
func (g *answerGate) releaseAnswer() { g.releaseOnce.Do(func() { close(g.release) }) }
func (g *answerGate) cutAnswer()     { g.cutOnce.Do(func() { close(g.cut) }) }

func (g *answerGate) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return &gatedConn{Conn: c, gate: g, moved: make(chan struct{})}, nil
}

// gatedConn is a net.Conn whose reads pass through the gate.
type gatedConn struct {
	net.Conn
	gate *answerGate

	// Read state; pgx never reads one connection concurrently.
	scan    msgScanner
	pending []byte // read from the server, not yet delivered
	holding bool

	mu           sync.Mutex
	readDeadline time.Time
	moved        chan struct{} // closed and replaced when the deadline moves
}

func (c *gatedConn) Read(p []byte) (int, error) {
	if len(c.pending) == 0 {
		buf := make([]byte, max(len(p), 1))
		n, err := c.Conn.Read(buf)
		if n == 0 {
			return 0, err
		}
		chunk := buf[:n]
		sawRow := c.scan.feed(chunk, 'D')
		if !sawRow || !c.gate.armed.CompareAndSwap(true, false) {
			return copy(p, chunk), err
		}
		// Read on through ReadyForQuery: the statement's transaction has
		// then ended on the server.
		for !c.scan.sawReady() {
			more := make([]byte, 4096)
			m, rerr := c.Conn.Read(more)
			c.scan.feed(more[:m], 0)
			chunk = append(chunk, more[:m]...)
			if rerr != nil {
				c.pending = chunk
				return 0, rerr
			}
		}
		c.pending = chunk
		c.holding = true
		c.gate.heldOnce.Do(func() { close(c.gate.held) })
	}
	if c.holding {
		if err := c.waitRelease(); err != nil {
			return 0, err
		}
		c.holding = false
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

// waitRelease blocks until the gate releases or the read deadline passes,
// or breaks the connection when the gate cuts the answer.
func (c *gatedConn) waitRelease() error {
	for {
		c.mu.Lock()
		dl, moved := c.readDeadline, c.moved
		c.mu.Unlock()
		if dl.IsZero() {
			select {
			case <-c.gate.release:
				return nil
			case <-c.gate.cut:
				_ = c.Close()
				return io.ErrUnexpectedEOF
			case <-moved:
				continue
			}
		}
		wait := time.Until(dl)
		if wait <= 0 {
			return os.ErrDeadlineExceeded
		}
		timer := time.NewTimer(wait)
		select {
		case <-c.gate.release:
			timer.Stop()
			return nil
		case <-c.gate.cut:
			timer.Stop()
			_ = c.Close()
			return io.ErrUnexpectedEOF
		case <-timer.C:
		case <-moved:
			timer.Stop()
		}
	}
}

func (c *gatedConn) setReadDeadline(t time.Time) {
	c.mu.Lock()
	c.readDeadline = t
	close(c.moved)
	c.moved = make(chan struct{})
	c.mu.Unlock()
}

func (c *gatedConn) SetDeadline(t time.Time) error {
	c.setReadDeadline(t)
	return c.Conn.SetDeadline(t)
}

func (c *gatedConn) SetReadDeadline(t time.Time) error {
	c.setReadDeadline(t)
	return c.Conn.SetReadDeadline(t)
}

// msgScanner tracks backend message boundaries across reads: a type byte,
// then a big-endian int32 length that counts itself but not the type.
type msgScanner struct {
	hdr  []byte
	body int // bytes of the current message's body still to come
	last byte
}

// feed consumes chunk and reports whether a message of type want starts in
// it.
func (s *msgScanner) feed(chunk []byte, want byte) bool {
	found := false
	for len(chunk) > 0 {
		if s.body > 0 {
			k := min(s.body, len(chunk))
			s.body -= k
			chunk = chunk[k:]
			continue
		}
		k := min(5-len(s.hdr), len(chunk))
		s.hdr = append(s.hdr, chunk[:k]...)
		chunk = chunk[k:]
		if len(s.hdr) == 5 {
			s.last = s.hdr[0]
			found = found || s.last == want
			s.body = int(binary.BigEndian.Uint32(s.hdr[1:])) - 4
			s.hdr = s.hdr[:0]
		}
	}
	return found
}

// sawReady reports whether the stream sits just past a whole ReadyForQuery.
func (s *msgScanner) sawReady() bool {
	return s.last == 'Z' && s.body == 0 && len(s.hdr) == 0
}

// gatedPool opens a pool whose connections are dialed through g, in
// plaintext so the gate sees protocol messages.
func gatedPool(t *testing.T, g *answerGate) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("SAIGE_TEST_POSTGRES_DSN")
	if url == "" {
		url = "postgres://postgres:test@localhost:5433/postgres?sslmode=disable"
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse %q: %v", url, err)
	}
	cfg.ConnConfig.TLSConfig = nil
	cfg.ConnConfig.Fallbacks = nil
	cfg.ConnConfig.DialFunc = g.dial
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pgxpool.NewWithConfig: %v", err)
	}
	t.Cleanup(pool.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return pool
}

// Regression: a worker stopped while its claim's answer is on the way must
// not leave the run leased to nobody. pgx abandons a statement as soon as
// its context is cancelled, but Postgres has already committed the lease;
// the worker, told the claim was cancelled, holds nothing. The run then sat
// leased for the whole TTL and was charged a failure when reclaimed. The
// queue must wait for the answer, and the worker must hand the run back.
func TestWorkerStopDuringClaimLeasesNothingToNobody(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	m := pgqueue.DefaultMapping()
	m.Table = uniqueName("dt_queue_stop")
	q := newQueue(t, pool, m)

	gate := newAnswerGate()
	gq, err := pgqueue.New(gatedPool(t, gate), m, pgqueue.WithPollInterval(5*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	lgr := ledger.NewMemory()
	reg := run.NewRegistry()
	var executions atomic.Int32
	wf := durable.ActivityIn(reg, "claimed-at-stop", func(context.Context, durable.Void) (string, error) {
		executions.Add(1)
		return "done", nil
	})
	h, err := durable.Start(ctx, durable.New(lgr, q), wf, durable.Void{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	gate.arm()
	wctx, stop := context.WithCancel(ctx)
	defer stop()
	w := worker.New(lgr, gq,
		worker.WithRegistry(reg),
		worker.WithLeaseTTL(30*time.Second), // expiry must NOT be the recovery path
		worker.WithJanitorEvery(0),
		worker.WithConcurrency(1),
	)
	done := make(chan struct{})
	go func() { _ = w.Run(wctx); close(done) }()

	select {
	case <-gate.held:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker's claim never answered")
	}
	// The server has committed the lease; the worker has not heard yet.
	var leased bool
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT lease_until IS NOT NULL FROM %s WHERE run_id = $1`, m.Table), h.RunID()).Scan(&leased); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if !leased {
		t.Fatal("setup: the held claim had not taken its lease")
	}

	stop()
	// A queue that abandons the claim lets the worker stop right here,
	// with the answer still held. One that waits cannot stop until the
	// answer arrives, so deliver it after giving an abandoning queue ample
	// time to show itself.
	select {
	case <-done:
	case <-time.After(time.Second):
	}
	gate.releaseAnswer()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not stop after its claim answered")
	}

	if got := executions.Load(); got != 0 {
		t.Errorf("executions = %d, want 0: a stopping worker must not run what it claims", got)
	}
	var (
		attempt, failures int
		leaseNull, ready  bool
	)
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT attempt, failures, lease_until IS NULL, ready_at IS NOT NULL FROM %s WHERE run_id = $1`, m.Table),
		h.RunID()).Scan(&attempt, &failures, &leaseNull, &ready); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if attempt != 1 || failures != 0 || !leaseNull || !ready {
		t.Errorf("row after stop = {attempt %d, failures %d, lease cleared %v, ready %v}, want {1, 0, true, true}: the claim's lease must be handed back",
			attempt, failures, leaseNull, ready)
	}

	// Claimable now, not after the 30s lease, and charged no failure.
	claimCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	it, _, err := q.Claim(claimCtx, time.Minute)
	if err != nil {
		t.Fatalf("run not claimable after the stop (leased to nobody): %v", err)
	}
	if it.RunID != h.RunID() || it.Attempt != 2 || it.Failures != 0 {
		t.Fatalf("reclaim = %+v, want {%s 2 0}", it, h.RunID())
	}
}

// Regression: once the claim statement is sent, an error is not the
// caller's cancellation, even when ctx is done by then. The statement runs
// on a context ctx's cancellation does not reach, so its error has another
// cause, and Postgres may have committed the lease before it: reported as
// context.Canceled, a lost answer read as "stopped before asking, nothing
// taken", while the run sat leased to nobody.
func TestClaimErrorAfterSendIsNotTheCallersCancellation(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	m := pgqueue.DefaultMapping()
	m.Table = uniqueName("dt_queue_cut")
	q := newQueue(t, pool, m)
	if err := q.Enqueue(ctx, "r1", 0); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	gate := newAnswerGate()
	gq, err := pgqueue.New(gatedPool(t, gate), m, pgqueue.WithPollInterval(5*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	gate.arm()
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	res := make(chan error, 1)
	go func() {
		_, _, err := gq.Claim(cctx, time.Minute)
		res <- err
	}()
	select {
	case <-gate.held:
	case <-time.After(10 * time.Second):
		t.Fatal("the claim never answered")
	}
	// Postgres has committed the lease. The caller stops, then the
	// connection breaks before the answer arrives.
	cancel()
	gate.cutAnswer()

	select {
	case err = <-res:
	case <-time.After(10 * time.Second):
		t.Fatal("Claim did not return after its connection broke")
	}
	if err == nil {
		t.Fatal("Claim succeeded, but its answer never arrived")
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("Claim error = %v: a claim sent and committed reads as one cancelled before it was asked", err)
	}
	var leased bool
	if err := pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT lease_until IS NOT NULL FROM %s WHERE run_id = 'r1'`, m.Table)).Scan(&leased); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if !leased {
		t.Error("setup: the cut claim had not taken its lease")
	}
}
