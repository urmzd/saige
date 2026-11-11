package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/urmzd/saige/flow/bus"
	"github.com/urmzd/saige/flow/event"
	turnrt "github.com/urmzd/saige/flow/turn"
	"github.com/urmzd/saige/flow/turn/native"
)

// Compacting a run on request.
//
// A host watching a long turn (a console with a "compact" button, an operator
// who sees the context window filling) can ask the run to summarize the older
// part of its history now, rather than wait for the deployment's budget. The
// turn compacts before its next model call, exactly as the budget would have
// made it, and publishes context.compacted with Trigger "requested" and who
// asked. The deployment has to take requests: WithCompactRequests on the
// runtime and native.Compaction.OnRequest on its agents, both of which
// flow.WithCompaction sets.
//
// # How the request reaches the turn
//
// The way a stop does, minus the ledger. It is published to a control stream
// of its own (CompactChannel), keyed by the asker's tenant and the run, and the
// attempt executing the run listens there for as long as its turn lasts. Only
// a request made under the run's own tenant is heard.
//
// # A request is live, and only live
//
// A stop is recorded on the run because it decides the run's outcome, and a
// request that waited for the next attempt would still be the right thing to
// obey. A compaction decides nothing about the outcome, and a request that
// waited would be wrong: a turn continued after a park, or retried after a
// crash, would summarize a history the person never saw grow. So a request is
// honoured only by the attempt that was listening when it was made, judged by
// its timestamp against the moment the attempt began to listen. One made while
// the run is queued, parked, between attempts or over does nothing, and one
// made after the turn's last model call does nothing either. Timestamps come
// from the asking process's clock, so a request made within a clock skew of an
// attempt starting may be dropped; ask again.
//
// It is not durable and not acknowledged. Compact returns once the request is
// published; the run's stream says whether the turn compacted.

// compactVerb names the compaction stream within a run's control topics.
const compactVerb = "compact"

// ErrCompactUnavailable reports a runtime that does not carry requests to
// compact: it was built without WithCompactRequests, or its bus can be
// published to and not subscribed to, so no turn it executes could hear one.
var ErrCompactUnavailable = errors.New("runtime: compaction on request is not available")

// WithCompactRequests makes every turn listen for requests to compact, and
// Compact publish them. Pass it with native.Compaction.OnRequest on the
// executor's agents: one without the other either listens for requests no
// turn obeys, or obeys requests no turn hears.
//
// Listening costs one bus subscription per running turn, beside the one for
// stop requests. A runtime without the option holds none and Compact refuses
// with ErrCompactUnavailable.
func WithCompactRequests() Option {
	return func(r *Runtime) { r.compactRequests = true }
}

// CompactRequest asks one run to compact its history.
type CompactRequest struct {
	// Tenant is the scope the request is made under, from the caller's
	// credential. Only a run of the same tenant hears it.
	Tenant string
	// RunID is the run, as Acceptance.RunID reported it.
	RunID string
	// By is who asked. It is published on context.compacted. Optional.
	By string
}

// CompactChannel is where a tenant's requests to compact a run are
// published: control://<tenant>/compact/<key>, the key a hash of the tenant
// and the run id, for the reasons StopChannel gives.
func CompactChannel(tenant, runID string) (string, error) {
	return controlChannel(compactVerb, tenant, runID)
}

