package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/flow/bus"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/runtime"
	"github.com/urmzd/saige/flow/turn/native"
)

// listener is an executor that waits, once it is running, for a request to
// compact, and records the first one it hears.
type listener struct {
	waiting chan struct{}
	heard   chan native.CompactRequest
	once    sync.Once
}

func newListener() *listener {
	return &listener{waiting: make(chan struct{}), heard: make(chan native.CompactRequest, 1)}
}

func (l *listener) Execute(ctx context.Context, req runtime.Request, _ func(context.Context, event.Body) error) (runtime.Outcome, error) {
	signal := runtime.CompactSignal(ctx)
	l.once.Do(func() { close(l.waiting) })
	select {
	case got := <-signal:
		l.heard <- got
		return runtime.Outcome{Text: "compacted", Prompt: req.Text}, nil
	case <-ctx.Done():
		return runtime.Outcome{}, ctx.Err()
	}
}

// A request reaches the attempt executing the run, under the run's own tenant
// only, and only if it was made while that attempt was listening: one made
// before the run had a worker does nothing, because the turn it would have
// compacted is not the one that runs.
func TestARequestToCompactReachesOnlyTheTurnRunningNowUnderItsTenant(t *testing.T) {
	w := newWorld(t)
	exec := newListener()
	rt := w.runtime(t, exec, runtime.WithCompactRequests())

	runID := w.accept(t, rt, tag("docs-bot", "evt-1", "explain"))
	compact := func(tenant, by string) {
		t.Helper()
		if err := rt.Compact(context.Background(), runtime.CompactRequest{Tenant: tenant, RunID: runID, By: by}); err != nil {
			t.Fatalf("Compact(%s): %v", tenant, err)
		}
	}
	compact("acme", "too early")

	w.work(t, rt, nil)
	select {
	case <-exec.waiting:
	case <-time.After(20 * time.Second):
		t.Fatal("the turn never started")
	}
	compact("globex", "intruder")
	compact("acme", "U1")

	select {
	case got := <-exec.heard:
		if got.By != "U1" {
			t.Fatalf("the turn heard a request by %q, want only the one made while it ran, under its tenant", got.By)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the turn never heard the request")
	}
	if out := w.result(t, rt, runID); out.Text != "compacted" {
		t.Errorf("Output.Text = %q", out.Text)
	}
}

// Compact answers the same for every run id, as Stop does, and refuses only
// what it cannot carry.
func TestCompactRefusesWhatItCannotCarry(t *testing.T) {
	h := newHarness(t, newExecutor("answer")) // its bus is publish-only
	if err := h.rt.Compact(context.Background(), runtime.CompactRequest{Tenant: "acme", RunID: "r1"}); !errors.Is(err, runtime.ErrCompactUnavailable) {
		t.Fatalf("Compact on a publish-only bus = %v, want ErrCompactUnavailable", err)
	}

	w := newWorld(t)
	rt := w.runtime(t, newPatient(), runtime.WithCompactRequests())
	for _, id := range []string{"", "has:colon"} {
		if err := rt.Compact(context.Background(), runtime.CompactRequest{Tenant: "acme", RunID: id}); !errors.Is(err, runtime.ErrInvalid) {
			t.Errorf("Compact(%q) = %v, want ErrInvalid", id, err)
		}
	}
	if err := rt.Compact(context.Background(), runtime.CompactRequest{Tenant: "acme", RunID: "r0123", By: "U1"}); err != nil {
		t.Fatalf("Compact for an unknown run = %v, want nil", err)
	}
	events := w.bus.rec.all()
	if len(events) != 1 {
		t.Fatalf("published %d events, want the one request", len(events))
	}
	want, _ := runtime.CompactChannel("acme", "r0123")
	if e := events[0]; e.Channel != want || e.Type != event.TypeCompact || e.Tenant != "acme" || e.Run != "r0123" {
		t.Errorf("request published on %s type %s tenant %q, want %s %s acme", e.Channel, e.Type, e.Tenant, want, event.TypeCompact)
	}
	if stop, _ := runtime.StopChannel("acme", "r0123"); want == stop {
		t.Error("requests to compact share a stream with requests to stop")
	}
}

// loopModel calls the lookup tool for a number of turns and then answers, and
// writes a summary when it is asked for one.
type loopModel struct {
	turns int

	mu    sync.Mutex
	calls int
}

func (m *loopModel) Stream(_ context.Context, req saigetypes.Request) (<-chan saigetypes.Delta, error) {
	var out []saigetypes.Delta
	first, _ := json.Marshal(req.Messages[:1])
	m.mu.Lock()
	switch {
	case strings.Contains(string(first), saigetypes.SummaryInstruction[:30]):
		out = agenttest.TextResponse("the earlier lookups, summarized")
	case m.calls < m.turns:
		m.calls++
		out = agenttest.ToolCallResponse(fmt.Sprintf("call-%d", m.calls), "lookup", map[string]any{"n": m.calls})
	default:
		out = agenttest.TextResponse("done")
	}
	m.mu.Unlock()
	ch := make(chan saigetypes.Delta, len(out))
	for _, d := range out {
		ch <- d
	}
	close(ch)
	return ch, nil
}

// heldLookup is a lookup that holds its third call until released, so a test
// can ask for a compaction while the turn is in its tool loop.
type heldLookup struct {
	running chan struct{}
	release chan struct{}

	mu    sync.Mutex
	calls int
}

func (h *heldLookup) Definition() saigetypes.ToolDef {
	return saigetypes.ToolDef{Name: "lookup", Description: "Look something up", Parameters: saigetypes.ParameterSchema{Type: "object"}}
}

func (h *heldLookup) Execute(ctx context.Context, _ map[string]any) (string, error) {
	h.mu.Lock()
	h.calls++
	n := h.calls
	h.mu.Unlock()
	switch {
	case n == 3:
		close(h.running)
		select {
		case <-h.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	case n > 3:
		// Later calls leave the request time to cross the bus.
		time.Sleep(10 * time.Millisecond)
	}
	return "row of results. row of results.", nil
}

// End to end: a host asks a running run to compact, the turn summarizes its
// older history before its next model call, and the run's stream says so,
// with who asked. The run then finishes as it would have.
func TestARequestedCompactionIsMadeByTheRunningTurnAndPublished(t *testing.T) {
	w := newWorld(t)
	lookup := &heldLookup{running: make(chan struct{}), release: make(chan struct{})}
	model := &loopModel{turns: 30}
	w.specs.edit("docs-bot", func(s *native.Spec) { s.Tools = []string{"lookup"} })
	exec := sandboxOf(t, w.specs, map[string]saigetypes.Provider{"docs-bot": model},
		runtime.WithCatalog(func(runtime.Request, func(event.Action) error) (native.Tools, error) {
			return native.StaticTools(lookup), nil
		}),
		runtime.WithAgentOptions(
			native.WithMaxIter(40),
			native.WithCompaction(native.Compaction{OnRequest: true, KeepTurns: 1}),
		))
	rt := w.runtime(t, exec, runtime.WithCompactRequests())
	w.work(t, rt, nil)

	runID := w.accept(t, rt, tag("docs-bot", "evt-1", "look it up"))
	select {
	case <-lookup.running:
	case <-time.After(20 * time.Second):
		t.Fatal("the turn never reached its third lookup")
	}
	if err := rt.Compact(context.Background(), runtime.CompactRequest{Tenant: "acme", RunID: runID, By: "U1"}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	close(lookup.release)

	if out := w.result(t, rt, runID); out.Text != "done" {
		t.Fatalf("Output.Text = %q, want the run to finish its loop", out.Text)
	}
	var compactions []event.Compaction
	for _, e := range w.bus.of(runID) {
		if e.Type != event.TypeCompacted {
			continue
		}
		body, err := decode[event.Compaction](e)
		if err != nil {
			t.Fatalf("decode %s: %v", e.Type, err)
		}
		compactions = append(compactions, body)
	}
	if len(compactions) != 1 {
		t.Fatalf("the run published %d %s events, want 1: %v", len(compactions), event.TypeCompacted, kindsOf(w.bus.of(runID)))
	}
	if c := compactions[0]; c.Trigger != native.TriggerRequested || c.By != "U1" || c.Summarized == 0 {
		t.Errorf("compaction = %+v, want a requested one by U1 that summarized something", c)
	}
}

// droppingBus ends the first subscription to a compact stream right after it
// has delivered one event, as a broker that loses a connection would, and
// says when the stream is subscribed to again.
type droppingBus struct {
	bus.Bus
	mu           sync.Mutex
	subscribed   int
	resubscribed chan struct{}
}

func (d *droppingBus) Subscribe(ctx context.Context, sub bus.Subscription) (bus.Stream, error) {
	s, err := d.Bus.Subscribe(ctx, sub)
	if err != nil || controlVerb(sub) != "compact" {
		return s, err
	}
	d.mu.Lock()
	d.subscribed++
	n := d.subscribed
	d.mu.Unlock()
	if n == 2 {
		close(d.resubscribed)
	}
	if n > 1 {
		return s, nil
	}
	return &droppingStream{Stream: s}, nil
}

type droppingStream struct {
	bus.Stream
	delivered bool
}

func (s *droppingStream) Recv(ctx context.Context) (event.Envelope, error) {
	if s.delivered {
		return event.Envelope{}, errors.New("connection lost")
	}
	e, err := s.Stream.Recv(ctx)
	s.delivered = err == nil
	return e, err
}

// counting is an executor that relays every request to compact it hears
// until the test lets it finish.
type counting struct {
	waiting chan struct{}
	heard   chan native.CompactRequest
	finish  chan struct{}
	once    sync.Once
}

func (c *counting) Execute(ctx context.Context, req runtime.Request, _ func(context.Context, event.Body) error) (runtime.Outcome, error) {
	signal := runtime.CompactSignal(ctx)
	c.once.Do(func() { close(c.waiting) })
	for {
		select {
		case got := <-signal:
			c.heard <- got
		case <-c.finish:
			return runtime.Outcome{Text: "done", Prompt: req.Text}, nil
		case <-ctx.Done():
			return runtime.Outcome{}, ctx.Err()
		}
	}
}

// A turn that loses its subscription listens again from where it left off: a
// request it already relayed is not relayed a second time, since each one
// costs a summary.
func TestARequestToCompactIsRelayedOnceAcrossAReconnect(t *testing.T) {
	broker := &droppingBus{Bus: newBroker(t), resubscribed: make(chan struct{})}
	w := newWorldOn(t, broker)
	exec := &counting{waiting: make(chan struct{}), heard: make(chan native.CompactRequest, 8), finish: make(chan struct{})}
	rt := w.runtime(t, exec, runtime.WithCompactRequests())
	w.work(t, rt, nil)

	runID := w.accept(t, rt, tag("docs-bot", "evt-1", "explain"))
	select {
	case <-exec.waiting:
	case <-time.After(20 * time.Second):
		t.Fatal("the turn never started")
	}
	if err := rt.Compact(context.Background(), runtime.CompactRequest{Tenant: "acme", RunID: runID, By: "U1"}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	select {
	case <-exec.heard:
	case <-time.After(20 * time.Second):
		t.Fatal("the turn never heard the request")
	}
	select {
	case <-broker.resubscribed:
	case <-time.After(20 * time.Second):
		t.Fatal("the turn never listened again after losing its subscription")
	}
	select {
	case again := <-exec.heard:
		t.Fatalf("the request was relayed again after the reconnect (by %q)", again.By)
	case <-time.After(300 * time.Millisecond):
	}
	close(exec.finish)
	w.result(t, rt, runID)
}

// controlVerb is the verb of the one control channel a subscription is to,
// or empty when it is to anything else.
func controlVerb(sub bus.Subscription) string {
	if len(sub.Channels) != 1 || !event.IsControl(sub.Channels[0]) {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(sub.Channels[0], event.ControlScheme+"://"), "/")
	if len(parts) != 3 {
		return ""
	}
	return parts[1]
}

// subscriptionCounter is a bus that counts the control subscriptions open at
// once, by verb, and the most of each it ever saw.
type subscriptionCounter struct {
	bus.Bus
	mu   sync.Mutex
	open map[string]int
	peak map[string]int
}

func (c *subscriptionCounter) Subscribe(ctx context.Context, sub bus.Subscription) (bus.Stream, error) {
	s, err := c.Bus.Subscribe(ctx, sub)
	verb := controlVerb(sub)
	if err != nil || verb == "" {
		return s, err
	}
	c.mu.Lock()
	c.open[verb]++
	c.peak[verb] = max(c.peak[verb], c.open[verb])
	c.mu.Unlock()
	return &countedStream{Stream: s, done: func() {
		c.mu.Lock()
		c.open[verb]--
		c.mu.Unlock()
	}}, nil
}

func (c *subscriptionCounter) peakOf(verb string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peak[verb]
}

type countedStream struct {
	bus.Stream
	once sync.Once
	done func()
}

func (s *countedStream) Close() error {
	s.once.Do(s.done)
	return s.Stream.Close()
}

// held is an executor whose turns all wait until the test releases them.
type held struct {
	started chan struct{}
	release chan struct{}
}

func (h *held) Execute(ctx context.Context, req runtime.Request, _ func(context.Context, event.Body) error) (runtime.Outcome, error) {
	h.started <- struct{}{}
	select {
	case <-h.release:
		return runtime.Outcome{Text: "done", Prompt: req.Text}, nil
	case <-ctx.Done():
		return runtime.Outcome{}, ctx.Err()
	}
}

// What a running turn holds on the bus for its whole length: a subscription
// for stop requests, and one for requests to compact only when the runtime
// takes them. A runtime that does not take them holds no second subscription
// and refuses Compact.
func TestARunningTurnListensForCompactionOnlyWhenTheRuntimeTakesRequests(t *testing.T) {
	const runs = 3
	for _, tc := range []struct {
		name        string
		opts        []runtime.Option
		wantCompact int
	}{
		{"requests are not taken", nil, 0},
		{"requests are taken", []runtime.Option{runtime.WithCompactRequests()}, runs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counter := &subscriptionCounter{Bus: newBroker(t), open: map[string]int{}, peak: map[string]int{}}
			w := newWorldOn(t, counter)
			w.concurrency = runs
			exec := &held{started: make(chan struct{}, runs), release: make(chan struct{})}
			rt := w.runtime(t, exec, tc.opts...)
			w.work(t, rt, nil)

			ids := make([]string, runs)
			for i := range ids {
				ids[i] = w.accept(t, rt, tag("docs-bot", fmt.Sprintf("evt-%d", i), "explain"))
			}
			for range runs {
				select {
				case <-exec.started:
				case <-time.After(20 * time.Second):
					t.Fatal("not every turn started")
				}
			}
			err := rt.Compact(context.Background(), runtime.CompactRequest{Tenant: "acme", RunID: ids[0]})
			if tc.wantCompact == 0 && !errors.Is(err, runtime.ErrCompactUnavailable) {
				t.Errorf("Compact = %v, want ErrCompactUnavailable from a runtime that does not take requests", err)
			}
			if tc.wantCompact > 0 && err != nil {
				t.Errorf("Compact = %v", err)
			}
			// The listeners subscribe as each turn begins; give the last
			// one a moment to be counted.
			deadline := time.Now().Add(5 * time.Second)
			for counter.peakOf("stop") < runs && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			for counter.peakOf("compact") < tc.wantCompact && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			close(exec.release)
			for _, id := range ids {
				w.result(t, rt, id)
			}
			if got := counter.peakOf("stop"); got != runs {
				t.Errorf("%d stop subscriptions were open at once, want one per running turn (%d)", got, runs)
			}
			if got := counter.peakOf("compact"); got != tc.wantCompact {
				t.Errorf("%d compact subscriptions were open at once, want %d", got, tc.wantCompact)
			}
		})
	}
}
