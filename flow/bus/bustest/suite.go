// Package bustest is the conformance suite every bus backend must pass. Run
// exercises the whole contract: channel and type prefixes, default deny,
// control and tenant isolation, opt-in types, sequence dedupe and id dedupe,
// the hop limit, resume, the catch-up/live seam under concurrent publish,
// slow-consumer isolation, resource release and retention, and consumer
// groups: one delivery per group, takeover when a member dies, and resume
// from the committed cursor.
//
// Two properties are deliberately not asserted, because the bus does not
// promise them: ordering between different runs or channels, and which events
// survive retention beyond "the newest ones".
package bustest

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/flow/bus"
	"github.com/urmzd/saige/flow/event"
)

const (
	// waitLong bounds things that must happen.
	waitLong = 10 * time.Second
	// waitIdle bounds a probe that must find nothing. It has to outlast a
	// backend's poll interval.
	waitIdle = 500 * time.Millisecond
	// waitPublish bounds one Publish call.
	waitPublish = 2 * time.Second
)

// Tenant is the tenant every event in the suite belongs to, unless a test
// says otherwise.
const Tenant = "acme"

// Channel is the channel the suite's run events are published on.
const Channel = "slack://T01/C02"

// Config is the configuration a factory must apply to the bus it returns.
type Config struct {
	// Retention is how many events each run keeps.
	Retention int
	// Backlog is the per-subscriber buffer depth.
	Backlog int
	// Lease is how long a consumer group member holds a tenant's lease
	// without renewing it (bus.WithLease). Zero leaves the default.
	Lease time.Duration
}

// Factory returns a fresh, empty bus configured per cfg. No bus may see
// another's events.
type Factory func(t *testing.T, cfg Config) bus.Bus

