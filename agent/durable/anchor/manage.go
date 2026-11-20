package anchor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/urmzd/saige/durable/ledger"
	"github.com/urmzd/saige/durable/queue"
	"github.com/urmzd/saige/durable/run"
	"github.com/urmzd/saige/durable/worker"

	"github.com/urmzd/saige/agent/types"
)

var (
	// ErrBusy reports a run another worker executes now.
	ErrBusy = errors.New("durable run already has a worker")
	// ErrNoNotifier reports that Await was called on an engine without a
	// Notifier.
	ErrNoNotifier = errors.New("durable engine has no notifier")
	// ErrSignal reports that a change was recorded but its announcement
	// failed. The change stands; only waiters in other processes miss the
	// wake-up until the next signal for the run.
	ErrSignal = errors.New("durable change saved but not announced")
	// ErrUnsupported reports an operation the engine's ledger or queue
	// lacks the capability for.
	ErrUnsupported = errors.New("durable operation unsupported by this ledger or queue")
)

// SignalChannel is the notifier channel an engine with a Notifier announces
// changes on.
const SignalChannel = "saige.durable"

// Signal kinds.
const (
	SignalReply  = "reply"  // a reply or decision was recorded
	SignalInput  = "input"  // Append added an input segment
	SignalCancel = "cancel" // Cancel ended the run
	SignalRun    = "run"    // RunHere saw the run's outcome
)

// Signal is the payload published on SignalChannel. It names what changed,
// not the change: a receiver reads the run with Inspect.
type Signal struct {
	RunID string `json:"run_id"`
	Kind  string `json:"kind"`
	// ID is the interrupt ID for SignalReply, the append key for
	// SignalInput and the run's status for SignalRun.
	ID string `json:"id,omitempty"`
}

// signalTimeout bounds a publish that outlives its caller's context.
const signalTimeout = 5 * time.Second

// signal publishes sig when the engine has a Notifier.
func (e *Engine) signal(ctx context.Context, sig Signal) error {
	if e.Notifier == nil {
		return nil
	}
	raw, err := json.Marshal(sig)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSignal, err)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), signalTimeout)
	defer cancel()
	if err := e.Notifier.Publish(ctx, SignalChannel, raw); err != nil {
		return fmt.Errorf("%w: %w", ErrSignal, err)
	}
	return nil
}

// RunHere starts the run (see Start) and executes it in this process now,
// returning when the attempt has an outcome: the final message of a
// completed run, or of an open run that answered all its input;
// types.ErrSuspended while an interrupt waits for a reply; ErrIndeterminate
// while a step waits for Reconcile; ErrFailed for a failed run and ErrClosed
// for a cancelled one; ErrBusy when another worker executes the run now. An
// attempt that failed and was released for another returns its error. The
// queue must support targeted claims (queue.Targeted).
func (e *Engine) RunHere(ctx context.Context, w *Workflow, id string, input []types.Message, opts ...worker.Option) (*types.AssistantMessage, error) {
	if err := e.Start(ctx, w, id, input); err != nil {
		return nil, err
	}
	if err := e.Worker(opts...).RunOne(ctx, id); err != nil {
		switch {
		case errors.Is(err, queue.ErrLeased):
			return nil, ErrBusy
		case errors.Is(err, queue.ErrNotQueued):
			// Nothing to execute: report what the run is.
		default:
			return nil, err
		}
	}
	final, err := e.outcomeOf(ctx, id)
	status := "pending"
	if s, ierr := e.Inspect(ctx, id); ierr == nil {
		status = string(s.Status)
		if s.Idle {
			status = "idle"
		}
	}
	if serr := e.signal(ctx, Signal{RunID: id, Kind: SignalRun, ID: status}); serr != nil {
		return final, errors.Join(err, serr)
	}
	return final, err
}

