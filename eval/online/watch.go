package online

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/eval"
	"github.com/urmzd/saige/eval/store"
)

// Announce publishes ref on channel (DefaultChannel when empty), telling a
// [Sampler.Watch] that the run ending at ref finished. Call it after the
// run's final node is stored. The payload is the ref as JSON.
func Announce(ctx context.Context, n types.Notifier, channel string, ref Ref) error {
	if channel == "" {
		channel = DefaultChannel
	}
	payload, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	return n.Publish(ctx, channel, payload)
}

// WatchOptions configures [Sampler.Watch].
type WatchOptions struct {
	// Channel is the notifier channel; empty means DefaultChannel.
	Channel string
	// Since, when set, first sweeps runs that finished from Since until
	// the subscription started, into the same run. A notification is a
	// hint, not a queue: runs that finished while no watcher listened are
	// only found this way.
	Since time.Time
	// OnUnit is called after each unit is stored, from the watching
	// goroutine.
	OnUnit func(eval.Unit)
	// OnReady is called once the subscription is live and any catch-up
	// sweep is done.
	OnReady func(runID string)
}

// Watch scores runs as they finish until ctx ends. It records one run,
// created when Watch starts and finished when it returns, and adds a unit
// for every announced run that passes the filter and the sample. A ref that
// is not a finished run, or one already scored by this watch, is skipped.
// Watch returns nil when ctx ends; the run is then marked succeeded with its
// aggregate computed from the stored units.
func (s *Sampler) Watch(ctx context.Context, n types.Notifier, src Source, opts WatchOptions) (Report, error) {
	if s.Store == nil {
		return Report{}, errors.New("online: sampler has no store")
	}
	channel := opts.Channel
	if channel == "" {
		channel = DefaultChannel
	}
	ch, cancel, err := n.Subscribe(ctx, channel)
	if err != nil {
		return Report{}, err
	}
	defer cancel()

	started := time.Now().UTC()
	run := eval.RunRecord{
		ID: eval.NewRunID(), Suite: s.suite(), Status: eval.RunRunning,
		StartedAt: started, Labels: s.runLabels(), Provenance: s.Provenance,
	}
	if err := s.Store.CreateRun(ctx, run); err != nil {
		return Report{}, err
	}
	w := &watcher{s: s, src: src, run: run, opts: opts, done: map[string]bool{}}

	if !opts.Since.IsZero() {
		recs, err := src.Records(ctx, Window{From: opts.Since, To: started})
		if err != nil {
			return w.finish(ctx, err)
		}
		for _, r := range recs {
			if err := w.handle(ctx, r); err != nil {
				return w.finish(ctx, err)
			}
		}
	}
	if opts.OnReady != nil {
		opts.OnReady(run.ID)
	}

	for {
		select {
		case <-ctx.Done():
			return w.finish(ctx, nil)
		case msg, ok := <-ch:
			if !ok {
				return w.finish(ctx, nil)
			}
			var ref Ref
			if err := json.Unmarshal(msg.Payload, &ref); err != nil || ref.Node == "" {
				s.logger().Warn("online: ignoring malformed announcement", "payload", string(msg.Payload))
				continue
			}
			if w.done[ref.Conversation+"\x00"+ref.Node] {
				continue
			}
			rec, err := src.Lookup(ctx, ref)
			if errors.Is(err, ErrNotFinished) {
				s.logger().Warn("online: announced run not found", "conversation", ref.Conversation, "node", ref.Node, "error", err)
				continue
			}
			if err != nil {
				if ctx.Err() != nil {
					return w.finish(ctx, nil)
				}
				return w.finish(ctx, err)
			}
			if err := w.handle(ctx, rec); err != nil {
				if ctx.Err() != nil {
					return w.finish(ctx, nil)
				}
				return w.finish(ctx, err)
			}
		}
	}
}

type watcher struct {
	s       *Sampler
	src     Source
	run     eval.RunRecord
	opts    WatchOptions
	done    map[string]bool
	rep     Report
	skipped atomic.Int64
}

// handle filters, samples, scores, and stores one record.
func (w *watcher) handle(ctx context.Context, r Record) error {
	id := r.Ref.Conversation + "\x00" + r.Ref.Node
	if w.done[id] {
		return nil
	}
	w.done[id] = true
	w.rep.Seen++
	if !w.s.Filter.Match(r) {
		return nil
	}
	w.rep.Matched++
	if !w.s.Sampled(r) {
		return nil
	}
	obs, err := r.Observation()
	if err != nil {
		return err
	}
	suite, err := eval.Run(ctx, w.s.suite(), []eval.Observation{obs}, w.s.scorers(&w.skipped), eval.WithLogger(w.s.logger()))
	if suite == nil || len(suite.Results) == 0 {
		return err // the context ended before the record was scored
	}
	u := suite.Units(w.run.ID)[0]
	stored, err := w.s.Store.PutUnit(ctx, withTrace(u))
	if err != nil {
		return fmt.Errorf("save unit %q: %w", u.Key, err)
	}
	w.rep.Scored++
	if w.opts.OnUnit != nil {
		w.opts.OnUnit(stored)
	}
	return nil
}

// finish marks the run finished with the summary of its stored units.
func (w *watcher) finish(ctx context.Context, cause error) (Report, error) {
	ctx = context.WithoutCancel(ctx)
	w.rep.JudgesSkipped = int(w.skipped.Load())
	units, err := w.s.Store.Units(ctx, w.run.ID, store.UnitFilter{})
	if err != nil {
		return w.rep, errors.Join(cause, err)
	}
	suite := eval.SuiteFromUnits(w.run, units)
	for _, u := range units {
		for _, sc := range u.Scores {
			if sc.Error != "" {
				suite.ErroredCases++
				break
			}
		}
	}
	final := eval.NewRunRecord(w.run.ID, suite, cause, w.run.Provenance)
	final.Labels = w.run.Labels
	w.rep.Run = final
	if err := w.s.Store.UpdateRun(ctx, final); err != nil {
		return w.rep, errors.Join(cause, err)
	}
	return w.rep, cause
}