// Run exercises a Bus implementation against the full contract.
func Run(t *testing.T, newBus Factory) {
	t.Run("EveryConsumerReceivesEveryEventInSequenceOrder", func(t *testing.T) {
		b := newBus(t, Config{Retention: 64, Backlog: 64})

		// One subscriber at each level of the channel, and one on the run.
		streams := map[string]bus.Stream{}
		for _, at := range []string{"slack://", "slack://T01", Channel} {
			streams[at] = subscribe(t, b, on(at))
		}
		byRun := on("slack://")
		byRun.Run = "run_1"
		streams["run"] = subscribe(t, b, byRun)

		publish(t, b, text(Channel, "run_1", 1), text(Channel, "run_1", 2), text(Channel, "run_1", 3))

		for at, s := range streams {
			got := recvN(t, s, 3)
			if diff := seqsOf(got); !slices.Equal(diff, []uint64{1, 2, 3}) {
				t.Errorf("subscriber on %s got sequences %v, want [1 2 3]", at, diff)
			}
			if body, ok := got[0].Data.(event.Text); !ok || body.Text != "run_1#1" {
				t.Errorf("subscriber on %s got data %#v, want the text run_1#1", at, got[0].Data)
			}
			if got[0].Time.IsZero() {
				t.Errorf("subscriber on %s got an event with no time", at)
			}
		}
	})

	t.Run("ChannelPrefixIsSegmentAware", func(t *testing.T) {
		b := newBus(t, Config{Retention: 64, Backlog: 64})
		s := subscribe(t, b, on("slack://T01/C02"))

		publish(t, b,
			text("slack://T01/C02", "run_1", 1),
			text("slack://T01/C02?thread=1699.001", "run_2", 1),
			text("slack://T01/C023", "run_3", 1),
			text("github://acme/handbook", "run_4", 1),
		)

		got := labelsOf(recvN(t, s, 2))
		slices.Sort(got)
		if want := []string{"run_1#1", "run_2#1"}; !slices.Equal(got, want) {
			t.Errorf("received %v, want %v", got, want)
		}
		recvIdle(t, s)
	})

	t.Run("TypePrefixNarrows", func(t *testing.T) {
		corpus := []event.Envelope{
			text(Channel, "run_a", 1),
			ev(Channel, "run_a", 2, event.Citation{Label: "run_a#2"}),
			ev(Channel, "run_a", 3, event.Completed{Text: "run_a#3"}),
			text(Channel, "run_b", 1),
			ev(Channel, "run_b", 2, event.Citation{Label: "run_b#2"}),
		}
		cases := []struct {
			name  string
			types []string
			want  []string
		}{
			{"every run event", []string{"run"}, []string{"run_a#1", "run_a#2", "run_a#3", "run_b#1", "run_b#2"}},
			{"family", []string{"run.delta"}, []string{"run_a#1", "run_a#2", "run_b#1", "run_b#2"}},
			{"exact type", []string{event.TypeCitation}, []string{"run_a#2", "run_b#2"}},
			{"two types", []string{event.TypeText, event.TypeCompleted}, []string{"run_a#1", "run_a#3", "run_b#1"}},
			{"segment aware", []string{"run.delta.tex"}, nil},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				b := newBus(t, Config{Retention: 64, Backlog: 64})
				publish(t, b, corpus...)
				sub := on(Channel)
				sub.Types = tc.types
				s := subscribe(t, b, sub)
				got := labelsOf(recvN(t, s, len(tc.want)))
				slices.Sort(got)
				if !slices.Equal(got, tc.want) && (len(got) != 0 || len(tc.want) != 0) {
					t.Errorf("received %v, want %v", got, tc.want)
				}
				recvIdle(t, s)
			})
		}
	})

	t.Run("EmptySubscriptionMatchesNothing", func(t *testing.T) { EmptySubscriptionMatchesNothing(t, newBus) })
	t.Run("RootDoesNotSeeControl", func(t *testing.T) { RootDoesNotSeeControl(t, newBus) })

	t.Run("TenantsAreIsolated", func(t *testing.T) {
		b := newBus(t, Config{Retention: 64, Backlog: 64})
		mine := subscribe(t, b, on("slack://"))
		theirs := on("slack://")
		theirs.Tenant = "globex"
		other := subscribe(t, b, theirs)

		foreign := text(Channel, "run_2", 1)
		foreign.Tenant = "globex"
		publish(t, b, text(Channel, "run_1", 1), foreign)

		if got := labelsOf(recvN(t, mine, 1)); got[0] != "run_1#1" {
			t.Errorf("acme received %v", got)
		}
		recvIdle(t, mine)
		if got := labelsOf(recvN(t, other, 1)); got[0] != "run_2#1" {
			t.Errorf("globex received %v", got)
		}
		recvIdle(t, other)
	})

	t.Run("OptInTypesNeedTheirFamilyNamed", func(t *testing.T) {
		b := newBus(t, Config{Retention: 64, Backlog: 64})
		publish(t, b,
			text(Channel, "run_1", 1),
			ev(Channel, "run_1", 2, event.UsageCall{AccountingID: "run_1#2"}),
			ev(Channel, "run_1", 3, event.Compaction{By: "run_1#3"}),
		)

		everything := subscribe(t, b, on(Channel))
		if got := labelsOf(recvN(t, everything, 1)); got[0] != "run_1#1" {
			t.Errorf("a run subscription received %v", got)
		}
		recvIdle(t, everything)

		usage := on(Channel)
		usage.Types = []string{"run.usage"}
		s := subscribe(t, b, usage)
		if got := recvN(t, s, 1); got[0].Type != event.TypeUsageCall {
			t.Errorf("a usage subscription received %s", got[0].Type)
		}
		recvIdle(t, s)
	})

	t.Run("FromResumesAfterASequenceWithinEachRun", func(t *testing.T) {
		b := newBus(t, Config{Retention: 64, Backlog: 64})
		publish(t, b,
			text(Channel, "run_a", 1),
			text(Channel, "run_a", 2),
			text(Channel, "run_b", 1),
			text(Channel, "run_b", 2),
		)
		sub := on(Channel)
		sub.From = 1
		s := subscribe(t, b, sub)
		got := labelsOf(recvN(t, s, 2))
		slices.Sort(got)
		if want := []string{"run_a#2", "run_b#2"}; !slices.Equal(got, want) {
			t.Errorf("received %v, want %v: From resumes within each run", got, want)
		}
		recvIdle(t, s)
	})

	t.Run("CatchUpJoinsTheLiveTailWithoutGapOrDuplicate", func(t *testing.T) {
		const (
			history = 100
			last    = 300
		)
		b := newBus(t, Config{Retention: 2048, Backlog: 2048})

		early := subscribe(t, b, on(Channel))
		if err := publishRange(t.Context(), b, "run_1", 1, history); err != nil {
			t.Fatal(err)
		}
		publishErr := make(chan error, 1)
		go func() {
			publishErr <- publishRange(context.WithoutCancel(t.Context()), b, "run_1", history+1, last)
		}()
		froms := []uint64{50, history, history + 40}
		mids := make([]bus.Stream, len(froms))
		for i, from := range froms {
			sub := on(Channel)
			sub.From = from
			mids[i] = subscribe(t, b, sub)
		}
		if err := <-publishErr; err != nil {
			t.Fatal(err)
		}
		wantContiguous(t, seqsOf(recvN(t, early, last)), 1, last)
		for i, from := range froms {
			wantContiguous(t, seqsOf(recvN(t, mids[i], int(last-from))), from+1, last)
		}
	})

	t.Run("ReplayingASequenceIsNotADuplicate", func(t *testing.T) {
		b := newBus(t, Config{Retention: 64, Backlog: 64})
		publish(t, b, text(Channel, "run_1", 1), text(Channel, "run_1", 2), text(Channel, "run_1", 3))

		tampered := text(Channel, "run_1", 2)
		tampered.Data = event.Text{Text: "tampered"}
		publish(t, b, text(Channel, "run_1", 1), tampered, text(Channel, "run_1", 3), text(Channel, "run_1", 4))

		sub := on(Channel)
		sub.Run = "run_1"
		s := subscribe(t, b, sub)
		got := recvN(t, s, 4)
		wantContiguous(t, seqsOf(got), 1, 4)
		if body, _ := got[1].Data.(event.Text); body.Text != "run_1#2" {
			t.Errorf("sequence 2 text = %q, want the original %q", body.Text, "run_1#2")
		}
		recvIdle(t, s)
	})

	t.Run("OtherEventsAreDeduplicatedByID", func(t *testing.T) {
		b := newBus(t, Config{Retention: 64, Backlog: 64})
		sub := on(Channel)
		sub.Types = []string{"message"}
		s := subscribe(t, b, sub)

		first := message(Channel, "Ev1", "hello")
		again := message(Channel, "Ev1", "hello again")
		publish(t, b, first, again, message(Channel, "Ev2", "second"))

		got := recvN(t, s, 2)
		if got[0].ID != "Ev1" || got[1].ID != "Ev2" {
			t.Fatalf("received %s, %s; want Ev1, Ev2", got[0].ID, got[1].ID)
		}
		if body := got[0].Data.(event.MessagePosted); body.Text != "hello" {
			t.Errorf("Ev1 text = %q, want the first publish", body.Text)
		}
		recvIdle(t, s)

		// The same id in another tenant is another event.
		foreign := message(Channel, "Ev1", "theirs")
		foreign.Tenant = "globex"
		publish(t, b, foreign)
		recvIdle(t, s)
	})

	t.Run("SlowConsumerWedgesNeitherThePublisherNorItsPeers", func(t *testing.T) {
		const total = 500
		b := newBus(t, Config{Retention: 4096, Backlog: 32})

		stalled := subscribe(t, b, on(Channel))
		healthy := newTracker(t, b, on(Channel))

		slowest := time.Duration(0)
		for seq := uint64(1); seq <= total; seq++ {
			started := time.Now()
			if err := b.Publish(t.Context(), text(Channel, "run_1", seq)); err != nil {
				t.Fatalf("Publish %d: %v", seq, err)
			}
			if took := time.Since(started); took > slowest {
				slowest = took
			}
		}
		if slowest > waitPublish {
			t.Errorf("slowest Publish took %v: a stalled consumer must not slow the publisher", slowest)
		}
		wantContiguous(t, healthy.wait(t, total), 1, total)

		got, err := drain(stalled, total)
		if err != nil && !errors.Is(err, bus.ErrSlowConsumer) {
			t.Fatalf("stalled consumer Recv error = %v, want nil or bus.ErrSlowConsumer", err)
		}
		if len(got) > 0 {
			wantContiguous(t, got, 1, got[len(got)-1])
		}
	})

	t.Run("RetentionKeepsTheNewestEventsPerRun", func(t *testing.T) {
		const (
			retention = 8
			published = 20
		)
		b := newBus(t, Config{Retention: retention, Backlog: 64})
		if err := publishRange(t.Context(), b, "run_wraps", 1, published); err != nil {
			t.Fatal(err)
		}
		if err := publishRange(t.Context(), b, "run_small", 1, 3); err != nil {
			t.Fatal(err)
		}
		s := subscribe(t, b, on(Channel))
		byRun := map[string][]uint64{}
		for _, e := range recvN(t, s, retention+3) {
			byRun[e.Run] = append(byRun[e.Run], e.Seq)
		}
		wantContiguous(t, byRun["run_wraps"], published-retention+1, published)
		wantContiguous(t, byRun["run_small"], 1, 3)
		recvIdle(t, s)
	})

	t.Run("ClosedStreamReportsClosed", func(t *testing.T) {
		b := newBus(t, Config{Retention: 64, Backlog: 64})
		s := subscribe(t, b, on(Channel))
		publish(t, b, text(Channel, "run_1", 1))
		recvN(t, s, 1)

		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("second Close: %v, want nil: Close is idempotent", err)
		}
		if _, err := s.Recv(t.Context()); !errors.Is(err, bus.ErrClosed) {
			t.Fatalf("Recv after Close = %v, want bus.ErrClosed", err)
		}

		blocked := subscribe(t, b, on("slack://T01/quiet"))
		errc := make(chan error, 1)
		go func() {
			_, err := blocked.Recv(context.WithoutCancel(t.Context()))
			errc <- err
		}()
		time.Sleep(50 * time.Millisecond)
		if err := blocked.Close(); err != nil {
			t.Fatalf("Close while Recv blocked: %v", err)
		}
		select {
		case err := <-errc:
			if !errors.Is(err, bus.ErrClosed) {
				t.Fatalf("blocked Recv = %v, want bus.ErrClosed", err)
			}
		case <-time.After(waitLong):
			t.Fatal("blocked Recv did not return after Close")
		}
	})

	t.Run("CancellingTheSubscriptionClosesTheStream", func(t *testing.T) {
		b := newBus(t, Config{Retention: 64, Backlog: 64})
		ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
		s, err := b.Subscribe(ctx, on(Channel))
		if err != nil {
			cancel()
			t.Fatalf("Subscribe: %v", err)
		}
		defer func() { _ = s.Close() }()

		errc := make(chan error, 1)
		go func() {
			_, err := s.Recv(context.WithoutCancel(t.Context()))
			errc <- err
		}()
		time.Sleep(50 * time.Millisecond)
		cancel()
		select {
		case err := <-errc:
			if !errors.Is(err, bus.ErrClosed) {
				t.Fatalf("Recv after the subscription context was cancelled = %v, want bus.ErrClosed", err)
			}
		case <-time.After(waitLong):
			t.Fatal("cancelling the subscription context did not unblock Recv")
		}
	})

	t.Run("RecvHonoursItsOwnContext", func(t *testing.T) {
		b := newBus(t, Config{Retention: 64, Backlog: 64})
		s := subscribe(t, b, on(Channel))

		ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
		errc := make(chan error, 1)
		go func() {
			_, err := s.Recv(ctx)
			errc <- err
		}()
		time.Sleep(50 * time.Millisecond)
		cancel()
		select {
		case err := <-errc:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Recv error = %v, want context.Canceled", err)
			}
		case <-time.After(waitLong):
			t.Fatal("cancelling Recv's context did not unblock it")
		}
		publish(t, b, text(Channel, "run_1", 1))
		recvN(t, s, 1)
	})

	t.Run("SubscriptionsLeaveNoGoroutinesBehind", func(t *testing.T) {
		b := newBus(t, Config{Retention: 64, Backlog: 64})
		cycle(t, b, false)
		cycle(t, b, true)
		runtime.GC()
		baseline := runtime.NumGoroutine()
		for range 20 {
			cycle(t, b, false)
			cycle(t, b, true)
		}
		deadline := time.Now().Add(waitLong)
		for {
			runtime.GC()
			got := runtime.NumGoroutine()
			if got <= baseline {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("goroutines = %d after 40 subscribe/release cycles, want <= %d", got, baseline)
			}
			time.Sleep(20 * time.Millisecond)
		}
	})

	t.Run("UnpublishableEventIsRejected", func(t *testing.T) {
		b := newBus(t, Config{Retention: 64, Backlog: 64})

		mutate := func(f func(*event.Envelope)) event.Envelope {
			e := text(Channel, "run_1", 1)
			f(&e)
			return e
		}
		cases := []struct {
			name string
			e    event.Envelope
		}{
			{"zero", event.Envelope{}},
			{"no tenant", mutate(func(e *event.Envelope) { e.Tenant = "" })},
			{"no id", mutate(func(e *event.Envelope) { e.ID = "" })},
			{"no type", mutate(func(e *event.Envelope) { e.Type = "" })},
			{"no channel", mutate(func(e *event.Envelope) { e.Channel = "" })},
			{"run event with no run", mutate(func(e *event.Envelope) { e.Run = "" })},
			{"run event with no sequence", mutate(func(e *event.Envelope) { e.Seq = 0 })},
			{"type and data disagree", mutate(func(e *event.Envelope) { e.Type = event.TypeThinking })},
			{"over the hop limit", mutate(func(e *event.Envelope) { e.Hops = bus.DefaultMaxHops + 1 })},
			{"sequenced message", func() event.Envelope { e := message(Channel, "Ev1", "hi"); e.Seq = 3; return e }()},
		}
		for _, tc := range cases {
			if err := b.Publish(t.Context(), tc.e); !errors.Is(err, bus.ErrInvalid) {
				t.Errorf("Publish(%s) = %v, want bus.ErrInvalid", tc.name, err)
			}
		}
		atLimit := mutate(func(e *event.Envelope) { e.Hops = bus.DefaultMaxHops })
		if err := b.Publish(t.Context(), atLimit); err != nil {
			t.Errorf("Publish at the hop limit = %v, want nil", err)
		}
		if _, err := b.Subscribe(t.Context(), bus.Subscription{Tenant: Tenant, Channels: []string{""}, Types: []string{"run"}}); !errors.Is(err, bus.ErrInvalid) {
			t.Errorf("Subscribe with an empty channel prefix = %v, want bus.ErrInvalid", err)
		}
	})

	t.Run("Groups", func(t *testing.T) { Groups(t, newBus) })
}

