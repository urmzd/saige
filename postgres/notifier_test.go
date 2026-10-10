package postgres

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/agent/types"
)

// notifyDatabase returns a pool on a fresh database holding only the
// Notifier and CacheStore tables, so it runs on a server without pgvector.
func notifyDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := freshDatabase(t)
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if err := execScript(ctx, conn.Conn(), notifySQL); err != nil {
		t.Fatal(err)
	}
	return pool
}

func receive(t *testing.T, ch <-chan types.Notification) types.Notification {
	t.Helper()
	select {
	case n, ok := <-ch:
		if !ok {
			t.Fatal("subscription closed")
		}
		return n
	case <-time.After(10 * time.Second):
		t.Fatal("no notification")
	}
	return types.Notification{}
}

func TestNotifierPublishSubscribe(t *testing.T) {
	pool := notifyDatabase(t)
	ctx := context.Background()
	pub := NewNotifier(pool, NotifierOptions{})
	defer pub.Close(ctx)
	sub := NewNotifier(pool, NotifierOptions{})
	defer sub.Close(ctx)

	a, cancelA, err := sub.Subscribe(ctx, "saige.test")
	if err != nil {
		t.Fatal(err)
	}
	defer cancelA()
	b, cancelB, err := sub.Subscribe(ctx, "saige.test")
	if err != nil {
		t.Fatal(err)
	}
	defer cancelB()

	for _, payload := range [][]byte{[]byte("hello"), {0, 1, 2, 0xff}, nil} {
		if err := pub.Publish(ctx, "saige.test", payload); err != nil {
			t.Fatal(err)
		}
		for _, ch := range []<-chan types.Notification{a, b} {
			n := receive(t, ch)
			if n.Channel != "saige.test" || !bytes.Equal(n.Payload, payload) {
				t.Fatalf("got %q on %s, want %q", n.Payload, n.Channel, payload)
			}
		}
	}

	if err := pub.Publish(ctx, "bad channel", nil); !errors.Is(err, types.ErrInvalidChannel) {
		t.Fatalf("Publish(invalid) = %v", err)
	}
}

func TestNotifierLargePayload(t *testing.T) {
	pool := notifyDatabase(t)
	ctx := context.Background()
	n := NewNotifier(pool, NotifierOptions{PayloadTTL: time.Hour})
	defer n.Close(ctx)
	ch, cancel, err := n.Subscribe(ctx, "big")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	text := []byte(strings.Repeat("x", 3*MaxNotifyPayload))
	binary := bytes.Repeat([]byte{0, 0xfe}, MaxNotifyPayload)
	for _, payload := range [][]byte{text, binary} {
		if err := n.Publish(ctx, "big", payload); err != nil {
			t.Fatal(err)
		}
		if got := receive(t, ch); !bytes.Equal(got.Payload, payload) {
			t.Fatalf("large payload: got %d bytes, want %d", len(got.Payload), len(payload))
		}
	}
	var stored int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM saige_notifications`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 2 {
		t.Fatalf("stored rows = %d, want 2", stored)
	}

	// Expired rows are swept.
	if _, err := pool.Exec(ctx, `UPDATE saige_notifications SET expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	removed, err := n.SweepNotifications(ctx)
	if err != nil || removed != 2 {
		t.Fatalf("SweepNotifications = %d, %v", removed, err)
	}
}

func TestNotifierReconnect(t *testing.T) {
	pool := notifyDatabase(t)
	ctx := context.Background()
	var reconnects atomic.Int32
	n := NewNotifier(pool, NotifierOptions{
		MinBackoff:      10 * time.Millisecond,
		MaxBackoff:      50 * time.Millisecond,
		ApplicationName: "saige-notifier-reconnect",
		OnReconnect:     func() { reconnects.Add(1) },
	})
	defer n.Close(ctx)
	ch, cancel, err := n.Subscribe(ctx, "survive")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	other, cancelOther, err := n.Subscribe(ctx, "survive.too")
	if err != nil {
		t.Fatal(err)
	}
	defer cancelOther()

	var killed int
	err = pool.QueryRow(ctx,
		`SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
		 WHERE application_name = 'saige-notifier-reconnect' AND datname = current_database()`,
	).Scan(&killed)
	if err != nil || killed != 1 {
		t.Fatalf("terminated %d listeners: %v", killed, err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for reconnects.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("listener did not reconnect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, c := range []struct {
		name string
		ch   <-chan types.Notification
	}{{"survive", ch}, {"survive.too", other}} {
		if err := n.Publish(ctx, c.name, []byte("after")); err != nil {
			t.Fatal(err)
		}
		if got := receive(t, c.ch); string(got.Payload) != "after" {
			t.Fatalf("%s: got %q", c.name, got.Payload)
		}
	}
}

func TestNotifierCloseAndCancel(t *testing.T) {
	pool := notifyDatabase(t)
	n := NewNotifier(pool, NotifierOptions{})
	ctx, stop := context.WithCancel(context.Background())
	ch, cancel, err := n.Subscribe(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	stop()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("unexpected notification")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("context cancellation did not close the subscription")
	}

	ch, cancel, err = n.Subscribe(context.Background(), "c")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if err := n.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-ch; ok {
		t.Fatal("subscription open after Close")
	}
	if err := n.Publish(context.Background(), "c", nil); !errors.Is(err, types.ErrNotifierClosed) {
		t.Fatalf("Publish after Close = %v", err)
	}
	if _, _, err := n.Subscribe(context.Background(), "c"); !errors.Is(err, types.ErrNotifierClosed) {
		t.Fatalf("Subscribe after Close = %v", err)
	}
	_ = n.Close(context.Background())
}

// Subscribe waits for the listener; a cancelled context ends the wait.
func TestNotifierSubscribeRespectsContext(t *testing.T) {
	pool := notifyDatabase(t)
	n := NewNotifier(pool, NotifierOptions{})
	defer n.Close(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := n.Subscribe(ctx, "c"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Subscribe = %v", err)
	}
}
