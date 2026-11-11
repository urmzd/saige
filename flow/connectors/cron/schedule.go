package cron

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors/mention"
	"github.com/urmzd/saige/flow/event"
)

// ErrSchedule reports a schedule that cannot be run. Callers match with
// errors.Is.
var ErrSchedule = errors.New("cron: invalid schedule")

// Schedule is one standing request: publish this text on this expression.
//
// It is the whole of what a schedule is. There is no per-schedule state here,
// no "last run" and no "next run", because a schedule is a declaration and the
// position in it belongs to whoever is ticking, which is the connector.
type Schedule struct {
	// Name identifies the schedule and appears in two places that outlive this
	// process: the channel and the occurrence id. It is held to the address
	// workspace charset (letters, digits, "-", "_") so that both stay readable
	// and unescaped.
	Name string

	// Expression is a standard 5-field cron expression. See Parse.
	Expression string

	// Agent, when set, is made a member of the schedule's channel that wakes
	// on its ticks (Config.Members). The tick itself names no agent.
	Agent string

	// Text is the request a tick carries. A schedule has no human typing a
	// message, so this is the message: "summarize yesterday's incidents".
	Text string

	// Tenant is the authorization scope the schedule was created under, and it
	// becomes both the tick's tenant and the workspace of its channel.
	//
	// A schedule is the one trigger where carrying a tenant is not "trusting the
	// wire": nothing arrives from outside. The schedule is a stored object,
	// written by a caller whose credential was already checked, and this field is
	// that credential's scope recorded at creation time.
	Tenant string

	// Context is extra trigger context shown to the agent next to the tick's
	// own fields. It never overrides them.
	Context map[string]string
}

// Validate checks that the schedule can run. It parses the expression, so a
// schedule that passes cannot fail to schedule later.
func (s Schedule) Validate() error {
	switch {
	case s.Name == "":
		return fmt.Errorf("%w: schedule has no name", ErrSchedule)
	case s.Tenant == "":
		return fmt.Errorf("%w: schedule %q has no tenant, so it has no scope and no channel", ErrSchedule, s.Name)
	}
	if err := address.ValidWorkspace(s.Name); err != nil {
		return fmt.Errorf("%w: schedule name %q is not usable in an address or an occurrence id: %w", ErrSchedule, s.Name, err)
	}
	if err := address.ValidWorkspace(s.Tenant); err != nil {
		return fmt.Errorf("%w: schedule %q has a tenant that cannot be an address workspace: %w", ErrSchedule, s.Name, err)
	}
	if s.Agent != "" && !mention.Valid(s.Agent) {
		return fmt.Errorf("%w: schedule %q names agent %q, which is not a usable agent name", ErrSchedule, s.Name, s.Agent)
	}
	if _, err := Parse(s.Expression); err != nil {
		return fmt.Errorf("%w: schedule %q: %w", ErrSchedule, s.Name, err)
	}
	return nil
}

// Channel is where the schedule's ticks are published:
// cron://<tenant>/<name>. Its members decide who a tick wakes, and an egress
// adapter subscribed to it renders the runs.
func (s Schedule) Channel() address.Address {
	return address.Address{Connector: Name, Workspace: s.Tenant, Path: []string{s.Name}}
}

// OccurrenceID is the identity of one firing: "<schedule>@<unix seconds>".
//
// It is derived from the occurrence, never from the clock of the process that
// computed it, so every replica and every restart computes the same id for the
// same firing. That id is the tick's event id, and so part of its run key: a
// firing seen twice joins one run.
func OccurrenceID(schedule string, occurrence time.Time) string {
	return schedule + "@" + strconv.FormatInt(occurrence.Unix(), 10)
}

// tick is the cron.tick.v1 event of one occurrence. Its source is opaque
// ("cron:<tenant>/<name>"), not an address: a tick has no surface, so each
// run it starts is a conversation of its own.
func (s Schedule) tick(occurrence time.Time) event.Envelope {
	actor := event.Actor{ID: Name + ":" + s.Name, Display: "schedule " + s.Name, Bot: true}
	req := event.Request{Text: s.Text, Actor: actor}
	if len(s.Context) > 0 {
		req.Context = maps.Clone(s.Context)
	}
	return event.Envelope{
		ID:      OccurrenceID(s.Name, occurrence),
		Source:  Name + ":" + s.Tenant + "/" + s.Name,
		Type:    event.TypeCronTick,
		Subject: s.Name,
		Channel: s.Channel().String(),
		Time:    occurrence.UTC(),
		Tenant:  s.Tenant,
		Data: event.CronTick{
			Request:    req,
			Schedule:   s.Name,
			Expression: s.Expression,
			Occurrence: occurrence.UTC(),
		},
	}
}

// Store holds the schedules a deployment runs.
//
// It is an interface with one method because a deployment's schedules live
// wherever its control plane keeps them — a table, a config file, an agent spec
// — and the ticker only ever needs to ask what they are right now. Reading them
// on every tick rather than caching is deliberate: a schedule added, edited or
// removed takes effect on the next tick with no reload path to get wrong.
type Store interface {
	Schedules(ctx context.Context) ([]Schedule, error)
}

// Memory is a complete in-process schedule store, safe for concurrent use.
//
// It is not a test double: a single-node deployment that keeps its schedules in
// its own configuration can run on it forever. What it does not do is survive a
// restart, which costs nothing here — the connector does not backfill missed
// occurrences anyway, so the state worth persisting is the schedule list itself,
// and that belongs to whatever wrote it.
type Memory struct {
	mu     sync.Mutex
	byName map[string]Schedule
}

var _ Store = (*Memory)(nil)

// NewMemory returns a store holding the given schedules. Every schedule is
// validated, so a typo in an expression is a startup error rather than a
// schedule that silently never fires.
func NewMemory(schedules ...Schedule) (*Memory, error) {
	m := &Memory{byName: make(map[string]Schedule, len(schedules))}
	for _, s := range schedules {
		if err := m.Add(s); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Add stores a schedule, replacing any schedule of the same name.
func (m *Memory) Add(s Schedule) error {
	if err := s.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byName == nil {
		m.byName = make(map[string]Schedule)
	}
	m.byName[s.Name] = s
	return nil
}

// Remove deletes a schedule and reports whether it was there.
func (m *Memory) Remove(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.byName[name]
	delete(m.byName, name)
	return ok
}

// Schedules implements Store, returning the schedules in name order so that a
// tick's emissions are deterministic.
func (m *Memory) Schedules(context.Context) ([]Schedule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Schedule, 0, len(m.byName))
	for _, s := range m.byName {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
