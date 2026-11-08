// Package cron is the schedule peer in the mesh: a trigger, and only a trigger.
//
// # Why there is no Sink here
//
// A schedule is not a place. Slack has a thread to answer in and GitHub has an
// issue to comment on, but "every weekday at 09:00" has no surface, so there is
// nothing for a Sink to render onto and this connector deliberately implements
// none.
//
// A tick is published as cron.tick.v1 on the schedule's channel,
// cron://<tenant>/<name>. It names no agent. A schedule's Agent becomes a
// member of that channel that wakes on its ticks (Config.Members), and any
// egress adapter subscribed to the channel, such as an outbound webhook,
// renders what the run produces.
//
// # The whole correctness story is the occurrence id
//
// Everything else in this package is scheduling arithmetic. The one property
// that matters is that a given firing of a given schedule has ONE identity, no
// matter which process computed it or how many times:
//
//	id = "<schedule>@<unix seconds of the scheduled instant>"
//
// The id comes from the instant the expression selected, never from the instant
// this process noticed it was due. So a restart, a redelivery, and a second
// replica all produce the same string, and the event id is part of the run key,
// so they join one run instead of starting three. See OccurrenceID.
//
// On top of that, the ticker never looks backwards. A schedule seen for the
// first time starts from the current instant, so a process that was down over
// the weekend does not wake up and fire Saturday and Sunday at once. Missed
// occurrences are missed, on purpose: an agent asked to summarize yesterday is
// useless a day late, and the alternative — replaying a backlog on boot — is how
// a scheduler turns an outage into a thundering herd.
//
// # No goroutine you did not ask for, and no sleeping in tests
//
// Tick does one pass and is exported. Ingest runs it on an interval, or, with a
// non-positive Interval, runs no ticker at all and leaves the driving to the
// caller — which is what a deployment with a leader-elected scheduler wants, and
// what a test wants. Config.Now is the clock seam, so scheduling is exercised by
// moving a variable rather than by sleeping.
//
// This package is stdlib plus the mesh's own leaf packages. It speaks to no
// network and holds no credential: a schedule is entirely a local decision.
package cron

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/urmzd/saige/flow/connector"
	"github.com/urmzd/saige/flow/connectors/internal/inbound"
	"github.com/urmzd/saige/flow/membership"
)

// Name is the address scheme this connector owns.
const Name = "cron"

// Defaults for the ticker.
const (
	// DefaultInterval is how often Ingest looks for due occurrences. A cron
	// expression resolves to the minute, so anything below a minute only affects
	// how promptly an occurrence is noticed; a second keeps a 09:00 schedule
	// visibly punctual without the loop costing anything.
	DefaultInterval = time.Second

	// DefaultMaxCatchUp bounds how many occurrences of ONE schedule a single tick
	// will emit. It exists for the case where the loop was stalled — a long GC, a
	// blocked router, a paused container — and an every-minute schedule now has a
	// hundred occurrences owed. Emitting them all at once would be a self-inflicted
	// denial of service; emitting a few per tick drains the backlog at a rate the
	// rest of the mesh can absorb, and each one keeps its own stable id.
	DefaultMaxCatchUp = 8
)

// Config is everything the cron connector needs.
type Config struct {
	// Store holds the schedules. Required. It is read on every tick, so adding
	// or removing a schedule takes effect immediately with no reload path.
	Store Store

	// Interval is how often Ingest ticks. Zero means DefaultInterval. A
	// non-positive interval runs no ticker: Ingest only holds the channel, and
	// the caller drives Tick.
	Interval time.Duration

	// Now is the clock. Nil means time.Now. It is the seam that lets scheduling
	// be tested without sleeping.
	Now func() time.Time

	// Location is the time zone expressions are interpreted in. Nil means UTC.
	//
	// UTC is the default because it is the only zone with no gaps and no
	// repeats. In a zone with daylight saving, an occurrence inside the hour that
	// is skipped forward never happens, and one inside the hour that repeats
	// happens once, because the cursor has already passed it. Both are defensible
	// and neither is what somebody who wrote "0 2 * * *" was picturing, so the
	// default declines to guess.
	Location *time.Location

	// MaxCatchUp bounds occurrences emitted per schedule per tick. Zero means
	// DefaultMaxCatchUp.
	MaxCatchUp int

	// Log receives the errors the background ticker cannot return to anyone.
	// Nil means slog.Default.
	Log *slog.Logger

	// Members, when set, makes each schedule's Agent a member of the
	// schedule's channel that wakes on cron.tick events, before its first tick
	// is published. Nil leaves membership to the host.
	Members Members
}

