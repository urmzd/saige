package sink_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/connectors/internal/render"
	"github.com/urmzd/saige/flow/connectors/internal/sink"
	"github.com/urmzd/saige/flow/event"
)

// fakeSurface is a complete in-memory surface: it holds a body per handle, just
// as Slack or GitHub holds a body per message, so a test can assert on how many
// messages exist as well as on what they say.
type fakeSurface struct {
	mu      sync.Mutex
	bodies  map[string]string
	targets map[string]address.Address
	ops     []string
	next    int
	failN   int // fail the next N calls
}

func newFakeSurface() *fakeSurface {
	return &fakeSurface{bodies: map[string]string{}, targets: map[string]address.Address{}}
}

var errSurface = errors.New("surface unavailable")

func (f *fakeSurface) Create(_ context.Context, target address.Address, body string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failN > 0 {
		f.failN--
		f.ops = append(f.ops, "create!")
		return "", errSurface
	}
	f.next++
	handle := "m" + string(rune('0'+f.next))
	f.bodies[handle] = body
	f.targets[handle] = target
	f.ops = append(f.ops, "create")
	return handle, nil
}

func (f *fakeSurface) Update(_ context.Context, _ address.Address, handle, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failN > 0 {
		f.failN--
		f.ops = append(f.ops, "update!")
		return errSurface
	}
	if _, ok := f.bodies[handle]; !ok {
		return errors.New("no such message")
	}
	f.bodies[handle] = body
	f.ops = append(f.ops, "update")
	return nil
}

func (f *fakeSurface) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeSurface) body(handle string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[handle]
}

func (f *fakeSurface) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

// plain is the simplest possible Markup: it spells everything as itself, so a
// layout assertion reads as the text a human would see.
type plain struct{}

func (plain) Bold(s string) string         { return s }
func (plain) Italic(s string) string       { return s }
func (plain) Code(s string) string         { return s }
func (plain) Link(text, uri string) string { return text + " <" + uri + ">" }
func (plain) Item(s string) string         { return "- " + s }

func layout(v render.View) string { return sink.Layout(plain{}, v) }

var target = address.MustParse("slack://T01/C02?thread=1699.1")

// channel is the channel ev publishes on.
const channel = "slack://T01/C02"

func ev(seq uint64, typ, data string) event.Envelope {
	if data == "" {
		data = "{}"
	}
	body, err := event.Decode(typ, json.RawMessage(data))
	if err != nil {
		panic(err)
	}
	return event.Envelope{
		ID:      fmt.Sprintf("run_1:%d", seq),
		Source:  "fiber://run/run_1",
		Type:    typ,
		Channel: channel,
		Tenant:  "acme",
		Run:     "run_1",
		Seq:     seq,
		Agent:   "docs-bot",
		Time:    time.Unix(1700000000, 0).Add(time.Duration(seq) * time.Second),
		Data:    body,
	}
}

func text(seq uint64, s string) event.Envelope {
	return ev(seq, event.TypeText, `{"text":"`+s+`"}`)
}

func TestDeliveringTheSameEventTwiceLeavesOneMessageEdited(t *testing.T) {
	surface := newFakeSurface()
	eng := sink.New(surface, layout, sink.WithInterval(0))
	ctx := context.Background()

	events := []event.Envelope{
		ev(1, event.TypeAccepted, ""),
		text(2, "hello "),
		text(3, "world"),
		ev(4, event.TypeCompleted, ""),
	}
	for _, e := range events {
		for range 3 { // every event delivered three times
			if err := eng.Deliver(ctx, target, e); err != nil {
				t.Fatalf("Deliver(seq %d): %v", e.Seq, err)
			}
		}
	}

	if got := surface.count(); got != 1 {
		t.Fatalf("surface holds %d messages, want exactly 1", got)
	}
	handle, ok := eng.Handle("run_1", channel)
	if !ok {
		t.Fatal("engine forgot the message handle")
	}
	if body := surface.body(handle); !strings.Contains(body, "hello world") {
		t.Errorf("body = %q, want the assembled answer", body)
	}
	// Redelivery must not repeat text either: exactly one "hello world".
	if got := strings.Count(surface.body(handle), "hello world"); got != 1 {
		t.Errorf("body contains the answer %d times, want 1", got)
	}
	// One create, and no update for an event that changed nothing.
	calls := surface.calls()
	if calls[0] != "create" {
		t.Errorf("first call = %q, want create", calls[0])
	}
	for _, c := range calls[1:] {
		if c != "update" {
			t.Errorf("calls = %v, want one create followed by updates", calls)
			break
		}
	}
	if len(calls) > len(events) {
		t.Errorf("calls = %v, want at most one write per distinct event", calls)
	}
}

