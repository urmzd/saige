package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// SignalChannel is the notifier channel an Engine with a Notifier announces
// state changes on.
const SignalChannel = "saige.durable.local"

// Signal kinds.
const (
	SignalReply  = "reply"  // a reply or decision was recorded
	SignalInput  = "input"  // Append added an input segment
	SignalCancel = "cancel" // Cancel closed the run
	SignalRun    = "run"    // Run saved its final status
)

// Signal is the payload published on SignalChannel. It names what changed,
// not the change itself: a receiver reads the run with Inspect.
type Signal struct {
	RunID string `json:"run_id"`
	Kind  string `json:"kind"`
	// ID is the interrupt ID for SignalReply and the run status for
	// SignalRun.
	ID string `json:"id,omitempty"`
}

// ErrNoNotifier reports that Await was called on an engine without a
// Notifier.
var ErrNoNotifier = errors.New("durable engine has no notifier")

// ErrSignal reports that a state change was saved but its announcement
// failed. The change stands; only waiters in other processes miss the
// wake-up until the next signal for the run.
var ErrSignal = errors.New("durable change saved but not announced")

// signalTimeout bounds a publish from a method that takes no context.
const signalTimeout = 5 * time.Second

// signal publishes sig when n is set. A nil notifier is a no-op, so an engine
// without one behaves as before.
func signal(ctx context.Context, n types.Notifier, sig Signal) error {
	if n == nil {
		return nil
	}
	raw, err := json.Marshal(sig)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSignal, err)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), signalTimeout)
	defer cancel()
	if err := n.Publish(ctx, SignalChannel, raw); err != nil {
		return fmt.Errorf("%w: %w", ErrSignal, err)
	}
	return nil
}

// Await blocks until ready reports true for the run's state, re-reading the
// state each time a signal for the run arrives. It checks once after
// subscribing, so a change made before the call is not missed. A run that
// does not exist yet is waited for. It needs Engine.Notifier; every process
// that changes the run must use a notifier that reaches this one.
//
// A worker that serves input from other processes waits with Resumable:
//
//	for {
//		if _, err := engine.Await(ctx, id, local.Resumable); err != nil {
//			return err
//		}
//		_, err := engine.Run(ctx, id, revision, factory, nil)
//		// handle err; ErrSuspended waits for the next reply
//	}
func (e *Engine) Await(ctx context.Context, id string, ready func(State) bool) (State, error) {
	if e.Notifier == nil {
		return State{}, ErrNoNotifier
	}
	if ready == nil {
		return State{}, errors.New("await requires a ready function")
	}
	ch, cancel, err := e.Notifier.Subscribe(ctx, SignalChannel)
	if err != nil {
		return State{}, err
	}
	defer cancel()
	for {
		s, err := e.Inspect(id)
		switch {
		case err == nil && ready(s):
			return s, nil
		case err != nil && !errors.Is(err, os.ErrNotExist):
			return State{}, err
		}
		if err := waitFor(ctx, ch, id); err != nil {
			return State{}, err
		}
	}
}

// waitFor returns when a signal for runID arrives.
func waitFor(ctx context.Context, ch <-chan types.Notification, runID string) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-ch:
			if !ok {
				if err := ctx.Err(); err != nil {
					return err
				}
				return types.ErrNotifierClosed
			}
			var sig Signal
			if json.Unmarshal(msg.Payload, &sig) == nil && sig.RunID == runID {
				return nil
			}
		}
	}
}

// Resumable reports whether Run has work to do on s: the run is new or has
// appended input after completing, or it is suspended and received a reply
// or input since it last suspended.
func Resumable(s State) bool {
	switch s.Status {
	case statusReady:
		return true
	case statusSuspended:
		suspended := 0
		for _, ev := range s.History {
			if ev.Kind == "run."+statusSuspended {
				suspended = ev.Sequence
			}
		}
		for _, ev := range s.History {
			if ev.Sequence <= suspended {
				continue
			}
			switch ev.Kind {
			case "approval.decided", "input.appended", "interrupt.expired":
				return true
			}
		}
	}
	return false
}