// Compact asks a run to compact its history before its next model call.
//
// It publishes the request and returns. Like Stop, it is the same call with
// the same nil result for a run that does not exist, belongs to another
// tenant, is not executing a turn, or has ended, so its answer never says
// whether a run exists. What happened is on the run's stream: a
// context.compacted event if the turn compacted.
//
// It returns ErrInvalid for a request that names no run and
// ErrCompactUnavailable when this runtime does not take requests
// (WithCompactRequests) or its bus cannot be subscribed to.
func (r *Runtime) Compact(ctx context.Context, req CompactRequest) error {
	if !r.compactRequests {
		return fmt.Errorf("%w: this runtime was built without WithCompactRequests", ErrCompactUnavailable)
	}
	if r.control == nil {
		return fmt.Errorf("%w: the bus this runtime was built with cannot be subscribed to", ErrCompactUnavailable)
	}
	req.RunID = strings.TrimSpace(req.RunID)
	ch, err := CompactChannel(req.Tenant, req.RunID)
	if err != nil {
		return fmt.Errorf("%w: compact: %w", ErrInvalid, err)
	}
	at := r.now().UTC()
	e := event.Envelope{
		ID:      requestID(compactVerb, req.RunID, at),
		Source:  "fiber://control",
		Type:    event.TypeCompact,
		Channel: ch,
		Time:    at,
		Tenant:  busTenant(req.Tenant),
		Run:     req.RunID,
		Data:    event.CompactRequest{RunID: req.RunID, By: clip(req.By), At: at},
	}
	if err := r.bus.Publish(ctx, e); err != nil {
		return fmt.Errorf("runtime: compact %s: %w", req.RunID, err)
	}
	return nil
}

// CompactSignal returns the channel on which the turn executing under ctx is
// asked to compact, or nil when it cannot be. A turn runtime hands it to the
// agent loop as native.Turn.Compact; one that does not simply never
// compacts on request.
func CompactSignal(ctx context.Context) <-chan native.CompactRequest {
	return turnrt.CompactSignal(ctx)
}

// listenCompact puts a CompactSignal on ctx and relays to it the requests to
// compact this run made from now until release is called.
func (r *Runtime) listenCompact(ctx context.Context, in Input, runID string) (context.Context, func()) {
	if !r.compactRequests || r.control == nil {
		return ctx, func() {}
	}
	ch, err := CompactChannel(in.Trigger.Tenant, runID)
	if err != nil {
		r.logger.Error("flow/runtime: compact channel is invalid", "run_id", runID, "error", err)
		return ctx, func() {}
	}
	// One slot: a request that finds it full folds into the one waiting,
	// which is what the turn would do with it anyway.
	signal := make(chan native.CompactRequest, 1)
	since := r.now().UTC()
	listenCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		sub := bus.Subscription{Tenant: busTenant(in.Trigger.Tenant), Channels: []string{ch}, Types: []string{event.TypeCompact}}
		// A resubscription reads the retained requests again, and the ids
		// already relayed are skipped: each request would cost a summary.
		seen := map[string]bool{}
		for listenCtx.Err() == nil {
			err := r.relayCompact(listenCtx, sub, seen, runID, since, signal)
			if listenCtx.Err() != nil {
				return
			}
			r.logger.Warn("flow/runtime: lost the compact subscription; listening again",
				"run_id", runID, "error", err)
			select {
			case <-listenCtx.Done():
				return
			case <-time.After(stopRetry):
			}
		}
	}()
	return turnrt.WithCompactSignal(ctx, signal), func() {
		cancel()
		<-done
	}
}

// relayCompact reads one subscription and relays every request this attempt
// should honour, until the subscription ends. It records every request it
// reads in seen, so the caller resubscribes without relaying one twice.
func (r *Runtime) relayCompact(ctx context.Context, sub bus.Subscription, seen map[string]bool, runID string, since time.Time, signal chan<- native.CompactRequest) error {
	stream, err := r.control.Subscribe(ctx, sub)
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	for {
		e, err := stream.Recv(ctx)
		if err != nil {
			return err
		}
		if seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		body, ok := e.Data.(event.CompactRequest)
		if !ok || e.Tenant != sub.Tenant || body.RunID != runID || body.At.Before(since) {
			// Not a request this run's topic should carry, or one made
			// before this attempt was listening (see "A request is live").
			continue
		}
		select {
		case signal <- native.CompactRequest{By: body.By}:
		default:
		}
	}
}
