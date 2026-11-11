package cron_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/flow/connectors/internal/rendertest"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/membership"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connector"
	"github.com/urmzd/saige/flow/connectors/cron"
)

// ── Expression parsing ──────────────────────────────────────────────

func TestParseAcceptsStarsRangesListsAndSteps(t *testing.T) {
	// Each case names the first occurrence strictly after "from", which is the
	// only question the scheduler ever asks a Spec.
	tests := []struct {
		name string
		expr string
		from string
		want string
	}{
		{
			name: "every minute",
			expr: "* * * * *",
			from: "2026-07-20T12:00:30Z",
			want: "2026-07-20T12:01:00Z",
		},
		{
			name: "a step selects every nth value from the start of the field",
			expr: "*/15 * * * *",
			from: "2026-07-20T12:01:00Z",
			want: "2026-07-20T12:15:00Z",
		},
		{
			name: "a step wraps to the next hour rather than to minute 60",
			expr: "*/15 * * * *",
			from: "2026-07-20T12:45:00Z",
			want: "2026-07-20T13:00:00Z",
		},
		{
			name: "a range bounds the field",
			expr: "0 9-17 * * *",
			from: "2026-07-20T17:30:00Z",
			want: "2026-07-21T09:00:00Z",
		},
		{
			name: "a stepped range steps from the range's start",
			expr: "0 8-18/4 * * *",
			from: "2026-07-20T09:00:00Z",
			want: "2026-07-20T12:00:00Z",
		},
		{
			name: "a list is the union of its items",
			expr: "0,30 * * * *",
			from: "2026-07-20T12:00:00Z",
			want: "2026-07-20T12:30:00Z",
		},
		{
			name: "a list of ranges and steps composes",
			expr: "0 0 1,15 * *",
			from: "2026-07-20T00:00:00Z",
			want: "2026-08-01T00:00:00Z",
		},
		{
			name: "an exact instant is never returned for itself, only the next one",
			expr: "0 3 * * *",
			from: "2026-07-20T03:00:00Z",
			want: "2026-07-21T03:00:00Z",
		},
		{
			name: "a month restriction skips whole months",
			expr: "0 0 1 1 *",
			from: "2026-07-20T00:00:00Z",
			want: "2027-01-01T00:00:00Z",
		},
		{
			name: "day of week 0 and 7 both mean Sunday",
			expr: "0 0 * * 7",
			from: "2026-07-20T00:00:00Z",
			want: "2026-07-26T00:00:00Z",
		},
		{
			name: "a weekday range excludes the weekend",
			expr: "30 9 * * 1-5",
			from: "2026-07-24T10:00:00Z", // a Friday, after the occurrence
			want: "2026-07-27T09:30:00Z", // the following Monday
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := cron.Parse(tc.expr)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.expr, err)
			}
			got, ok := spec.Next(at(t, tc.from))
			if !ok {
				t.Fatalf("Next(%s) found no occurrence", tc.from)
			}
			if want := at(t, tc.want); !got.Equal(want) {
				t.Errorf("Next(%s) = %s, want %s", tc.from, got.UTC().Format(time.RFC3339), tc.want)
			}
			if !spec.Matches(got) {
				t.Errorf("Matches(%s) is false for an instant Next returned", got)
			}
		})
	}
}

func TestBothDayFieldsRestrictedMeansEitherNotBoth(t *testing.T) {
	// crontab(5)'s one genuine surprise. "the 1st, and every Monday" is the
	// correct reading; implementing it as an AND is silent and almost never
	// fires.
	spec, err := cron.Parse("0 0 1 * 1")
	if err != nil {
		t.Fatal(err)
	}
	// 2026-08-01 is a Saturday: it matches on day-of-month alone.
	if got, _ := spec.Next(at(t, "2026-07-28T00:00:00Z")); !got.Equal(at(t, "2026-08-01T00:00:00Z")) {
		t.Errorf("Next = %s, want the 1st even though it is not a Monday", got.UTC().Format(time.RFC3339))
	}
	// 2026-08-03 is a Monday: it matches on day-of-week alone.
	if got, _ := spec.Next(at(t, "2026-08-01T00:00:00Z")); !got.Equal(at(t, "2026-08-03T00:00:00Z")) {
		t.Errorf("Next = %s, want the Monday even though it is not the 1st", got.UTC().Format(time.RFC3339))
	}

	// With only one day field restricted, the other is not consulted at all.
	daily := cron.MustParse("0 0 15 * *")
	if got, _ := daily.Next(at(t, "2026-07-01T00:00:00Z")); !got.Equal(at(t, "2026-07-15T00:00:00Z")) {
		t.Errorf("Next = %s, want the 15th", got.UTC().Format(time.RFC3339))
	}
}