func TestOutOfOrderDeliveryNeverRendersStaleTextOverFreshText(t *testing.T) {
	surface := newFakeSurface()
	eng := sink.New(surface, layout, sink.WithInterval(0))
	ctx := context.Background()

	// The tail of the answer arrives before its head, which is what a
	// resubscribe after a slow-consumer disconnect can produce.
	for _, e := range []event.Envelope{
		text(3, "world"),
		text(2, "hello "),
		ev(4, event.TypeCompleted, ""),
		text(2, "hello "), // and then the head is redelivered
	} {
		if err := eng.Deliver(ctx, target, e); err != nil {
			t.Fatalf("Deliver(seq %d): %v", e.Seq, err)
		}
	}

	handle, _ := eng.Handle("run_1", channel)
	body := surface.body(handle)
	if !strings.Contains(body, "hello world") {
		t.Errorf("body = %q, want %q assembled in sequence order", body, "hello world")
	}
	if strings.Contains(body, "worldhello") {
		t.Errorf("body = %q, stale fragment rendered after the fresh one", body)
	}
}

func TestFirstAndTerminalEventsJumpTheCoalescingInterval(t *testing.T) {
	surface := newFakeSurface()
	now := time.Unix(1700000000, 0)
	eng := sink.New(surface, layout,
		sink.WithInterval(time.Second),
		sink.WithClock(func() time.Time { return now }),
	)
	ctx := context.Background()

	// The first event is acknowledged immediately: a human is waiting.
	if err := eng.Deliver(ctx, target, ev(1, event.TypeStarted, "")); err != nil {
		t.Fatal(err)
	}
	if got := len(surface.calls()); got != 1 {
		t.Fatalf("after the first event: %d calls, want 1", got)
	}

	// Deltas inside the interval are buffered, not written.
	for seq := uint64(2); seq < 20; seq++ {
		if err := eng.Deliver(ctx, target, text(seq, "x")); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(surface.calls()); got != 1 {
		t.Errorf("inside the interval: %d calls, want the deltas coalesced into 0", got)
	}

	// Past the interval, the next delta carries everything buffered.
	now = now.Add(2 * time.Second)
	if err := eng.Deliver(ctx, target, text(20, "y")); err != nil {
		t.Fatal(err)
	}
	if got := len(surface.calls()); got != 2 {
		t.Errorf("after the interval: %d calls, want 2", got)
	}
	handle, _ := eng.Handle("run_1", channel)
	if body := surface.body(handle); !strings.Contains(body, strings.Repeat("x", 18)+"y") {
		t.Errorf("body = %q, want every buffered delta", body)
	}

	// A terminal event is never left buffered, however recently we wrote.
	if err := eng.Deliver(ctx, target, ev(21, event.TypeCompleted, "")); err != nil {
		t.Fatal(err)
	}
	if got := len(surface.calls()); got != 3 {
		t.Errorf("after completion: %d calls, want the final state written", got)
	}
	if body := surface.body(handle); !strings.Contains(body, "(done)") {
		t.Errorf("body = %q, want the terminal status", body)
	}
}

func TestFlushWritesWhatCoalescingIsStillHolding(t *testing.T) {
	surface := newFakeSurface()
	now := time.Unix(1700000000, 0)
	eng := sink.New(surface, layout,
		sink.WithInterval(time.Hour),
		sink.WithClock(func() time.Time { return now }),
	)
	ctx := context.Background()

	if err := eng.Deliver(ctx, target, ev(1, event.TypeStarted, "")); err != nil {
		t.Fatal(err)
	}
	if err := eng.Deliver(ctx, target, text(2, "buffered")); err != nil {
		t.Fatal(err)
	}
	handle, _ := eng.Handle("run_1", channel)
	if strings.Contains(surface.body(handle), "buffered") {
		t.Fatal("the delta was written despite the interval")
	}

	if err := eng.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if !strings.Contains(surface.body(handle), "buffered") {
		t.Error("Flush did not write the buffered delta")
	}
	// A second Flush has nothing to do and must not touch the surface.
	before := len(surface.calls())
	if err := eng.Flush(ctx); err != nil {
		t.Fatalf("second Flush: %v", err)
	}
	if got := len(surface.calls()); got != before {
		t.Errorf("second Flush made %d extra calls, want 0", got-before)
	}
}

func TestAFailedWriteIsRetriedRatherThanLosingTheAnswer(t *testing.T) {
	surface := newFakeSurface()
	eng := sink.New(surface, layout, sink.WithInterval(0))
	ctx := context.Background()

	surface.failN = 1
	err := eng.Deliver(ctx, target, text(1, "first"))
	if !errors.Is(err, errSurface) {
		t.Fatalf("Deliver error = %v, want the surface error", err)
	}
	if got := surface.count(); got != 0 {
		t.Fatalf("surface holds %d messages after a failed create, want 0", got)
	}

	// The next event retries the create, and the first fragment is not lost.
	if err := eng.Deliver(ctx, target, text(2, " second")); err != nil {
		t.Fatalf("Deliver after failure: %v", err)
	}
	handle, _ := eng.Handle("run_1", channel)
	if body := surface.body(handle); !strings.Contains(body, "first second") {
		t.Errorf("body = %q, want both fragments", body)
	}
	if got := surface.count(); got != 1 {
		t.Errorf("surface holds %d messages, want 1", got)
	}

	// A failed update leaves the run dirty, so Flush can complete it.
	surface.failN = 1
	if err := eng.Deliver(ctx, target, ev(3, event.TypeCompleted, "")); !errors.Is(err, errSurface) {
		t.Fatalf("Deliver error = %v, want the surface error", err)
	}
	if err := eng.Flush(ctx); err != nil {
		t.Fatalf("Flush after a failed update: %v", err)
	}
	if body := surface.body(handle); !strings.Contains(body, "(done)") {
		t.Errorf("body = %q, want the retried terminal state", body)
	}
}

func TestOneRunOnTwoChannelsOwnsOneMessageOnEach(t *testing.T) {
	surface := newFakeSurface()
	eng := sink.New(surface, layout, sink.WithInterval(0))
	ctx := context.Background()

	other := address.MustParse("slack://T01/C99")
	for _, tgt := range []address.Address{target, other} {
		for _, e := range []event.Envelope{text(1, "hi"), ev(2, event.TypeCompleted, "")} {
			e.Channel = tgt.String()
			if err := eng.Deliver(ctx, tgt, e); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := surface.count(); got != 2 {
		t.Fatalf("surface holds %d messages, want one per channel", got)
	}
	h1, _ := eng.Handle("run_1", target.String())
	h2, _ := eng.Handle("run_1", other.String())
	if h1 == h2 {
		t.Errorf("both channels share handle %q", h1)
	}
}

func TestAnUnrenderableEventCostsNoRoundTrip(t *testing.T) {
	surface := newFakeSurface()
	eng := sink.New(surface, layout, sink.WithInterval(0))
	ctx := context.Background()

	if err := eng.Deliver(ctx, target, ev(1, "run.delta.unheard-of.v1", `{"x":1}`)); err != nil {
		t.Fatal(err)
	}
	if got := len(surface.calls()); got != 0 {
		t.Errorf("%d calls for an unrenderable event, want 0", got)
	}
	if _, ok := eng.Handle("run_1", channel); ok {
		t.Error("a message was created for an event with nothing to show")
	}
}

func TestConcurrentDeliveriesForOneRunStillCreateOneMessage(t *testing.T) {
	surface := newFakeSurface()
	eng := sink.New(surface, layout, sink.WithInterval(0))
	ctx := context.Background()

	var wg sync.WaitGroup
	for seq := uint64(1); seq <= 32; seq++ {
		wg.Add(1)
		go func(seq uint64) {
			defer wg.Done()
			if err := eng.Deliver(ctx, target, text(seq, "x")); err != nil {
				t.Errorf("Deliver(seq %d): %v", seq, err)
			}
		}(seq)
	}
	wg.Wait()

	if got := surface.count(); got != 1 {
		t.Fatalf("surface holds %d messages, want 1", got)
	}
	handle, _ := eng.Handle("run_1", channel)
	if got := strings.Count(surface.body(handle), "x"); got != 32 {
		t.Errorf("body holds %d fragments, want 32", got)
	}
}

func TestEvictionIsBoundedAndKeepsTheNewestRuns(t *testing.T) {
	surface := newFakeSurface()
	eng := sink.New(surface, layout, sink.WithInterval(0), sink.WithMaxRuns(2))
	ctx := context.Background()

	for _, run := range []string{"a", "b", "c"} {
		e := text(1, "hi")
		e.Run = run
		if err := eng.Deliver(ctx, target, e); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := eng.Handle("a", channel); ok {
		t.Error("the oldest run was not evicted")
	}
	for _, run := range []string{"b", "c"} {
		if _, ok := eng.Handle(run, channel); !ok {
			t.Errorf("run %q was evicted before the oldest", run)
		}
	}
}

func TestLayoutOmitsEmptySectionsAndNumbersFootnotes(t *testing.T) {
	d := render.New()
	d.Apply(ev(1, event.TypeStarted, ""))
	got := layout(d.View())
	if got != "docs-bot (working)" {
		t.Errorf("layout of a bare run = %q, want just the header", got)
	}

	d.Apply(ev(2, event.TypeToolCall, `{"id":"t1","name":"search","arguments":{"q":"x"}}`))
	d.Apply(ev(3, event.TypeToolDone, `{"id":"t1","result":"3 hits"}`))
	d.Apply(text(4, "the answer"))
	d.Apply(ev(5, event.TypeCitation, `{"source":{"document_title":"Design","source_uri":"https://example.com/d"}}`))
	d.Apply(ev(6, event.TypeAction, `{"summary":"commented","address":"github://urmzd/fiber/issues/42"}`))
	d.Apply(ev(7, event.TypeCompleted, ""))

	want := strings.Join([]string{
		"docs-bot (done)",
		"",
		`- search {"q":"x"} -> 3 hits`,
		"",
		"the answer",
		"",
		"[1] Design <https://example.com/d>",
		"",
		"- changed: commented (github://urmzd/fiber/issues/42 <github://urmzd/fiber/issues/42>)",
	}, "\n")
	if got := layout(d.View()); got != want {
		t.Errorf("layout =\n%q\nwant\n%q", got, want)
	}
}

func TestClampKeepsTheHeadAndSaysItTruncated(t *testing.T) {
	body := strings.Repeat("a", 100)
	got := sink.Clamp(body, 40)
	if len(got) > 40 {
		t.Errorf("Clamp produced %d bytes, want at most 40", len(got))
	}
	if !strings.HasPrefix(got, "aaaa") || !strings.HasSuffix(got, "[truncated]") {
		t.Errorf("Clamp = %q, want the head plus a marker", got)
	}
	if got := sink.Clamp(body, 0); got != body {
		t.Error("Clamp with no limit changed the body")
	}
	// A multi-byte rune must not be split.
	if got := sink.Clamp(strings.Repeat("é", 20), 15); !isValidUTF8(got) {
		t.Errorf("Clamp = %q, which is not valid UTF-8", got)
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// A stopped run is written at once, like any terminal event, and the message
// says it was stopped: in the header, and again under the partial answer with
// the reason, so nobody reads half an answer as a whole one or as an error.
func TestAStoppedRunIsFlushedAndRenderedAsStopped(t *testing.T) {
	surface := newFakeSurface()
	now := time.Unix(1700000000, 0)
	eng := sink.New(surface, layout,
		sink.WithInterval(time.Hour),
		sink.WithClock(func() time.Time { return now }),
	)
	ctx := context.Background()
	if err := eng.Deliver(ctx, target, ev(1, event.TypeStarted, "")); err != nil {
		t.Fatal(err)
	}
	if err := eng.Deliver(ctx, target, text(2, "Half an answer")); err != nil {
		t.Fatal(err)
	}
	stopped := ev(3, event.TypeStopped, `{"text":"Half an answer","reason":"Stopped\nby a user","by":"slack:U1"}`)
	if err := eng.Deliver(ctx, target, stopped); err != nil {
		t.Fatal(err)
	}
	// Redelivery edits nothing and posts nothing.
	if err := eng.Deliver(ctx, target, stopped); err != nil {
		t.Fatal(err)
	}

	if got := surface.count(); got != 1 {
		t.Fatalf("%d messages, want the one the run owns", got)
	}
	if got := len(surface.calls()); got != 2 {
		t.Fatalf("%d writes, want the first event and the stop: %v", got, surface.calls())
	}
	handle, _ := eng.Handle("run_1", channel)
	want := "docs-bot (stopped)\n\nHalf an answer\n\nstopped: Stopped by a user"
	if body := surface.body(handle); body != want {
		t.Fatalf("body = %q\nwant   %q", body, want)
	}
	for _, wrong := range []string{"(done)", "(working)", "failed", "slack:U1"} {
		if strings.Contains(surface.body(handle), wrong) {
			t.Errorf("a stopped run's message contains %q", wrong)
		}
	}
}

// With nothing said and no reason given, a stopped run still renders as
// stopped rather than as an empty message or one that looks unfinished.
func TestAStoppedRunWithNothingToShowStillSaysItStopped(t *testing.T) {
	d := render.New()
	d.Apply(ev(1, event.TypeStarted, ""))
	d.Apply(ev(2, event.TypeStopped, `{}`))
	if got, want := layout(d.View()), "docs-bot (stopped)\n\nstopped"; got != want {
		t.Fatalf("layout = %q, want %q", got, want)
	}
	if got := sink.StatusLabel(render.StatusStopped); got != "stopped" {
		t.Errorf("StatusLabel = %q", got)
	}
	escaped := sink.MapText(render.View{Reason: "a<b"}, func(s string) string { return strings.ReplaceAll(s, "<", "&lt;") })
	if escaped.Reason != "a&lt;b" {
		t.Errorf("the reason is not escaped with the rest of the prose: %q", escaped.Reason)
	}
}
