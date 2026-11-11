package egress_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/bus"
	"github.com/urmzd/saige/flow/connector"
	"github.com/urmzd/saige/flow/connectors/github"
	"github.com/urmzd/saige/flow/connectors/slack"
	"github.com/urmzd/saige/flow/egress"
	"github.com/urmzd/saige/flow/event"
)

const tenant = "acme"

// run is a fixed run sequence: what a run publishes on its channel, from
// accepted to completed, with a citation and a tool call on the way.
func run(channel string) []event.Envelope {
	bodies := []string{
		event.TypeAccepted + `|{}`,
		event.TypeStarted + `|{"attempt":1}`,
		event.TypeText + `|{"text":"The retry path posts twice "}`,
		event.TypeToolCall + `|{"id":"c1","name":"search","phase":"executing"}`,
		event.TypeToolDone + `|{"id":"c1","name":"search","result":"3 hits"}`,
		event.TypeText + `|{"text":"because the handle was lost [1]."}`,
		event.TypeCitation + `|{"label":"[1]","source":{"document_title":"sink.go","source_uri":"https://github.com/urmzd/saige/flow/blob/main/sink.go"}}`,
		event.TypeCompleted + `|{"attempt":1,"text":"The retry path posts twice because the handle was lost [1]."}`,
	}
	out := make([]event.Envelope, len(bodies))
	for i, b := range bodies {
		var typ, data string
		for j := range b {
			if b[j] == '|' {
				typ, data = b[:j], b[j+1:]
				break
			}
		}
		body, err := event.Decode(typ, json.RawMessage(data))
		if err != nil {
			panic(err)
		}
		seq := uint64(i + 1)
		out[i] = event.Envelope{
			ID: fmt.Sprintf("run_1:%d", seq), Source: "fiber://run/run_1", Type: typ,
			Channel: channel, Time: time.Now().UTC(), Tenant: tenant, Run: "run_1", Seq: seq,
			Agent: "docs-bot", Version: "1", Data: body,
		}
	}
	return out
}

// surface is a sink connector and what it has rendered so far.
type surface struct {
	sink     connector.Sink
	rendered func() any
}

// golden replays the run through egress and, separately, delivers every
// event straight to a second connector in order, as the router did. Both
// surfaces must end the same.
func golden(t *testing.T, channel string, build func() surface) {
	t.Helper()
	events := run(channel)

	direct := build()
	target := address.MustParse(channel)
	for _, e := range events {
		if err := direct.sink.Deliver(context.Background(), target, e); err != nil {
			t.Fatalf("direct deliver %s: %v", e.Type, err)
		}
	}
	want := direct.rendered()

	routed := build()
	reg := connector.NewRegistry()
	if err := reg.Register(routed.sink); err != nil {
		t.Fatal(err)
	}
	b := bus.NewMemory()
	runEgress(t, egress.New(b, reg))
	for _, e := range events {
		if err := b.Publish(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for !reflect.DeepEqual(routed.rendered(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("egress rendered\n%+v\nwant\n%+v", routed.rendered(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func runEgress(t *testing.T, e *egress.Engine) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx, tenant) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("egress: %v", err)
		}
	})
}

func TestEgressGoldenSlack(t *testing.T) {
	golden(t, "slack://T01ABCDEF/C02CHANNEL?thread=1700000000.000100", func() surface {
		fake := slack.NewFake()
		c, err := slack.New(slack.Config{API: fake, Team: "T01ABCDEF", Interval: -1})
		if err != nil {
			t.Fatal(err)
		}
		return surface{sink: c, rendered: func() any { return fake.Messages() }}
	})
}

func TestEgressGoldenGitHub(t *testing.T) {
	golden(t, "github://urmzd/fiber/issues/42", func() surface {
		fake := github.NewFake()
		c, err := github.New(github.Config{API: fake, Owners: []string{"urmzd"}, Interval: -1})
		if err != nil {
			t.Fatal(err)
		}
		return surface{sink: c, rendered: func() any { return fake.Comments() }}
	})
}

// recorder is a sink that records what it was handed, by target.
type recorder struct {
	name string
	subs []connector.Subscription

	mu   sync.Mutex
	seen []string
}

func (r *recorder) Name() string { return r.name }

func (r *recorder) Deliver(_ context.Context, target address.Address, e event.Envelope) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, fmt.Sprintf("%s %s %d", target, e.Channel, e.Seq))
	return nil
}