func TestExpressionsThisParserDoesNotSupportAreRefused(t *testing.T) {
	// Every one of these has a plausible meaning in some other cron. Guessing at
	// any of them would run an agent at an hour nobody chose, and would never
	// report an error.
	tests := []struct{ name, expr string }{
		{"empty", ""},
		{"four fields", "0 0 * *"},
		{"six fields, the seconds dialect", "0 0 0 * * *"},
		{"a macro", "@daily"},
		{"a month name", "0 0 1 JAN *"},
		{"a weekday name", "0 9 * * MON"},
		{"a minute out of range", "60 * * * *"},
		{"an hour out of range", "0 24 * * *"},
		{"day of month zero", "0 0 0 * *"},
		{"a step of zero", "*/0 * * * *"},
		{"a negative step", "*/-1 * * * *"},
		{"a step on a single value, which would mean only that value", "5/10 * * * *"},
		{"a backwards range", "0 22-2 * * *"},
		{"an empty list item", "0,,30 * * * *"},
		{"the Quartz no-op", "0 0 ? * *"},
		{"the Quartz last-day marker", "0 0 L * *"},
		{"the Quartz nth-weekday marker", "0 0 * * 1#2"},
		{"a bare word", "nightly"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := cron.Parse(tc.expr)
			if err == nil {
				t.Fatalf("Parse(%q) succeeded, want a refusal", tc.expr)
			}
			if !errors.Is(err, cron.ErrExpression) {
				t.Errorf("Parse(%q) error = %v, want ErrExpression", tc.expr, err)
			}
		})
	}
}

func TestAnExpressionThatCanNeverHappenReportsSoRatherThanHanging(t *testing.T) {
	// The thirtieth of February parses: every field is in range. Next must be
	// bounded rather than search forever.
	spec := cron.MustParse("0 0 30 2 *")
	if got, ok := spec.Next(at(t, "2026-01-01T00:00:00Z")); ok {
		t.Errorf("Next = %s, want no occurrence", got)
	}
}

// ── Occurrence identity ─────────────────────────────────────────────

func TestOccurrenceIdsAreStableAcrossProcessesAndUniquePerOccurrence(t *testing.T) {
	store := memory(t, nightly())
	clk := &clock{at: at(t, "2026-07-20T04:00:00Z")}

	// One process sees the schedule just after that night's 03:00, then two
	// nights pass.
	first, tags := start(t, store, clk)
	tick(t, first)
	clk.set(at(t, "2026-07-22T04:00:00Z"))
	tick(t, first)

	got := drain(t, tags, 2)
	want := []string{
		cron.OccurrenceID("nightly", at(t, "2026-07-21T03:00:00Z")),
		cron.OccurrenceID("nightly", at(t, "2026-07-22T03:00:00Z")),
	}
	if !equal(ids(got), want) {
		t.Fatalf("ids = %v, want %v", ids(got), want)
	}
	if got[0].ID == got[1].ID {
		t.Error("two occurrences share one id, so the second would join the first's run")
	}

	// A completely separate connector, over the same schedule and the same two
	// nights, produces byte-identical ids. That is what makes a restart, a
	// redelivery and a second replica all one run.
	otherClock := &clock{at: at(t, "2026-07-20T04:00:00Z")}
	second, otherTags := start(t, store, otherClock)
	tick(t, second)
	otherClock.set(at(t, "2026-07-22T04:00:00Z"))
	tick(t, second)
	if !equal(ids(drain(t, otherTags, 2)), want) {
		t.Error("a second process computed different ids for the same occurrences")
	}
}