// outcomeOf reports the run's outcome as RunHere does, without waiting.
func (e *Engine) outcomeOf(ctx context.Context, id string) (*types.AssistantMessage, error) {
	s, err := e.Inspect(ctx, id)
	if err != nil {
		return nil, err
	}
	switch {
	case s.Cancelled:
		return nil, ErrClosed
	case s.Status == run.StatusSucceeded, s.Idle:
		return s.Final, nil
	case s.Status == run.StatusFailed:
		return nil, fmt.Errorf("%w: %s", ErrFailed, s.Error)
	case len(s.Pending) > 0:
		return nil, types.ErrSuspended
	case len(s.Uncertain) > 0:
		return nil, fmt.Errorf("%w: %s: %s", ErrIndeterminate, s.Uncertain[0].Step, s.Uncertain[0].Error)
	}
	return nil, errors.New("durable run stopped without an outcome; run it again")
}

// Cancel ends a run before it finishes: it can never resume, and its
// pending interrupts close. reason is recorded after run.ErrCancelled. A
// finished run returns ErrClosed.
func (e *Engine) Cancel(ctx context.Context, id, reason string) error {
	if err := e.client.Cancel(ctx, id, reason); err != nil {
		if errors.Is(err, run.ErrAlreadyTerminal) {
			return ErrClosed
		}
		return err
	}
	return e.signal(ctx, Signal{RunID: id, Kind: SignalCancel})
}

// List returns the state of every run of the engine for which keep returns
// true (every run when keep is nil), ordered by run ID. The ledger must list
// its runs (ledger.RunLister).
func (e *Engine) List(ctx context.Context, keep func(State) bool) ([]State, error) {
	l, ok := e.lgr.(ledger.RunLister)
	if !ok {
		return nil, fmt.Errorf("%w: the ledger cannot list runs", ErrUnsupported)
	}
	runs, err := l.Runs(ctx)
	if err != nil {
		return nil, err
	}
	var out []State
	for _, r := range runs {
		s, err := e.Inspect(ctx, r.ID)
		if errors.Is(err, run.ErrNotFound) {
			continue // deleted since it was listed
		}
		if err != nil {
			return nil, err
		}
		if keep == nil || keep(s) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunID < out[j].RunID })
	return out, nil
}

// Leased reports whether a worker executes the run now. The queue must be
// able to tell (queue.LeaseProber). An unknown run is run.ErrNotFound.
func (e *Engine) Leased(ctx context.Context, id string) (bool, error) {
	if _, err := e.getRun(ctx, id); err != nil {
		return false, err
	}
	p, ok := e.q.(queue.LeaseProber)
	if !ok {
		return false, fmt.Errorf("%w: the queue cannot report leases", ErrUnsupported)
	}
	return p.Leased(ctx, id)
}

// Delete removes a finished or idle run and every record of it. A run that
// still has work (running, waiting on a reply, uncertain, or holding input
// it has not answered) returns ErrConflict, and one a worker executes now
// ErrBusy. The ledger must delete runs (ledger.Deleter).
func (e *Engine) Delete(ctx context.Context, id string) error {
	d, ok := e.lgr.(ledger.Deleter)
	if !ok {
		return fmt.Errorf("%w: the ledger cannot delete runs", ErrUnsupported)
	}
	s, err := e.Inspect(ctx, id)
	if err != nil {
		return err
	}
	if !s.Status.Terminal() && !s.Idle {
		return fmt.Errorf("%w: run %s still has work", ErrConflict, id)
	}
	if p, ok := e.q.(queue.LeaseProber); ok {
		if leased, err := p.Leased(ctx, id); err != nil {
			return err
		} else if leased {
			return ErrBusy
		}
	}
	if err := d.Delete(ctx, id); err != nil {
		return err
	}
	if r, ok := e.q.(interface{ Remove(string) error }); ok {
		return r.Remove(id)
	}
	return nil
}

// Await blocks until ready reports true for the run's state, reading the
// state again each time a signal for the run arrives. It checks once after
// subscribing, so a change made before the call is not missed, and waits
// for a run that does not exist yet. It needs the engine's Notifier; every
// process that changes the run must use a notifier that reaches this one.
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
		s, err := e.Inspect(ctx, id)
		switch {
		case err == nil && ready(s):
			return s, nil
		case err != nil && !errors.Is(err, run.ErrNotFound):
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

// Resumable reports whether the run has work a worker would do now: it has
// not finished, is not idle, and waits on no reply or reconciliation.
func Resumable(s State) bool {
	return !s.Status.Terminal() && !s.Idle && len(s.Pending) == 0 && len(s.Uncertain) == 0
}
