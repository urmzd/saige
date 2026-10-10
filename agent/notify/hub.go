// Package notify provides an in-memory types.Notifier, the subscriber fan-out
// that backend notifiers share, and helpers that turn notifications into
// cache invalidation and refresh triggers.
//
// A notification is a hint, not a durable message. A subscriber checks
// durable state after it subscribes and again after each notification, so a
// message missed during a reconnect delays work instead of losing it.
package notify

import (
	"context"
	"sync"

	"github.com/urmzd/saige/agent/types"
)

// DefaultBuffer is the per-subscriber channel capacity Hub uses when none is
// set.
const DefaultBuffer = 64

// Hub fans notifications out to local subscribers. Delivery is lossless:
// Deliver waits for each subscriber to accept the message, for that
// subscription to end, or for the delivery context to end. A subscriber that
// stops reading therefore slows every publisher on its channel once its
// buffer is full; cancel subscriptions that are no longer read.
//
// The zero value is ready to use. It is safe for concurrent use.
type Hub struct {
	// Buffer is the per-subscriber channel capacity. Zero uses DefaultBuffer.
	Buffer int

	mu     sync.Mutex
	subs   map[string]map[*subscriber]struct{}
	closed bool
}

type subscriber struct {
	out  chan types.Notification
	done chan struct{}
	// send serializes deliveries with the close of out, so a send never
	// races a close.
	send   sync.Mutex
	once   sync.Once
	cancel func()
}

// Subscribe registers a subscriber on channel. onCancel, when non-nil, runs
// once after the subscriber is removed, whether by the returned cancel, by
// ctx, or by Close. The channel name is not validated here; notifiers
// validate before calling.
func (h *Hub) Subscribe(ctx context.Context, channel string, onCancel func()) (<-chan types.Notification, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	buffer := h.Buffer
	if buffer <= 0 {
		buffer = DefaultBuffer
	}
	s := &subscriber{out: make(chan types.Notification, buffer), done: make(chan struct{})}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, nil, types.ErrNotifierClosed
	}
	if h.subs == nil {
		h.subs = map[string]map[*subscriber]struct{}{}
	}
	if h.subs[channel] == nil {
		h.subs[channel] = map[*subscriber]struct{}{}
	}
	h.subs[channel][s] = struct{}{}
	// The lock stays held until s.cancel is set, so Close never sees a
	// subscriber without one.

	var (
		stopMu sync.Mutex
		stop   func() bool
	)
	cancel := func() {
		s.once.Do(func() {
			close(s.done)
			h.mu.Lock()
			if set := h.subs[channel]; set != nil {
				delete(set, s)
				if len(set) == 0 {
					delete(h.subs, channel)
				}
			}
			h.mu.Unlock()
			s.send.Lock()
			close(s.out)
			s.send.Unlock()
			stopMu.Lock()
			if stop != nil {
				stop()
			}
			stopMu.Unlock()
			if onCancel != nil {
				onCancel()
			}
		})
	}
	s.cancel = cancel
	h.mu.Unlock()
	stopMu.Lock()
	select {
	case <-s.done: // Close already ended the subscription.
	default:
		stop = context.AfterFunc(ctx, cancel)
	}
	stopMu.Unlock()
	return s.out, cancel, nil
}

// Deliver sends payload to every current subscriber of channel. Each
// subscriber receives its own copy. It returns ctx's error if ctx ends
// before every subscriber accepted the message.
func (h *Hub) Deliver(ctx context.Context, channel string, payload []byte) error {
	h.mu.Lock()
	targets := make([]*subscriber, 0, len(h.subs[channel]))
	for s := range h.subs[channel] {
		targets = append(targets, s)
	}
	h.mu.Unlock()

	for _, s := range targets {
		if err := s.deliver(ctx, types.Notification{Channel: channel, Payload: append([]byte(nil), payload...)}); err != nil {
			return err
		}
	}
	return nil
}

func (s *subscriber) deliver(ctx context.Context, n types.Notification) error {
	s.send.Lock()
	defer s.send.Unlock()
	select {
	case <-s.done:
		return nil
	default:
	}
	select {
	case s.out <- n:
		return nil
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Channels returns the channels that have at least one subscriber.
func (h *Hub) Channels() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.subs))
	for c := range h.subs {
		out = append(out, c)
	}
	return out
}

// Subscribed reports whether channel has at least one subscriber.
func (h *Hub) Subscribed(channel string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[channel]) > 0
}

// Close ends every subscription and rejects later ones. It is idempotent
// and always returns nil.
func (h *Hub) Close(context.Context) error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	var cancels []func()
	for _, set := range h.subs {
		for s := range set {
			cancels = append(cancels, s.cancel)
		}
	}
	h.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	return nil
}