func TestARestartDoesNotRefireAnOccurrenceAlreadyDelivered(t *testing.T) {
	store := memory(t, nightly())

	// A process runs across one night and delivers 03:00.
	clk := &clock{at: at(t, "2026-07-20T04:00:00Z")}
	before, tags := start(t, store, clk)
	tick(t, before)
	clk.set(at(t, "2026-07-21T04:00:00Z"))
	tick(t, before)

	delivered := drain(t, tags, 1)
	if want := cron.OccurrenceID("nightly", at(t, "2026-07-21T03:00:00Z")); delivered[0].ID != want {
		t.Fatalf("delivered %q, want %q", delivered[0].ID, want)
	}

	// The process dies. A new one starts at the same instant, over the same
	// store, with no memory of the cursor.
	afterClock := &clock{at: at(t, "2026-07-21T04:00:00Z")}
	after, restarted := start(t, store, afterClock)
	tick(t, after)
	if n := len(restarted); n != 0 {
		t.Fatalf("a restart re-fired %d past occurrence(s): %v", n, ids(drain(t, restarted, n)))
	}

	// It resumes normally from the instant it started, so the following night
	// fires exactly once.
	afterClock.set(at(t, "2026-07-22T04:00:00Z"))
	tick(t, after)
	next := drain(t, restarted, 1)
	if want := cron.OccurrenceID("nightly", at(t, "2026-07-22T03:00:00Z")); next[0].ID != want {
		t.Errorf("after restart delivered %q, want %q", next[0].ID, want)
	}
}

func TestASchedulesFirstSightNeverBackfills(t *testing.T) {
	// The schedule has been due every minute for a decade. Being seen for the
	// first time must produce nothing at all, not ten years of tags.
	store := memory(t, cron.Schedule{
		Name: "chatty", Expression: "* * * * *", Agent: "docs-bot", Tenant: "acme", Text: "hi",
	})
	c, tags := start(t, store, &clock{at: at(t, "2026-07-20T12:00:00Z")})
	tick(t, c)
	if len(tags) != 0 {
		t.Errorf("first tick emitted %d tags, want none", len(tags))
	}
	if _, seen := c.Cursor("chatty"); !seen {
		t.Error("the schedule was not remembered, so the next tick would backfill instead")
	}
}

func TestABacklogIsDrainedInOrderAndBounded(t *testing.T) {
	store := memory(t, cron.Schedule{
		Name: "chatty", Expression: "* * * * *", Agent: "docs-bot", Tenant: "acme", Text: "hi",
	})
	clk := &clock{at: at(t, "2026-07-20T12:00:00Z")}
	c, err := cron.New(cron.Config{Store: store, Interval: -1, Now: clk.now, MaxCatchUp: 3})
	if err != nil {
		t.Fatal(err)
	}
	tags := ingest(t, c)

	tick(t, c)
	// Twenty minutes elapse in one tick: the loop was stalled.
	clk.set(at(t, "2026-07-20T12:20:00Z"))
	tick(t, c)

	got := drain(t, tags, 3)
	if len(got) != 3 {
		t.Fatalf("emitted %d tags, want MaxCatchUp of 3", len(got))
	}
	for i, want := range []string{"12:01:00Z", "12:02:00Z", "12:03:00Z"} {
		if expect := cron.OccurrenceID("chatty", at(t, "2026-07-20T"+want)); got[i].ID != expect {
			t.Errorf("tag %d = %q, want %q: the backlog must drain oldest first", i, got[i].ID, expect)
		}
	}
	// The rest is still owed, and comes out on the next tick.
	tick(t, c)
	if next := drain(t, tags, 3); len(next) != 3 || next[0].ID != cron.OccurrenceID("chatty", at(t, "2026-07-20T12:04:00Z")) {
		t.Errorf("the next tick did not resume the backlog: %v", ids(next))
	}
}

