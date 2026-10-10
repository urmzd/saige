package notify

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
)

func recv(t *testing.T, ch <-chan types.Notification) types.Notification {
	t.Helper()
	select {
	case n, ok := <-ch:
		if !ok {
			t.Fatal("subscription closed")
		}
		return n
	case <-time.After(5 * time.Second):
		t.Fatal("no notification")
	}
	return types.Notification{}
}

func closed(t *testing.T, ch <-chan types.Notification) {
	t.Helper()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("unexpected notification")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subscription not closed")
	}
}

func TestMemoryFanOut(t *testing.T) {
	ctx := context.Background()
	m := NewMemory(0)
	defer m.Close(ctx)
	a, cancelA, err := m.Subscribe(ctx, "jobs")
	if err != nil {
		t.Fatal(err)
	}
	defer cancelA()
	b, cancelB, err := m.Subscribe(ctx, "jobs")
	if err != nil {
		t.Fatal(err)
	}
	defer cancelB()
	other, cancelOther, err := m.Subscribe(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	defer cancelOther()

	payload := []byte("hello")
	if err := m.Publish(ctx, "jobs", payload); err != nil {
		t.Fatal(err)
	}
	payload[0] = 'X' // the publisher's buffer is not shared
	for _, ch := range []<-chan types.Notification{a, b} {
		n := recv(t, ch)
		if n.Channel != "jobs" || string(n.Payload) != "hello" {
			t.Fatalf("got %+v", n)
		}
	}
	select {
	case n := <-other:
		t.Fatalf("other channel received %+v", n)
	default:
	}
}

func TestMemoryValidatesChannel(t *testing.T) {
	m := NewMemory(0)
	defer m.Close(context.Background())
	for _, ch := range []string{"", "has space", "semi;colon", string(make([]byte, 64))} {
		if err := m.Publish(context.Background(), ch, nil); !errors.Is(err, types.ErrInvalidChannel) {
			t.Errorf("Publish(%q) = %v", ch, err)
		}
		if _, _, err := m.Subscribe(context.Background(), ch); !errors.Is(err, types.ErrInvalidChannel) {
			t.Errorf("Subscribe(%q) = %v", ch, err)
		}
	}
}

func TestMemoryCancelAndContext(t *testing.T) {
	m := NewMemory(0)
	defer m.Close(context.Background())
	ch, cancel, err := m.Subscribe(context.Background(), "c")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	cancel() // idempotent
	closed(t, ch)

	ctx, stop := context.WithCancel(context.Background())
	ch, cancel, err = m.Subscribe(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	stop()
	closed(t, ch)
	if m.hub.Subscribed("c") {
		t.Fatal("cancelled subscriber still registered")
	}
}

func TestMemoryClose(t *testing.T) {
	m := NewMemory(0)
	ch, cancel, err := m.Subscribe(context.Background(), "c")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	closed(t, ch)
	if err := m.Publish(context.Background(), "c", nil); !errors.Is(err, types.ErrNotifierClosed) {
		t.Fatalf("Publish after Close = %v", err)
	}
	if _, _, err := m.Subscribe(context.Background(), "c"); !errors.Is(err, types.ErrNotifierClosed) {
		t.Fatalf("Subscribe after Close = %v", err)
	}
}

// A full subscriber holds the publisher until the publisher's context ends
// or the subscriber cancels; nothing is dropped silently.
func TestMemorySlowSubscriber(t *testing.T) {
	m := NewMemory(1)
	defer m.Close(context.Background())
	ch, cancel, err := m.Subscribe(context.Background(), "c")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Publish(context.Background(), "c", []byte("1")); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stop()
	if err := m.Publish(ctx, "c", []byte("2")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Publish to full subscriber = %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- m.Publish(context.Background(), "c", []byte("3")) }()
	if n := recv(t, ch); string(n.Payload) != "1" {
		t.Fatalf("got %q", n.Payload)
	}
	if n := recv(t, ch); string(n.Payload) != "3" {
		t.Fatalf("got %q", n.Payload)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	go func() { done <- m.Publish(context.Background(), "c", []byte("4")) }()
	go func() { done <- m.Publish(context.Background(), "c", []byte("5")) }()
	time.Sleep(20 * time.Millisecond)
	cancel() // releases the blocked publisher
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestMemoryConcurrent(t *testing.T) {
	m := NewMemory(4)
	defer m.Close(context.Background())
	ctx := context.Background()
	const subscribers, messages = 8, 100
	var wg sync.WaitGroup
	counts := make([]int, subscribers)
	ready := make(chan struct{}, subscribers)
	for i := range subscribers {
		ch, cancel, err := m.Subscribe(ctx, "load")
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cancel()
			ready <- struct{}{}
			for range ch {
				counts[i]++
				if counts[i] == messages {
					return
				}
			}
		}()
	}
	for range subscribers {
		<-ready
	}
	// Churn subscriptions while publishing.
	churn := make(chan struct{})
	go func() {
		defer close(churn)
		for i := range 50 {
			ch, cancel, err := m.Subscribe(ctx, fmt.Sprintf("load-%d", i%3))
			if err == nil {
				cancel()
				for range ch {
				}
			}
		}
	}()
	var pub sync.WaitGroup
	for p := range 4 {
		pub.Add(1)
		go func() {
			defer pub.Done()
			for j := range messages / 4 {
				if err := m.Publish(ctx, "load", []byte(fmt.Sprint(p, j))); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	pub.Wait()
	wg.Wait()
	<-churn
	for i, c := range counts {
		if c != messages {
			t.Errorf("subscriber %d got %d messages, want %d", i, c, messages)
		}
	}
}

func TestListen(t *testing.T) {
	m := NewMemory(0)
	defer m.Close(context.Background())
	got := make(chan string, 1)
	stop, err := Listen(context.Background(), m, "refresh", func(_ context.Context, n types.Notification) {
		got <- string(n.Payload)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Publish(context.Background(), "refresh", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-got:
		if v != "v2" {
			t.Fatalf("got %q", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listener not called")
	}
	stop()
	stop()
	if m.hub.Subscribed("refresh") {
		t.Fatal("listener still subscribed after stop")
	}
}
