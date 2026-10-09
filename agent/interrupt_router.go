package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// maxExpiredInterrupts bounds how many expired interrupt IDs a router keeps
// so a late reply reports ErrInterruptExpired instead of ErrInterruptNotFound.
const maxExpiredInterrupts = 1024

// maxAnsweredInterrupts bounds how many answered and withdrawn interrupts a
// router keeps so a retried reply stays idempotent after its waiter moved on.
const maxAnsweredInterrupts = 1024

// MemoryInterruptRouter is the in-process types.InterruptRouter. Every local
// EventStream owns one: each MarkerDelta posts an interrupt to it, and
// ResolveMarker and EventStream.ReplyInterrupt answer through it. A parent
// run posts a copy of each child interrupt it forwards, so the root stream's
// router lists every interrupt raised anywhere in the run.
//
// An interrupt with an ExpiresAt is withdrawn at that time: its reply
// channel closes without a value and the waiting run applies the
// interrupt's Policy. It is safe for concurrent use.
type MemoryInterruptRouter struct {
	mu      sync.Mutex
	pending map[string]*memoryInterrupt
	expired map[string]struct{}
	// answered maps a withdrawn, answered interrupt's ID to the idempotency
	// key of its reply.
	answered map[string]string
}

type memoryInterrupt struct {
	in       types.Interrupt
	reply    chan types.InterruptReply // buffered; receives at most one reply
	answered bool
	key      string // idempotency key of the delivered reply
	timer    *time.Timer
}

// NewMemoryInterruptRouter returns an empty router.
func NewMemoryInterruptRouter() *MemoryInterruptRouter {
	return &MemoryInterruptRouter{pending: map[string]*memoryInterrupt{}, expired: map[string]struct{}{}, answered: map[string]string{}}
}

var _ types.InterruptRouter = (*MemoryInterruptRouter)(nil)

// Post registers in and returns the channel that receives its reply. The
// channel closes without a value when the interrupt expires. Posting an ID
// that is already pending fails, and so does an interrupt that has already
// expired.
func (r *MemoryInterruptRouter) Post(_ context.Context, in types.Interrupt) (<-chan types.InterruptReply, error) {
	if in.ID == "" {
		return nil, errors.New("interrupt needs an ID")
	}
	now := time.Now()
	if in.Expired(now) {
		return nil, fmt.Errorf("%w: %s", types.ErrInterruptExpired, in.ID)
	}
	if in.CreatedAt.IsZero() {
		in.CreatedAt = now.UTC()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.pending[in.ID]; ok {
		return nil, fmt.Errorf("interrupt %s is already pending", in.ID)
	}
	delete(r.expired, in.ID)
	delete(r.answered, in.ID)
	p := &memoryInterrupt{in: in, reply: make(chan types.InterruptReply, 1)}
	if !in.ExpiresAt.IsZero() {
		id := in.ID
		p.timer = time.AfterFunc(in.ExpiresAt.Sub(now), func() { r.expire(id, p) })
	}
	r.pending[in.ID] = p
	return p.reply, nil
}

// expire closes an unanswered interrupt's reply channel.
func (r *MemoryInterruptRouter) expire(id string, p *memoryInterrupt) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending[id] != p || p.answered {
		return
	}
	delete(r.pending, id)
	r.rememberExpiredLocked(id)
	close(p.reply)
}

func (r *MemoryInterruptRouter) rememberExpiredLocked(id string) {
	if len(r.expired) >= maxExpiredInterrupts {
		clear(r.expired)
	}
	r.expired[id] = struct{}{}
}

// Reply delivers a decision to a pending interrupt. Delivering the same
// reply again with the same non-empty IdempotencyKey returns nil and has no
// effect, also after the run consumed the first reply. A second reply with
// another key, or with none, returns ErrMarkerResolved. An unknown ID returns ErrInterruptNotFound, and a reply
// after the deadline returns ErrInterruptExpired.
func (r *MemoryInterruptRouter) Reply(_ context.Context, reply types.InterruptReply) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.pending[reply.ID]
	if !ok {
		if key, done := r.answered[reply.ID]; done {
			if reply.IdempotencyKey != "" && reply.IdempotencyKey == key {
				return nil
			}
			return fmt.Errorf("%w: %s", ErrMarkerResolved, reply.ID)
		}
		if _, gone := r.expired[reply.ID]; gone {
			return fmt.Errorf("%w: %s", types.ErrInterruptExpired, reply.ID)
		}
		return fmt.Errorf("%w: %s", types.ErrInterruptNotFound, reply.ID)
	}
	if p.answered {
		if reply.IdempotencyKey != "" && reply.IdempotencyKey == p.key {
			return nil
		}
		return fmt.Errorf("%w: %s", ErrMarkerResolved, reply.ID)
	}
	if p.in.Expired(time.Now()) {
		delete(r.pending, reply.ID)
		r.rememberExpiredLocked(reply.ID)
		if p.timer != nil {
			p.timer.Stop()
		}
		close(p.reply)
		return fmt.Errorf("%w: %s", types.ErrInterruptExpired, reply.ID)
	}
	if p.timer != nil {
		p.timer.Stop()
	}
	p.answered = true
	p.key = reply.IdempotencyKey
	p.reply <- reply
	return nil
}

// Pending lists the unanswered interrupts of runID, oldest first. An empty
// runID lists every unanswered interrupt the router holds.
func (r *MemoryInterruptRouter) Pending(_ context.Context, runID string) ([]types.Interrupt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []types.Interrupt
	for _, p := range r.pending {
		if p.answered || (runID != "" && p.in.RunID != runID) {
			continue
		}
		in := p.in
		in.Path = append([]string(nil), in.Path...)
		in.Markers = append([]types.Marker(nil), in.Markers...)
		in.Payload = append([]byte(nil), in.Payload...)
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// withdraw removes an interrupt whose waiter stopped waiting, answered or
// not, and reports whether it had been answered. A later reply to an
// answered interrupt is handled as a retry of the first reply. A later
// reply to an unanswered one returns ErrInterruptNotFound, or
// ErrInterruptExpired when the interrupt expired first.
func (r *MemoryInterruptRouter) withdraw(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.pending[id]
	if !ok {
		_, answered := r.answered[id]
		return answered
	}
	if p.timer != nil {
		p.timer.Stop()
	}
	delete(r.pending, id)
	if !p.answered {
		return false
	}
	if len(r.answered) >= maxAnsweredInterrupts {
		clear(r.answered)
	}
	r.answered[id] = p.key
	return true
}