func TestAnOccurrenceThatCouldNotBeHandedOffIsStillOwed(t *testing.T) {
	// Nothing is ingesting, so the hand-off fails. The cursor must not move:
	// dropping the occurrence would lose the run, and re-emitting it is free
	// because its id is unchanged.
	store := memory(t, nightly())
	clk := &clock{at: at(t, "2026-07-20T04:00:00Z")}
	c, err := cron.New(cron.Config{Store: store, Interval: -1, Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}

	if err := c.Tick(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	clk.set(at(t, "2026-07-21T04:00:00Z"))
	if err := c.Tick(context.Background()); err == nil {
		t.Fatal("Tick with nothing ingesting reported success")
	}
	if cursor, _ := c.Cursor("nightly"); !cursor.Equal(at(t, "2026-07-20T04:00:00Z")) {
		t.Errorf("cursor = %s, want it left where it was", cursor.UTC().Format(time.RFC3339))
	}

	// Once something is listening, the same occurrence comes out, with the same
	// id it would have had.
	tags := ingest(t, c)
	tick(t, c)
	got := drain(t, tags, 1)
	if want := cron.OccurrenceID("nightly", at(t, "2026-07-21T03:00:00Z")); got[0].ID != want {
		t.Errorf("id = %q, want %q", got[0].ID, want)
	}
}

// ── The event a schedule publishes ──────────────────────────────────

func TestAScheduleTickIsACompleteEventOnItsOwnChannel(t *testing.T) {
	store := memory(t, cron.Schedule{
		Name:       "nightly",
		Expression: "0 3 * * *",
		Agent:      "docs-bot",
		Tenant:     "acme",
		Text:       "summarize yesterday's incidents",
		Context:    map[string]string{"team": "platform"},
	})
	clk := &clock{at: at(t, "2026-07-20T04:00:00Z")}
	c, tags := start(t, store, clk)
	tick(t, c)
	clk.set(at(t, "2026-07-21T03:05:00Z"))
	tick(t, c)

	e := drain(t, tags, 1)[0]
	b, ok := e.Data.(event.CronTick)
	if !ok {
		t.Fatalf("published %s, want %s", e.Type, event.TypeCronTick)
	}
	if e.ID != "nightly@1784602800" || e.Tenant != "acme" || e.Subject != "nightly" {
		t.Errorf("id/tenant/subject = %q/%q/%q", e.ID, e.Tenant, e.Subject)
	}
	// The channel is the schedule's address; the source is opaque, so each
	// run a tick starts is a conversation of its own.
	if e.Channel != "cron://acme/nightly" || e.Source != "cron:acme/nightly" {
		t.Errorf("channel/source = %q/%q", e.Channel, e.Source)
	}
	if b.Text != "summarize yesterday's incidents" || len(b.Mentions) != 0 {
		t.Errorf("text = %q, mentions = %v: the channel's members decide who answers", b.Text, b.Mentions)
	}
	if !b.Actor.Bot {
		t.Error("Actor is not marked as automation, so a sink would render a human")
	}
	// Time is the scheduled instant, not the instant the tick noticed it.
	if want := at(t, "2026-07-21T03:00:00Z"); !e.Time.Equal(want) || !b.Occurrence.Equal(want) {
		t.Errorf("time/occurrence = %s/%s, want the scheduled instant", e.Time.Format(time.RFC3339), b.Occurrence.Format(time.RFC3339))
	}
	if b.Schedule != "nightly" || b.Expression != "0 3 * * *" || b.Context["team"] != "platform" {
		t.Errorf("body = %+v", b)
	}
}

// A schedule's agent joins the schedule's channel before its first tick is
// published, waking on ticks alone, and joins once.
func TestAScheduleAgentJoinsItsChannelOnce(t *testing.T) {
	members := &joins{}
	clk := &clock{at: at(t, "2026-07-20T04:00:00Z")}
	c, err := cron.New(cron.Config{Store: memory(t, nightly()), Interval: -1, Now: clk.now, Members: members})
	if err != nil {
		t.Fatal(err)
	}
	tags := ingest(t, c)
	tick(t, c)
	clk.set(at(t, "2026-07-21T03:05:00Z"))
	tick(t, c)
	drain(t, tags, 1)

	want := []string{"acme cron://acme/nightly docs-bot [cron.tick] ambient"}
	if got := members.all(); !equal(got, want) {
		t.Errorf("joins = %q, want %q", got, want)
	}
}

func TestTheChannelAddressRoundTripsThroughPkgAddress(t *testing.T) {
	s := cron.Schedule{Name: "nightly-review", Expression: "0 3 * * *", Agent: "docs-bot", Tenant: "acme"}
	uri := s.Channel().String()
	if uri != "cron://acme/nightly-review" {
		t.Fatalf("Channel() = %q", uri)
	}
	parsed, err := address.Parse(uri)
	if err != nil {
		t.Fatalf("Parse(%q): %v", uri, err)
	}
	if parsed.String() != uri || parsed.Connector != cron.Name || parsed.Workspace != "acme" || parsed.Resource() != "nightly-review" {
		t.Errorf("round trip = %+v", parsed)
	}
}

func TestUnrunnableSchedulesAreRefusedWhenTheyAreStored(t *testing.T) {
	tests := []struct {
		name string
		s    cron.Schedule
	}{
		{"no name", cron.Schedule{Expression: "* * * * *", Agent: "a", Tenant: "acme"}},
		{"no tenant, so no scope and no address", cron.Schedule{Name: "n", Expression: "* * * * *", Agent: "a"}},
		{"a name that cannot be an address segment", cron.Schedule{Name: "nightly/review", Expression: "* * * * *", Agent: "a", Tenant: "acme"}},
		{"a tenant that cannot be a workspace", cron.Schedule{Name: "n", Expression: "* * * * *", Agent: "a", Tenant: "acme.co"}},
		{"an agent name no topic could carry", cron.Schedule{Name: "n", Expression: "* * * * *", Agent: "docs bot", Tenant: "acme"}},
		{"an expression that would silently never fire", cron.Schedule{Name: "n", Expression: "@daily", Agent: "a", Tenant: "acme"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := cron.NewMemory(tc.s); err == nil {
				t.Fatal("stored a schedule that cannot run")
			}
		})
	}
}

// ── Faces ───────────────────────────────────────────────────────────

func TestCronIsATriggerAndNothingElse(t *testing.T) {
	c, err := cron.New(cron.Config{Store: mustMemory(t)})
	if err != nil {
		t.Fatal(err)
	}
	roles := connector.RolesOf(c)
	if !roles.Trigger || roles.Sink || roles.Actor {
		t.Fatalf("RolesOf(cron) = %+v, want trigger only", roles)
	}

	registry := connector.NewRegistry()
	if err := registry.Register(c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, err = registry.Sink(address.MustParse("cron://acme/nightly"))
	if !errors.Is(err, connector.ErrUnsupported) {
		t.Errorf("Registry.Sink(cron://...) error = %v, want ErrUnsupported", err)
	}
	if len(registry.Triggers()) != 1 {
		t.Error("cron did not register as a trigger")
	}
	if len(registry.Actions()) != 0 {
		t.Error("cron contributed actions, and a schedule has no verbs")
	}
}

// ── Helpers ─────────────────────────────────────────────────────────

// clock is the seam that keeps every test in this file instantaneous.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = t
}

func at(t *testing.T, rfc3339 string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		t.Fatalf("bad timestamp %q in test: %v", rfc3339, err)
	}
	return parsed.UTC()
}

func nightly() cron.Schedule {
	return cron.Schedule{
		Name:       "nightly",
		Expression: "0 3 * * *",
		Agent:      "docs-bot",
		Tenant:     "acme",
		Text:       "summarize yesterday's incidents",
	}
}

func memory(t *testing.T, schedules ...cron.Schedule) *cron.Memory {
	t.Helper()
	m, err := cron.NewMemory(schedules...)
	if err != nil {
		t.Fatalf("NewMemory: %v", err)
	}
	return m
}

func mustMemory(t *testing.T) *cron.Memory { return memory(t) }

// start builds a connector with no ticker of its own and holds its channel, so
// the test decides when time passes and when a pass happens.
func start(t *testing.T, store cron.Store, clk *clock) (*cron.Connector, chan event.Envelope) {
	t.Helper()
	c, err := cron.New(cron.Config{Store: store, Interval: -1, Now: clk.now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, ingest(t, c)
}

func ingest(t *testing.T, c *cron.Connector) chan event.Envelope {
	t.Helper()
	tags := make(chan event.Envelope, 64)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Ingest(ctx, func(_ context.Context, e event.Envelope) error {
			tags <- e
			return nil
		})
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	deadline := time.Now().Add(2 * time.Second)
	for !c.Ingesting() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !c.Ingesting() {
		t.Fatal("Ingest never claimed the channel")
	}
	return tags
}

func tick(t *testing.T, c *cron.Connector) {
	t.Helper()
	if err := c.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
}

func drain(t *testing.T, tags chan event.Envelope, n int) []event.Envelope {
	t.Helper()
	out := make([]event.Envelope, 0, n)
	for range n {
		select {
		case e := <-tags:
			if err := e.Validate(); err != nil {
				t.Fatalf("published an invalid event: %v", err)
			}
			rendertest.Check(t, e)
			out = append(out, e)
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d events arrived", len(out), n)
		}
	}
	return out
}

// joins records the memberships a connector asks for.
type joins struct {
	mu  sync.Mutex
	got []string
}

func (j *joins) Join(_ context.Context, tenant, channel, agent string, rule membership.Rule) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	mode := "addressed"
	if rule.Addressed != nil && !*rule.Addressed {
		mode = "ambient"
	}
	j.got = append(j.got, fmt.Sprintf("%s %s %s %v %s", tenant, channel, agent, rule.Types, mode))
	return nil
}

func (j *joins) all() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.got)
}

func ids(tags []event.Envelope) []string {
	out := make([]string, len(tags))
	for i, tag := range tags {
		out[i] = tag.ID
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