func (r *recorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

// recorderSub is a recorder that subscribes on its own.
type recorderSub struct{ *recorder }

func (r recorderSub) Subscriptions() []connector.Subscription { return r.subs }

// A sink renders only its own scheme's channels: a run on a GitHub issue is
// never posted to Slack because Slack is configured, and a sink that names a
// channel of another scheme renders it only at a target of its own.
func TestEgressOnlyOwnScheme(t *testing.T) {
	sl := &recorder{name: "slack"}
	gh := &recorder{name: "github"}
	hook := &recorder{name: "webhook", subs: []connector.Subscription{{
		Channels: []string{"github://urmzd/"},
		Types:    []string{event.TypeCompleted},
		Target:   address.MustParse("webhook://acme/deploys"),
	}}}
	reg := connector.NewRegistry()
	for _, s := range []connector.Sink{sl, gh, recorderSub{hook}} {
		if err := reg.Register(s); err != nil {
			t.Fatal(err)
		}
	}
	b := bus.NewMemory()
	runEgress(t, egress.New(b, reg))

	issue := "github://urmzd/fiber/issues/42"
	events := run(issue)
	// A control event on the same tenant is never delivered.
	stop := event.Envelope{
		ID: "stop-1", Source: "fiber://control", Type: event.TypeStop, Channel: "control://acme",
		Time: time.Now().UTC(), Tenant: tenant, Run: "run_1",
		Data: event.StopRequest{RunID: "run_1", By: "ana", At: time.Now().UTC()},
	}
	// Another tenant's run on the same channel is not this engine's.
	other := run(issue)[0]
	other.Tenant, other.Run, other.ID = "globex", "run_2", "run_2:1"
	for _, e := range append(append(events, stop), other) {
		if err := b.Publish(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(10 * time.Second)
	for (len(gh.got()) < len(events) || len(hook.got()) < 1) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)

	if got := sl.got(); len(got) != 0 {
		t.Errorf("slack was handed a GitHub run: %v", got)
	}
	var want []string
	for _, e := range events {
		want = append(want, fmt.Sprintf("%s %s %d", issue, issue, e.Seq))
	}
	if got := gh.got(); !reflect.DeepEqual(got, want) {
		t.Errorf("github got %v, want %v", got, want)
	}
	if got, want := hook.got(), []string{fmt.Sprintf("webhook://acme/deploys %s %d", issue, len(events))}; !reflect.DeepEqual(got, want) {
		t.Errorf("webhook got %v, want %v", got, want)
	}
}

// A subscription whose target belongs to another connector is refused.
func TestEgressRefusesATargetOfAnotherScheme(t *testing.T) {
	bad := &recorder{name: "webhook", subs: []connector.Subscription{{
		Channels: []string{"github://"},
		Target:   address.MustParse("slack://T01/C02"),
	}}}
	reg := connector.NewRegistry()
	if err := reg.Register(recorderSub{bad}); err != nil {
		t.Fatal(err)
	}
	err := egress.New(bus.NewMemory(), reg).Run(context.Background(), tenant)
	if !errors.Is(err, egress.ErrInvalid) {
		t.Fatal("Run accepted a subscription whose target is another connector's")
	}
}

// An event older than the max age is not delivered: a resubscribe does not
// replay what the bus retained long ago.
func TestEgressSkipsAnEventOlderThanItsMaxAge(t *testing.T) {
	gh := &recorder{name: "github"}
	reg := connector.NewRegistry()
	if err := reg.Register(gh); err != nil {
		t.Fatal(err)
	}
	b := bus.NewMemory()
	runEgress(t, egress.New(b, reg, egress.WithMaxAge(time.Minute)))
	old := run("github://urmzd/fiber/issues/42")
	old[0].Time = time.Now().Add(-time.Hour)
	fresh := old[1]
	for _, e := range []event.Envelope{old[0], fresh} {
		if err := b.Publish(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(gh.got()) < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := gh.got(); len(got) != 1 || got[0] != "github://urmzd/fiber/issues/42 github://urmzd/fiber/issues/42 2" {
		t.Errorf("delivered %v, want only the fresh event", got)
	}
}

// waitFor polls until cond holds or fails the test.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// twoEngines runs two Engines, as two replicas would, on the buses given
// (one shared bus, or two on one database), delivering to one recorder, and
// checks every event reaches the surface exactly once.
func twoEngines(t *testing.T, one, two bus.Bus) {
	t.Helper()
	gh := &recorder{name: "github"}
	reg := connector.NewRegistry()
	if err := reg.Register(gh); err != nil {
		t.Fatal(err)
	}
	runEgress(t, egress.New(one, reg))
	runEgress(t, egress.New(two, reg))

	issue := "github://urmzd/fiber/issues/42"
	events := run(issue)
	for i, e := range events {
		b := one
		if i%2 == 1 {
			b = two
		}
		if err := b.Publish(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "every event delivered", func() bool { return len(gh.got()) >= len(events) })
	time.Sleep(300 * time.Millisecond)

	var want []string
	for _, e := range events {
		want = append(want, fmt.Sprintf("%s %s %d", issue, issue, e.Seq))
	}
	if got := gh.got(); !reflect.DeepEqual(got, want) {
		t.Errorf("two engines delivered %v, want each event once: %v", got, want)
	}
}

// Two Engines on one bus are one deliverer: a Slack message or a GitHub
// comment is posted once, not once per replica.
func TestEgressTwoEnginesDeliverEachEventOnce(t *testing.T) {
	b := bus.NewMemory()
	twoEngines(t, b, b)
}

// The same on Postgres, with each Engine on a bus of its own over one
// database: two processes.
func TestEgressTwoEnginesOnPostgresDeliverEachEventOnce(t *testing.T) {
	url := os.Getenv("SAIGE_TEST_POSTGRES_DSN")
	if url == "" {
		t.Skip("set SAIGE_TEST_POSTGRES_DSN")
	}
	pool := postgresPool(t, url)
	one := bus.NewPostgres(pool, bus.WithPollInterval(25*time.Millisecond))
	two := bus.NewPostgres(pool, bus.WithPollInterval(25*time.Millisecond))
	if err := one.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	twoEngines(t, one, two)
}

// An Engine that stops commits what it delivered, and the Engine that takes
// over starts after it: nothing is posted twice and nothing is skipped.
func TestEgressTakesOverFromTheCommittedCursor(t *testing.T) {
	gh := &recorder{name: "github"}
	reg := connector.NewRegistry()
	if err := reg.Register(gh); err != nil {
		t.Fatal(err)
	}
	b := bus.NewMemory()
	issue := "github://urmzd/fiber/issues/42"
	events := run(issue)

	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- egress.New(b, reg).Run(ctx, tenant) }()
	for _, e := range events[:3] {
		if err := b.Publish(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "the first engine to deliver three events", func() bool { return len(gh.got()) == 3 })
	stop()
	if err := <-done; err != nil {
		t.Fatalf("first engine: %v", err)
	}

	runEgress(t, egress.New(b, reg))
	for _, e := range events[3:] {
		if err := b.Publish(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "every event delivered", func() bool { return len(gh.got()) >= len(events) })
	time.Sleep(200 * time.Millisecond)
	var want []string
	for _, e := range events {
		want = append(want, fmt.Sprintf("%s %s %d", issue, issue, e.Seq))
	}
	if got := gh.got(); !reflect.DeepEqual(got, want) {
		t.Errorf("delivered %v, want each event once across the takeover: %v", got, want)
	}
}

// postgresPool returns a pool on a schema of its own, dropped when the test
// ends.
func postgresPool(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	schema := fmt.Sprintf("fibertest_egress_%d", time.Now().UnixNano())
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA "+ident+" CASCADE")
		pool.Close()
	})
	return pool
}