// Members is the part of a membership store a schedule needs: joining its
// agent to its channel. *membership.Memory and *membership.Postgres satisfy
// it.
type Members interface {
	Join(ctx context.Context, tenant, channel, agent string, rule membership.Rule) error
}

// Connector is the schedule peer. It implements pkg/connector's Trigger face and
// no other.
type Connector struct {
	store      Store
	interval   time.Duration
	maxCatchUp int
	loc        *time.Location
	now        func() time.Time
	log        *slog.Logger

	pipe    inbound.Pipe
	members Members

	// tick serializes Tick, so a caller driving it by hand cannot race the
	// background loop into emitting an occurrence twice. Cursors are only ever
	// read and written under it.
	tick    sync.Mutex
	cursors map[string]time.Time
	specs   map[string]Spec
	// joined is the agent each schedule's channel was last joined for, so a
	// schedule joins once per process and again when its agent changes.
	joined map[string]string
}

// Compile-time proof of the faces. The absence of Sink and Actor is the
// declaration that matters: this connector cannot receive and cannot act.
var (
	_ connector.Trigger   = (*Connector)(nil)
	_ connector.Connector = (*Connector)(nil)
)

// New returns a cron connector.
func New(cfg Config) (*Connector, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("cron: config needs a schedule store")
	}
	c := &Connector{
		store:      cfg.Store,
		interval:   cfg.Interval,
		maxCatchUp: cfg.MaxCatchUp,
		loc:        cfg.Location,
		now:        cfg.Now,
		log:        cfg.Log,
		cursors:    make(map[string]time.Time),
		specs:      make(map[string]Spec),
		joined:     make(map[string]string),
		members:    cfg.Members,
	}
	if c.interval == 0 {
		c.interval = DefaultInterval
	}
	if c.maxCatchUp <= 0 {
		c.maxCatchUp = DefaultMaxCatchUp
	}
	if c.loc == nil {
		c.loc = time.UTC
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	return c, nil
}

// Name implements connector.Connector.
func (c *Connector) Name() string { return Name }

// Ingest implements connector.Trigger.
//
// It claims publish, starts the ticker, and parks until ctx is cancelled. With a
// non-positive Config.Interval no ticker is started and the caller is expected
// to drive Tick.
func (c *Connector) Ingest(ctx context.Context, publish connector.Publish) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan struct{})
	if c.interval > 0 {
		go func() {
			defer close(done)
			c.loop(ctx)
		}()
	} else {
		close(done)
	}

	err := c.pipe.Serve(ctx, publish)
	// Serve returned, so the channel is no longer claimed and the ticker has
	// nowhere to send. Stop it before returning, so Ingest owns the whole
	// lifetime of everything it started.
	cancel()
	<-done
	return err
}

// Ingesting reports whether an Ingest is running.
func (c *Connector) Ingesting() bool { return c.pipe.Ingesting() }

// loop ticks until ctx ends.
func (c *Connector) loop(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.Tick(ctx); err != nil && ctx.Err() == nil {
				// The loop keeps going. A schedule with a bad expression, or a
				// store that is momentarily unavailable, must not stop the
				// schedules that are fine — and the next tick is a second away.
				c.log.Error("cron: tick failed", "error", err)
			}
		}
	}
}

