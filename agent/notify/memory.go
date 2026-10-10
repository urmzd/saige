package notify

import (
	"context"

	"github.com/urmzd/saige/agent/types"
)

// Memory is an in-process types.Notifier. Every subscriber of a channel
// receives every message published to it after it subscribed. It reaches
// only subscribers in the same process; use postgres.Notifier across
// processes.
type Memory struct {
	hub Hub
}

var _ types.Notifier = (*Memory)(nil)

// NewMemory returns an in-memory notifier. buffer is the per-subscriber
// channel capacity; zero uses DefaultBuffer.
func NewMemory(buffer int) *Memory {
	return &Memory{hub: Hub{Buffer: buffer}}
}

// Publish delivers payload to every current subscriber of channel. It waits
// for slow subscribers until ctx ends; see Hub.
func (m *Memory) Publish(ctx context.Context, channel string, payload []byte) error {
	if err := types.ValidateChannel(channel); err != nil {
		return err
	}
	m.hub.mu.Lock()
	closed := m.hub.closed
	m.hub.mu.Unlock()
	if closed {
		return types.ErrNotifierClosed
	}
	return m.hub.Deliver(ctx, channel, payload)
}

// Subscribe implements types.Notifier.
func (m *Memory) Subscribe(ctx context.Context, channel string) (<-chan types.Notification, func(), error) {
	if err := types.ValidateChannel(channel); err != nil {
		return nil, nil, err
	}
	return m.hub.Subscribe(ctx, channel, nil)
}

// Close ends every subscription. Later calls return types.ErrNotifierClosed.
func (m *Memory) Close(ctx context.Context) error {
	return m.hub.Close(ctx)
}
