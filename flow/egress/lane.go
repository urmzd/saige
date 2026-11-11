package egress

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
	"github.com/urmzd/saige/flow/event"
)

// lane carries one run's events to one target, in order.
type lane struct {
	e      *Engine
	key    laneKey
	sink   connector.Sink
	target address.Address
	acks   *acks
	queue  chan item
}

// item is one event in a lane, with its position in the tenant's order.
type item struct {
	ev  event.Envelope
	pos uint64
}

// offer queues it. It never blocks: a lane that is this far behind is a sink
// that is not keeping up, and blocking here would stall every other run on
// the binding. A dropped delivery is finished: it is logged, and waiting on
// it would hold the cursor forever. Called with the engine lock held.
func (l *lane) offer(it item) {
	select {
	case l.queue <- it:
	default:
		l.e.log.Error("egress: delivery dropped, sink is not keeping up",
			l.attrs(it.ev, "queue", cap(l.queue))...)
		l.acks.finish(it.pos)
	}
}

// serve delivers until the run ends, the lane is idle, or ctx is done. It
// leaves the engine's lane table only with an empty queue, under the engine
// lock, so no event offered to it is lost.
func (l *lane) serve(ctx context.Context) {
	idle := time.NewTimer(l.e.idle)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			l.abandon()
			return
		case it := <-l.queue:
			if !l.deliver(ctx, it.ev) {
				// Abandoned at shutdown: left uncommitted, for whoever
				// delivers next.
				continue
			}
			l.acks.finish(it.pos)
			if terminal(it.ev.Type) && l.leave() {
				return
			}
			idle.Reset(l.e.idle)
		case <-idle.C:
			if l.leave() {
				return
			}
			idle.Reset(l.e.idle)
		}
	}
}

// leave removes the lane from the engine when its queue is empty.
func (l *lane) leave() bool {
	l.e.mu.Lock()
	defer l.e.mu.Unlock()
	if len(l.queue) > 0 {
		return false
	}
	if l.e.lanes[l.key] == l {
		delete(l.e.lanes, l.key)
	}
	return true
}

// abandon logs what is left at shutdown and leaves the engine.
func (l *lane) abandon() {
	l.e.mu.Lock()
	if l.e.lanes[l.key] == l {
		delete(l.e.lanes, l.key)
	}
	n := len(l.queue)
	l.e.mu.Unlock()
	if n > 0 {
		l.e.log.Warn("egress: undelivered events abandoned at shutdown",
			"run", l.key.order, "target", l.target.String(), "connector", l.target.Connector, "pending", n)
	}
}

// deliver offers e to the sink until it succeeds, fails permanently, or the
// attempts run out, and reports whether it is finished with e. It is not
// when ctx ended first.
func (l *lane) deliver(ctx context.Context, e event.Envelope) bool {
	for attempt := 1; ; attempt++ {
		err := l.attempt(ctx, e)
		switch {
		case err == nil:
			if attempt > 1 {
				l.e.log.Info("egress: delivery succeeded after retrying", l.attrs(e, "attempts", attempt)...)
			}
			return true
		case ctx.Err() != nil:
			l.e.log.Warn("egress: delivery abandoned at shutdown", l.attrs(e, "attempts", attempt, "error", err)...)
			return false
		case permanent(err):
			l.retire(e, err, attempt, "permanent")
			return true
		case attempt >= l.e.attempts:
			l.retire(e, err, attempt, "attempts exhausted")
			return true
		}
		wait := l.e.backoffFor(attempt)
		l.e.log.Warn("egress: delivery failed, retrying",
			l.attrs(e, "attempt", attempt, "retry_in", wait.String(), "error", err)...)
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
		}
	}
}

// attempt makes one sink call, bounded by the timeout and the concurrency
// limit, with a panic turned into ErrPanic.
func (l *lane) attempt(ctx context.Context, e event.Envelope) (err error) {
	if err := l.e.acquire(ctx); err != nil {
		return err
	}
	defer l.e.release()

	call := ctx
	if l.e.timeout > 0 {
		var cancel context.CancelFunc
		call, cancel = context.WithTimeout(ctx, l.e.timeout)
		defer cancel()
	}
	defer func() {
		if v := recover(); v != nil {
			err = &panicError{value: v, stack: debug.Stack()}
		}
	}()
	return l.sink.Deliver(call, l.target, e)
}

func (l *lane) retire(e event.Envelope, err error, attempts int, reason string) {
	attrs := l.attrs(e, "attempts", attempts, "reason", reason, "error", err)
	var p *panicError
	if errors.As(err, &p) {
		attrs = append(attrs, "stack", string(p.stack))
	}
	l.e.log.Error("egress: delivery failed, giving up", attrs...)
}

func (l *lane) attrs(e event.Envelope, extra ...any) []any {
	attrs := []any{
		"run", e.Run,
		"agent", e.Agent,
		"version", e.Version,
		"seq", e.Seq,
		"type", e.Type,
		"target", l.target.String(),
		"connector", l.target.Connector,
	}
	return append(attrs, extra...)
}

// permanent reports a failure that no retry can fix.
func permanent(err error) bool {
	for _, sentinel := range []error{
		connector.ErrUndeliverable, // not a surface this connector can reach
		connector.ErrUnsupported,   // this connector cannot receive at all
		connector.ErrNotFound,      // no such connector in this deployment
		ErrPanic,                   // a defect in the sink, not congestion
		event.ErrInvalid,           // unrenderable, and the event will not change
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

type panicError struct {
	value any
	stack []byte
}

func (p *panicError) Error() string { return fmt.Sprintf("egress: sink panicked: %v", p.value) }

func (p *panicError) Unwrap() error { return ErrPanic }