// Tick emits every occurrence that has come due since the previous tick.
//
// It is exported because the loop is the least interesting part of a scheduler
// and the most opinionated: a deployment that already has leader election, or a
// Kubernetes CronJob, or a test with a fake clock, drives this directly and
// leaves Config.Interval non-positive.
//
// Emission is ordered and cursor-advancing: a schedule's cursor moves only after
// its tick has been published by the running Ingest, so a tick that fails to hand off
// leaves the occurrence owed rather than lost. Errors from separate schedules
// are joined, because one broken expression must not stop the others.
func (c *Connector) Tick(ctx context.Context) error {
	schedules, err := c.store.Schedules(ctx)
	if err != nil {
		return fmt.Errorf("cron: read schedules: %w", err)
	}

	c.tick.Lock()
	defer c.tick.Unlock()

	now := c.now().In(c.loc)
	live := make(map[string]struct{}, len(schedules))
	var errs []error
	for _, s := range schedules {
		live[s.Name] = struct{}{}
		if err := c.fire(ctx, s, now); err != nil {
			errs = append(errs, err)
		}
	}

	// Forget the cursors of schedules that no longer exist, so a long-lived
	// process does not accumulate one entry per schedule ever configured. A
	// schedule that comes back is a schedule seen for the first time, which
	// starts from now and does not backfill — the same rule as a restart.
	for name := range c.cursors {
		if _, ok := live[name]; !ok {
			delete(c.cursors, name)
			delete(c.joined, name)
		}
	}
	return errors.Join(errs...)
}

// fire emits the occurrences of one schedule that fall in (cursor, now].
func (c *Connector) fire(ctx context.Context, s Schedule, now time.Time) error {
	if err := s.Validate(); err != nil {
		return err
	}
	spec, err := c.spec(s.Expression)
	if err != nil {
		return fmt.Errorf("cron: schedule %q: %w", s.Name, err)
	}

	cursor, seen := c.cursors[s.Name]
	if !seen {
		// First sight of this schedule in this process. The cursor starts at now,
		// which is what makes a restart quiet: everything before this instant is
		// treated as already handled, whether it was or not. See the package doc
		// for why not backfilling is the deliberate choice.
		c.cursors[s.Name] = now
		return nil
	}

	for range c.maxCatchUp {
		occurrence, ok := spec.Next(cursor)
		if !ok {
			return fmt.Errorf("cron: schedule %q: %q has no occurrence within %d years", s.Name, s.Expression, searchYears)
		}
		if occurrence.After(now) {
			return nil
		}
		if err := c.join(ctx, s); err != nil {
			return err
		}
		if err := c.pipe.Send(ctx, s.tick(occurrence)); err != nil {
			// The cursor stays where it was, so this occurrence is emitted again
			// on the next tick. That is safe precisely because its id is derived
			// from the scheduled instant: a retry cannot become a second run.
			return fmt.Errorf("cron: emit %s: %w", OccurrenceID(s.Name, occurrence), err)
		}
		cursor = occurrence
		c.cursors[s.Name] = occurrence
	}
	return nil
}

// join makes the schedule's agent a member of its channel that wakes on its
// ticks, once per process and agent. The channel is the schedule's own, so
// the membership replaces nothing the host set elsewhere.
func (c *Connector) join(ctx context.Context, s Schedule) error {
	if c.members == nil || s.Agent == "" || c.joined[s.Name] == s.Agent {
		return nil
	}
	rule := membership.Rule{Types: []string{"cron.tick"}}.Ambient()
	if err := c.members.Join(ctx, s.Tenant, s.Channel().String(), s.Agent, rule); err != nil {
		return fmt.Errorf("cron: schedule %q: join %s to its channel: %w", s.Name, s.Agent, err)
	}
	c.joined[s.Name] = s.Agent
	return nil
}

// spec parses and caches an expression. The cache is keyed on the expression
// text rather than the schedule name so that editing a schedule reparses it.
func (c *Connector) spec(expr string) (Spec, error) {
	if s, ok := c.specs[expr]; ok {
		return s, nil
	}
	s, err := Parse(expr)
	if err != nil {
		return Spec{}, err
	}
	c.specs[expr] = s
	return s, nil
}

// Cursor reports the last occurrence emitted for a schedule, and whether the
// connector has seen the schedule at all. It exists for operators asking "when
// did this last fire" and for tests; the ticker does not use it.
func (c *Connector) Cursor(schedule string) (time.Time, bool) {
	c.tick.Lock()
	defer c.tick.Unlock()
	at, ok := c.cursors[schedule]
	return at, ok
}
