package pgkit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// lazyPool returns a pool that parses but never connects: pgxpool dials
// lazily, and nothing listens on port 1, so every dial fails fast.
func lazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://unit:unit@127.0.0.1:1/unit?connect_timeout=1")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestApplyRejectsMissingArguments(t *testing.T) {
	ctx := context.Background()
	if err := Apply(ctx, lazyPool(t), "", "SELECT 1"); err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Fatalf("Apply with no name = %v, want a name error", err)
	}
	var pool *pgxpool.Pool
	var conn *pgx.Conn
	for _, db := range []Beginner{nil, pool, conn} {
		if err := Apply(ctx, db, "x", "SELECT 1"); err == nil || !strings.Contains(err.Error(), "no database") {
			t.Fatalf("Apply(%T(nil)) = %v, want a no-database error", db, err)
		}
	}
}

func TestApplyReportsConnectionFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := Apply(ctx, lazyPool(t), "unreachable", "SELECT 1")
	if err == nil || !strings.Contains(err.Error(), `pgkit: apply "unreachable"`) {
		t.Fatalf("Apply on an unreachable pool = %v, want a wrapped error naming the set", err)
	}
}

func TestPreview(t *testing.T) {
	if got := preview("  CREATE TABLE\n\tx (a int)  "); got != "CREATE TABLE x (a int)" {
		t.Fatalf("preview collapses whitespace: got %q", got)
	}
	long := strings.Repeat("é", 40) // 80 bytes, two per rune
	got := preview(long)
	if !strings.HasSuffix(got, "...") || len(got) > 63 || strings.ContainsRune(got, '�') {
		t.Fatalf("preview(%d bytes) = %q, want a cut on a rune boundary", len(long), got)
	}
}

func TestNewListenerValidates(t *testing.T) {
	if _, err := NewListener(nil); err == nil {
		t.Fatal("NewListener(nil) = nil error, want error")
	}
	pool := lazyPool(t)
	for name, opt := range map[string]ListenerOption{
		"buffer 1":        WithBuffer(1),
		"zero backoff":    WithBackoff(0, time.Second),
		"inverted window": WithBackoff(time.Second, time.Millisecond),
	} {
		if _, err := NewListener(pool, opt); err == nil {
			t.Errorf("NewListener with %s = nil error, want error", name)
		}
	}
	l, err := NewListener(pool, WithBuffer(2), WithBackoff(time.Millisecond, time.Millisecond))
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	if l.buffer != 2 || l.minBackoff != time.Millisecond || l.maxBackoff != time.Millisecond {
		t.Fatalf("options not applied: %+v", l)
	}
}

func TestListenRejectsChannelsPostgresWouldNotAgreeOn(t *testing.T) {
	l, err := NewListener(lazyPool(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, c := range []string{"", strings.Repeat("c", 64), "bad\xff", "nul\x00"} {
		if _, err := l.Listen(ctx, c); err == nil {
			t.Errorf("Listen(%q) = nil error, want error", c)
		}
	}
	if err := validChannel(strings.Repeat("c", 63)); err != nil {
		t.Errorf("a 63-byte channel is valid: %v", err)
	}
	if err := validChannel(`Mixed "Case"; --`); err != nil {
		t.Errorf("any UTF-8 name is a channel once quoted: %v", err)
	}
	done, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := l.Listen(done, "c"); err == nil {
		t.Error("Listen on a done context = nil error, want error")
	}
}

// Without a database the Listener keeps retrying, never claims to be
// listening, and a subscription still ends with its context.
func TestListenWithoutDatabase(t *testing.T) {
	l, err := NewListener(lazyPool(t), WithBackoff(time.Millisecond, 5*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := l.Listen(ctx, "c")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if l.Listening() {
		t.Fatal("Listening() = true with no database")
	}
	cancel()
	select {
	case n, ok := <-ch:
		if ok {
			t.Fatalf("got %+v with no database, want the channel closed", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subscription channel not closed after its context ended")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.count != 0 || len(l.subs) != 0 || l.stop != nil {
		t.Fatalf("listener state after the last subscription: count=%d subs=%d stop=%v", l.count, len(l.subs), l.stop != nil)
	}
}

// offer never blocks and keeps its promise: a full buffer always ends in a
// Resync, so whatever it drops is covered by a re-read.
func TestOfferFoldsOverflowIntoResync(t *testing.T) {
	note := func(p string) Notification { return Notification{Channel: "c", Payload: p} }
	resync := Notification{Channel: "c", Resync: true}

	s := &subscriber{ch: make(chan Notification, 3)}
	s.offer(resync)
	s.offer(note("a"))
	s.offer(note("b")) // the last free slot: becomes a Resync
	s.offer(note("c")) // full: dropped, covered by the queued Resync
	s.offer(resync)    // full: dropped, the queued Resync is the same message
	want := []Notification{resync, note("a"), resync}
	if got := drain(s.ch); !equal(got, want) {
		t.Fatalf("after overflow got %+v, want %+v", got, want)
	}

	s.offer(note("d"))
	s.offer(resync)
	s.offer(resync) // a Resync may take the last slot
	if got := drain(s.ch); !equal(got, []Notification{note("d"), resync, resync}) {
		t.Fatalf("after draining got %+v", got)
	}

	// Under any interleaving of offers and reads, a buffer that is full
	// ends in a Resync.
	s = &subscriber{ch: make(chan Notification, 2)}
	for i := range 200 {
		if i%3 == 0 && len(s.ch) > 0 {
			<-s.ch
		}
		s.offer(note("x"))
		if len(s.ch) == cap(s.ch) {
			got := drain(s.ch)
			if !got[len(got)-1].Resync {
				t.Fatalf("round %d: full buffer %+v does not end in a Resync", i, got)
			}
		}
	}
}

func drain(ch chan Notification) []Notification {
	var out []Notification
	for {
		select {
		case n := <-ch:
			out = append(out, n)
		default:
			return out
		}
	}
}

func equal(a, b []Notification) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