// Groups holds a backend to the consumer group contract. Run calls it; a
// backend's own tests may call it alone.
func Groups(t *testing.T, newBus Factory) {
	t.Run("EachEventIsDeliveredOncePerGroup", func(t *testing.T) {
		b := newBus(t, Config{Retention: 64, Backlog: 64, Lease: time.Second})
		ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
		defer cancel()

		got := make(chan member, 64)
		for _, name := range []string{"a1", "a2"} {
			go collect(ctx, consume(t, b, ctx, "egress", on(Channel)), name, got)
		}
		other := make(chan member, 64)
		go collect(ctx, consume(t, b, ctx, "observer", on(Channel)), "o", other)

		publish(t, b, text(Channel, "run_1", 1), text(Channel, "run_2", 1), text(Channel, "run_1", 2),
			text(Channel, "run_2", 2), text(Channel, "run_1", 3), text(Channel, "run_2", 3))
		want := []string{"run_1#1", "run_1#2", "run_1#3", "run_2#1", "run_2#2", "run_2#3"}

		deliveries := gather(t, got, len(want))
		idle(t, got)
		members := map[string]bool{}
		var labels []string
		for _, d := range deliveries {
			members[d.name] = true
			labels = append(labels, d.Event.ID)
		}
		slices.Sort(labels)
		if !slices.Equal(labels, want) {
			t.Errorf("group delivered %v, want each of %v once", labels, want)
		}
		if len(members) != 1 {
			t.Errorf("events went to members %v, want to the one holding the lease", members)
		}
		for i := 1; i < len(deliveries); i++ {
			if deliveries[i].Pos <= deliveries[i-1].Pos {
				t.Errorf("positions %d then %d, want increasing", deliveries[i-1].Pos, deliveries[i].Pos)
			}
		}

		labels = nil
		for _, d := range gather(t, other, len(want)) {
			labels = append(labels, d.Event.ID)
		}
		slices.Sort(labels)
		if !slices.Equal(labels, want) {
			t.Errorf("a second group got %v, want every event again", labels)
		}
	})

	t.Run("AMemberThatDiesIsTakenOverFromItsCursor", func(t *testing.T) {
		b := newBus(t, Config{Retention: 64, Backlog: 64, Lease: 500 * time.Millisecond})
		publish(t, b, text(Channel, "run_1", 1), text(Channel, "run_1", 2), text(Channel, "run_1", 3),
			text(Channel, "run_1", 4), text(Channel, "run_1", 5))

		dying, die := context.WithCancel(context.WithoutCancel(t.Context()))
		a, err := b.Consume(dying, "egress", on(Channel))
		if err != nil {
			t.Fatal(err)
		}
		first := recvGroup(t, a, 3)
		if err := a.Commit(t.Context(), first[1].Pos); err != nil {
			t.Fatalf("Commit: %v", err)
		}

		// b2 waits on the lease a holds; a dies without releasing it.
		ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
		defer cancel()
		got := make(chan member, 16)
		go collect(ctx, consume(t, b, ctx, "egress", on(Channel)), "b", got)
		idle(t, got)
		die()

		var seqs []uint64
		for _, d := range gather(t, got, 3) {
			seqs = append(seqs, d.Event.Seq)
		}
		wantContiguous(t, seqs, 3, 5)
		publish(t, b, text(Channel, "run_1", 6))
		if d := gather(t, got, 1); d[0].Event.Seq != 6 {
			t.Errorf("after takeover got %s, want run_1#6", d[0].Event.ID)
		}
		if err := a.Commit(t.Context(), first[2].Pos); !errors.Is(err, bus.ErrLeaseLost) {
			t.Errorf("Commit by the member that died = %v, want bus.ErrLeaseLost", err)
		}
		_ = a.Close()
	})

	t.Run("ANewMemberResumesAfterTheCommittedCursor", func(t *testing.T) {
		b := newBus(t, Config{Retention: 64, Backlog: 64, Lease: time.Second})
		publish(t, b, text(Channel, "run_1", 1), text(Channel, "run_1", 2), text(Channel, "run_1", 3))

		a, err := b.Consume(context.WithoutCancel(t.Context()), "observer", on(Channel))
		if err != nil {
			t.Fatal(err)
		}
		read := recvGroup(t, a, 2)
		if err := a.Commit(t.Context(), read[1].Pos); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		// A commit never moves the cursor back.
		if err := a.Commit(t.Context(), read[0].Pos); err != nil {
			t.Fatalf("Commit of an earlier position: %v", err)
		}
		if err := a.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		publish(t, b, text(Channel, "run_1", 4))

		// Close released the lease, so the next member does not wait it out.
		c := consume(t, b, context.WithoutCancel(t.Context()), "observer", on(Channel))
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 400*time.Millisecond)
		defer cancel()
		var seqs []uint64
		for range 2 {
			d, err := c.Recv(ctx)
			if err != nil {
				t.Fatalf("Recv after a released lease: %v", err)
			}
			seqs = append(seqs, d.Event.Seq)
		}
		wantContiguous(t, seqs, 3, 4)
	})

	t.Run("TenantsHaveLeasesOfTheirOwn", func(t *testing.T) {
		b := newBus(t, Config{Retention: 64, Backlog: 64, Lease: time.Second})
		acme := consume(t, b, context.WithoutCancel(t.Context()), "egress", on(Channel))
		globexSub := on(Channel)
		globexSub.Tenant = "globex"
		globex := consume(t, b, context.WithoutCancel(t.Context()), "egress", globexSub)
		other := text(Channel, "run_9", 1)
		other.Tenant = "globex"
		publish(t, b, text(Channel, "run_1", 1), other)
		if d := recvGroup(t, acme, 1); d[0].Event.Tenant != Tenant {
			t.Errorf("acme's member got %s's event", d[0].Event.Tenant)
		}
		if d := recvGroup(t, globex, 1); d[0].Event.Tenant != "globex" {
			t.Errorf("globex's member got %s's event", d[0].Event.Tenant)
		}
	})

	t.Run("AGroupNeedsANameAndATenant", func(t *testing.T) {
		b := newBus(t, Config{Retention: 64, Backlog: 64})
		if _, err := b.Consume(t.Context(), "", on(Channel)); !errors.Is(err, bus.ErrInvalid) {
			t.Errorf("Consume with no group = %v, want bus.ErrInvalid", err)
		}
		noTenant := on(Channel)
		noTenant.Tenant = ""
		if _, err := b.Consume(t.Context(), "egress", noTenant); !errors.Is(err, bus.ErrInvalid) {
			t.Errorf("Consume with no tenant = %v, want bus.ErrInvalid", err)
		}
	})

	t.Run("ClosedGroupStreamReportsClosed", func(t *testing.T) {
		b := newBus(t, Config{Retention: 64, Backlog: 64})
		s, err := b.Consume(context.WithoutCancel(t.Context()), "egress", on(Channel))
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
		_ = s.Close()
		if _, err := s.Recv(t.Context()); !errors.Is(err, bus.ErrClosed) {
			t.Errorf("Recv after Close = %v, want bus.ErrClosed", err)
		}
	})
}

