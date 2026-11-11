package bus_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urmzd/saige/flow/bus"
	"github.com/urmzd/saige/flow/bus/bustest"
	"github.com/urmzd/saige/flow/event"
)

func newMemory(_ *testing.T, cfg bustest.Config) bus.Bus {
	return bus.NewMemory(bus.WithRetention(cfg.Retention), bus.WithBacklog(cfg.Backlog), bus.WithLease(cfg.Lease))
}

func TestMemoryConformance(t *testing.T) { bustest.Run(t, newMemory) }

func TestRootDoesNotSeeControl(t *testing.T) { bustest.RootDoesNotSeeControl(t, newMemory) }

func TestEmptySubscriptionMatchesNothing(t *testing.T) {
	bustest.EmptySubscriptionMatchesNothing(t, newMemory)
}

// The conformance suite tolerates either overflow policy, because a backend
// that can push back on its own storage is entitled to. Memory cannot: its
// publisher holds the lock, so overflow must terminate the subscriber, and it
// must say so.
func TestMemoryOverflowTerminatesTheSubscriberWithItsBufferDrained(t *testing.T) {
	const backlog = 4
	b := bus.NewMemory(bus.WithRetention(64), bus.WithBacklog(backlog))

	s, err := b.Subscribe(t.Context(), all)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = s.Close() }()

	for seq := uint64(1); seq <= backlog+5; seq++ {
		if err := b.Publish(t.Context(), runEvent("run_1", seq)); err != nil {
			t.Fatalf("Publish %d: %v", seq, err)
		}
	}

	// Everything that made it into the buffer is handed over first: it is a
	// contiguous prefix, and it carries the consumer's cursor as far forward
	// as it can go before it has to resubscribe.
	for seq := uint64(1); seq <= backlog; seq++ {
		e, err := s.Recv(t.Context())
		if err != nil {
			t.Fatalf("Recv %d: %v", seq, err)
		}
		if e.Seq != seq {
			t.Fatalf("Recv %d returned sequence %d", seq, e.Seq)
		}
	}
	if _, err := s.Recv(t.Context()); !errors.Is(err, bus.ErrSlowConsumer) {
		t.Fatalf("Recv after overflow = %v, want bus.ErrSlowConsumer", err)
	}
	// The verdict is sticky: the subscriber does not silently resume mid-gap.
	if err := b.Publish(t.Context(), runEvent("run_1", backlog+6)); err != nil {
		t.Fatalf("Publish after overflow: %v", err)
	}
	if _, err := s.Recv(t.Context()); !errors.Is(err, bus.ErrSlowConsumer) {
		t.Fatalf("Recv after a later publish = %v, want bus.ErrSlowConsumer", err)
	}
}

// A stalled subscriber must not keep a publisher waiting even for as long as
// one scheduling hiccup, and must not stop its peers from being served.
func TestMemoryPublishNeverWaitsOnAStalledSubscriber(t *testing.T) {
	b := bus.NewMemory(bus.WithRetention(4096), bus.WithBacklog(1))

	for range 8 {
		s, err := b.Subscribe(t.Context(), all)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer func() { _ = s.Close() }()
	}

	started := time.Now()
	for seq := uint64(1); seq <= 2000; seq++ {
		if err := b.Publish(t.Context(), runEvent("run_1", seq)); err != nil {
			t.Fatalf("Publish %d: %v", seq, err)
		}
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("2000 publishes into 8 stalled subscribers took %v", took)
	}
}

// Runs are evicted coldest-first once the bus is holding more than it was
// configured for: an in-process bus that remembered every run it ever carried
// would be a leak with a retention policy on top.
func TestMemoryEvictsTheColdestRunOverTheRunBound(t *testing.T) {
	b := bus.NewMemory(bus.WithRetention(4), bus.WithBacklog(64), bus.WithMaxRuns(2))

	publish := func(runID string, seq uint64) {
		t.Helper()
		if err := b.Publish(t.Context(), runEvent(runID, seq)); err != nil {
			t.Fatalf("Publish(%s, %d): %v", runID, seq, err)
		}
	}
	publish("run_cold", 1)
	publish("run_warm", 1)
	publish("run_warm", 2) // run_warm is now the most recently published
	publish("run_hot", 1)  // over the bound: run_cold is dropped

	s, err := b.Subscribe(t.Context(), all)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	seen := map[string]int{}
	for range 3 {
		e, err := s.Recv(ctx)
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		seen[e.Run]++
	}
	if seen["run_cold"] != 0 {
		t.Errorf("run_cold survived eviction: %v", seen)
	}
	if seen["run_warm"] != 2 || seen["run_hot"] != 1 {
		t.Errorf("retained runs = %v, want run_warm twice and run_hot once", seen)
	}
}

// all is every run event of the tenant the helpers publish in.
var all = bus.Subscription{Tenant: "acme", Channels: []string{"slack://"}, Types: []string{"run"}}

func runEvent(runID string, seq uint64) event.Envelope {
	return event.Envelope{
		ID:      fmt.Sprintf("%s#%d", runID, seq),
		Source:  "fiber://run/" + runID,
		Type:    event.TypeText,
		Channel: "slack://T01/C02",
		Tenant:  "acme",
		Run:     runID,
		Seq:     seq,
		Agent:   "docs-bot",
		Data:    event.Text{Text: "hi"},
	}
}
