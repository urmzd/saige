// Package deliverystate keeps a sink's native message handle and accumulated
// rendering together across replica changes. It owns storage, not rendering or
// external API calls. Callers choose the backend and a stable connector scope.
package deliverystate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Key isolates a run's message by tenant, connector instance and destination.
type Key struct{ Tenant, Scope, Run, Channel, Target string }

func (k Key) id() (string, error) {
	if k.Tenant == "" || k.Scope == "" || k.Run == "" || k.Channel == "" || k.Target == "" {
		return "", fmt.Errorf("deliverystate: incomplete key")
	}
	b, err := json.Marshal(k)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// Store serializes an update for one Key, including the callback's surface
// write. The callback sees the latest committed bytes, nil for a new message.
// A failed callback leaves the old state intact. Success is returned only once
// the new bytes and callback's pending flag are committed. Callbacks must not
// recursively call Update.
//
// External API writes and storage commits are not one transaction. A crash
// after Create succeeds but before commit can still produce a duplicate unless
// the surface itself supports recovery or idempotent creation.
type Store interface {
	Update(context.Context, Key, func([]byte) ([]byte, bool, error)) error
	// Pending returns at most limit dirty keys in this connector scope, oldest
	// update first. It lets a new replica flush acknowledged coalesced events.
	Pending(context.Context, string, int) ([]Key, error)
}

type entry struct {
	token   chan struct{}
	data    []byte
	key     Key
	pending bool
	at      time.Time
}

// Memory is a complete Store shared by connectors within one process.
type Memory struct {
	mu      sync.Mutex
	entries map[string]*entry
}

// NewMemory returns an empty store. Records live as long as the store.
func NewMemory() *Memory { return &Memory{entries: make(map[string]*entry)} }

// Update implements Store. Different keys never wait for each other's API call.
func (m *Memory) Update(ctx context.Context, k Key, update func([]byte) ([]byte, bool, error)) error {
	id, err := k.id()
	if err != nil {
		return err
	}
	m.mu.Lock()
	e := m.entries[id]
	if e == nil {
		e = &entry{token: make(chan struct{}, 1), key: k}
		e.token <- struct{}{}
		m.entries[id] = e
	}
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-e.token:
	}
	defer func() { e.token <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return err
	}
	data, pending, err := update(append([]byte(nil), e.data...))
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	e.data = append([]byte(nil), data...)
	e.pending, e.at = pending, time.Now()
	return nil
}

// Pending implements Store. The result is bounded even for a large backlog.
func (m *Memory) Pending(ctx context.Context, scope string, limit int) ([]Key, error) {
	if limit <= 0 || limit > 256 {
		return nil, fmt.Errorf("deliverystate: pending limit must be 1..256")
	}
	m.mu.Lock()
	entries := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		if e.key.Scope == scope {
			entries = append(entries, e)
		}
	}
	m.mu.Unlock()
	type candidate struct {
		key Key
		at  time.Time
	}
	var found []candidate
	for _, e := range entries {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-e.token:
		}
		if e.pending {
			found = append(found, candidate{e.key, e.at})
		}
		e.token <- struct{}{}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].at.Before(found[j].at) })
	if len(found) > limit {
		found = found[:limit]
	}
	out := make([]Key, 0, len(found))
	for _, c := range found {
		out = append(out, c.key)
	}
	return out, nil
}