// member is one delivery and the member that received it.
type member struct {
	bus.Delivery
	name string
}

// consume joins group or fails the test, and closes the stream afterwards.
func consume(t *testing.T, b bus.Bus, ctx context.Context, group string, sub bus.Subscription) bus.GroupStream {
	t.Helper()
	s, err := b.Consume(ctx, group, sub)
	if err != nil {
		t.Fatalf("Consume(%s): %v", group, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// collect sends everything s delivers to out until s ends.
func collect(ctx context.Context, s bus.GroupStream, name string, out chan<- member) {
	for {
		d, err := s.Recv(ctx)
		if err != nil {
			return
		}
		out <- member{Delivery: d, name: name}
	}
}

// gather receives exactly n deliveries or fails the test.
func gather(t *testing.T, in <-chan member, n int) []member {
	t.Helper()
	timeout := time.NewTimer(waitLong)
	defer timeout.Stop()
	got := make([]member, 0, n)
	for len(got) < n {
		select {
		case d := <-in:
			got = append(got, d)
		case <-timeout.C:
			t.Fatalf("received %d of %d deliveries", len(got), n)
		}
	}
	return got
}

// idle asserts nothing more is delivered for a while.
func idle(t *testing.T, in <-chan member) {
	t.Helper()
	select {
	case d := <-in:
		t.Fatalf("%s received %s, want nothing", d.name, d.Event.ID)
	case <-time.After(waitIdle):
	}
}

// recvGroup receives exactly n deliveries from s or fails the test.
func recvGroup(t *testing.T, s bus.GroupStream, n int) []bus.Delivery {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), waitLong)
	defer cancel()
	got := make([]bus.Delivery, 0, n)
	for range n {
		d, err := s.Recv(ctx)
		if err != nil {
			t.Fatalf("Recv %d of %d: %v", len(got)+1, n, err)
		}
		got = append(got, d)
	}
	return got
}

// EmptySubscriptionMatchesNothing holds a backend to default deny: a
// subscription missing its tenant, its channels or its types receives
// nothing.
func EmptySubscriptionMatchesNothing(t *testing.T, newBus Factory) {
	b := newBus(t, Config{Retention: 64, Backlog: 64})
	noTenant := on(Channel)
	noTenant.Tenant = ""
	noChannels := on(Channel)
	noChannels.Channels = nil
	noTypes := on(Channel)
	noTypes.Types = nil
	subs := map[string]bus.Stream{
		"zero":        subscribe(t, b, bus.Subscription{}),
		"no tenant":   subscribe(t, b, noTenant),
		"no channels": subscribe(t, b, noChannels),
		"no types":    subscribe(t, b, noTypes),
	}
	publish(t, b, text(Channel, "run_1", 1), message(Channel, "Ev1", "hi"))
	for name, s := range subs {
		t.Run(name, func(t *testing.T) { recvIdle(t, s) })
	}
}

// RootDoesNotSeeControl holds a backend to control isolation: no prefix
// reaches a control channel; only the channel named exactly does.
func RootDoesNotSeeControl(t *testing.T, newBus Factory) {
	b := newBus(t, Config{Retention: 64, Backlog: 64})
	ctl := event.ControlChannel(Tenant, "stop", "run_1")
	broad := bus.Subscription{
		Tenant:   Tenant,
		Channels: []string{"control:", "control://", "control://" + Tenant, "control://" + Tenant + "/stop", "slack://"},
		Types:    []string{"control", "run"},
	}
	everything := subscribe(t, b, broad)
	exact := subscribe(t, b, bus.Subscription{Tenant: Tenant, Channels: []string{ctl}, Types: []string{"control"}})

	publish(t, b, stop(ctl, "run_1", "k1"), text(Channel, "run_1", 1))

	if got := recvN(t, everything, 1); got[0].Type != event.TypeText {
		t.Errorf("a broad subscription delivered %s on %s", got[0].Type, got[0].Channel)
	}
	recvIdle(t, everything)
	if got := recvN(t, exact, 1); got[0].Type != event.TypeStop {
		t.Errorf("the control channel delivered %s", got[0].Type)
	}
}

// tracker is a consumer that keeps up: it reads as fast as it can and, if the
// bus terminates it for falling behind, resubscribes from the last sequence it
// processed. That is the recovery every consumer of an at-least-once bus is
// required to implement, and it is why terminating a slow consumer is safe.
//
// It tracks a single run, because resuming from a cursor is only unambiguous
// for one.
type tracker struct {
	seqs chan uint64
	errc chan error
	done chan struct{}
}

func newTracker(t *testing.T, b bus.Bus, sub bus.Subscription) *tracker {
	t.Helper()
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	tr := &tracker{
		seqs: make(chan uint64, 8192),
		errc: make(chan error, 1),
		done: make(chan struct{}),
	}
	go func() {
		defer close(tr.done)
		for {
			s, err := b.Subscribe(ctx, sub)
			if err != nil {
				tr.fail(fmt.Errorf("resubscribe from %d: %w", sub.From, err))
				return
			}
			for {
				e, err := s.Recv(ctx)
				if err != nil {
					_ = s.Close()
					if errors.Is(err, bus.ErrSlowConsumer) {
						break // resubscribe from the cursor we reached
					}
					tr.fail(err)
					return
				}
				sub.From = e.Seq
				select {
				case tr.seqs <- e.Seq:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-tr.done })
	return tr
}

func (tr *tracker) fail(err error) {
	select {
	case tr.errc <- err:
	default:
	}
}

// wait collects n sequences, or fails.
func (tr *tracker) wait(t *testing.T, n int) []uint64 {
	t.Helper()
	got := make([]uint64, 0, n)
	deadline := time.After(waitLong)
	for len(got) < n {
		select {
		case seq := <-tr.seqs:
			got = append(got, seq)
		case err := <-tr.errc:
			t.Fatalf("consumer failed after %d of %d events: %v", len(got), n, err)
		case <-tr.done:
			t.Fatalf("consumer stopped after %d of %d events", len(got), n)
		case <-deadline:
			t.Fatalf("consumer received %d of %d events before the deadline", len(got), n)
		}
	}
	return got
}

// cycles numbers the events cycle publishes, so each is new to the run.
var cycles uint64

// cycle subscribes, moves one event through the stream, and releases it either
// by cancelling its context or by closing it.
func cycle(t *testing.T, b bus.Bus, byCancel bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	defer cancel()

	s, err := b.Subscribe(ctx, on(Channel))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	cycles++
	publish(t, b, text(Channel, "run_cycle", cycles))
	recvN(t, s, 1)
	if byCancel {
		cancel()
		return
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// on is a subscription to every run event under a channel prefix, in the
// suite's tenant, from the beginning of what the bus holds.
func on(channel string) bus.Subscription {
	return bus.Subscription{Tenant: Tenant, Channels: []string{channel}, Types: []string{"run"}}
}

// ev builds a run event whose identity is its label, so a test can tell which
// event it received and not merely how many.
func ev(channel, run string, seq uint64, body event.Body) event.Envelope {
	return event.Envelope{
		ID:      label(run, seq),
		Source:  "fiber://run/" + run,
		Type:    body.Type(),
		Channel: channel,
		Tenant:  Tenant,
		Run:     run,
		Seq:     seq,
		Agent:   "docs-bot",
		Version: "1",
		Data:    body,
	}
}

// text is a streaming-output event, the common case. Its text is its label.
func text(channel, run string, seq uint64) event.Envelope {
	return ev(channel, run, seq, event.Text{Text: label(run, seq)})
}

// message is an ingress event.
func message(channel, id, text string) event.Envelope {
	return event.Envelope{
		ID:      id,
		Source:  "slack://T01",
		Type:    event.TypeMessagePosted,
		Channel: channel,
		Tenant:  Tenant,
		Data:    event.MessagePosted{Request: event.Request{Text: text}},
	}
}

// stop is a stop request on a control channel.
func stop(channel, run, id string) event.Envelope {
	return event.Envelope{
		ID:      id,
		Source:  "fiber://control",
		Type:    event.TypeStop,
		Channel: channel,
		Tenant:  Tenant,
		Run:     run,
		Data:    event.StopRequest{RunID: run},
	}
}

func label(runID string, seq uint64) string {
	return fmt.Sprintf("%s#%d", runID, seq)
}

// publish publishes each event or fails the test.
func publish(t *testing.T, b bus.Bus, events ...event.Envelope) {
	t.Helper()
	for _, e := range events {
		if err := b.Publish(t.Context(), e); err != nil {
			t.Fatalf("Publish(%s %s): %v", e.Type, e.ID, err)
		}
	}
}

// publishRange publishes one event per sequence in [from, to]. It returns an
// error rather than failing the test, so it can be called from a goroutine.
func publishRange(ctx context.Context, b bus.Bus, runID string, from, to uint64) error {
	for seq := from; seq <= to; seq++ {
		if err := b.Publish(ctx, text(Channel, runID, seq)); err != nil {
			return fmt.Errorf("publish %s: %w", label(runID, seq), err)
		}
	}
	return nil
}

// subscribe subscribes or fails the test, and closes the stream afterwards.
func subscribe(t *testing.T, b bus.Bus, sub bus.Subscription) bus.Stream {
	t.Helper()
	s, err := b.Subscribe(context.WithoutCancel(t.Context()), sub)
	if err != nil {
		t.Fatalf("Subscribe(%v): %v", sub.Channels, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// recvN receives exactly n events or fails the test.
func recvN(t *testing.T, s bus.Stream, n int) []event.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), waitLong)
	defer cancel()

	got := make([]event.Envelope, 0, n)
	for range n {
		e, err := s.Recv(ctx)
		if err != nil {
			t.Fatalf("Recv %d of %d: %v", len(got)+1, n, err)
		}
		got = append(got, e)
	}
	return got
}

// recvIdle asserts nothing more arrives. It is how a test proves an event was
// filtered out rather than merely late.
func recvIdle(t *testing.T, s bus.Stream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), waitIdle)
	defer cancel()

	e, err := s.Recv(ctx)
	if err == nil {
		t.Fatalf("Recv returned %s %s, want nothing", e.Type, e.ID)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Recv error = %v, want context.DeadlineExceeded", err)
	}
}

// drain reads up to n events and stops at the first error, returning both.
func drain(s bus.Stream, n int) ([]uint64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), waitLong)
	defer cancel()

	var got []uint64
	for range n {
		e, err := s.Recv(ctx)
		if err != nil {
			return got, err
		}
		got = append(got, e.Seq)
	}
	return got, nil
}

func seqsOf(events []event.Envelope) []uint64 {
	out := make([]uint64, len(events))
	for i, e := range events {
		out[i] = e.Seq
	}
	return out
}

func labelsOf(events []event.Envelope) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.ID
	}
	return out
}

// wantContiguous asserts got is exactly first..last ascending: no gap, no
// duplicate, no reordering.
func wantContiguous(t *testing.T, got []uint64, first, last uint64) {
	t.Helper()
	want := make([]uint64, 0, last-first+1)
	for seq := first; seq <= last; seq++ {
		want = append(want, seq)
	}
	if slices.Equal(got, want) {
		return
	}
	t.Fatalf("sequences = %s, want %d..%d contiguous", summarize(got), first, last)
}

// summarize renders a long sequence list as runs, so a failure reads as "1..99,
// 101..200" instead of two hundred numbers.
func summarize(seqs []uint64) string {
	if len(seqs) == 0 {
		return "[]"
	}
	var b strings.Builder
	start := seqs[0]
	for i := 1; i <= len(seqs); i++ {
		if i < len(seqs) && seqs[i] == seqs[i-1]+1 {
			continue
		}
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		if start == seqs[i-1] {
			fmt.Fprintf(&b, "%d", start)
		} else {
			fmt.Fprintf(&b, "%d..%d", start, seqs[i-1])
		}
		if i < len(seqs) {
			start = seqs[i]
		}
	}
	return "[" + b.String() + "]"
}
