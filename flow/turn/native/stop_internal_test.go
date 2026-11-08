package native

import (
	"sync/atomic"
	"testing"

	saige "github.com/urmzd/saige/agent"
	saigetypes "github.com/urmzd/saige/agent/types"
)

// saige refuses to wrap up some streams (a remote one, a replayed one, one
// under a durable step runner). A person who pressed stop does not care which
// kind of stream their turn is: it must still end. So a refused stop cancels
// the stream instead, and remembers that it did, so the turn is reported as
// stopped and not as the cancellation it was carried out with.
func TestAStreamThatCannotWrapUpIsCancelledAndStillReportedStopped(t *testing.T) {
	t.Parallel()

	deltas := make(chan saigetypes.Delta)
	var cancelled atomic.Bool
	stream := saige.NewRemoteStream(deltas, nil, func() {
		if cancelled.CompareAndSwap(false, true) {
			close(deltas)
		}
	}, nil)

	s := newStopper(stream)
	if _, ok := s.cancelled(); ok {
		t.Fatal("a stopper that has done nothing reports a cancellation")
	}
	s.apply(Stop{Reason: "Stopped by a user", By: "U1"})

	if !cancelled.Load() {
		t.Fatal("the stream was left running after a stop it could not wrap up")
	}
	got, ok := s.cancelled()
	if !ok || got != (Stop{Reason: "Stopped by a user", By: "U1"}) {
		t.Fatalf("cancelled() = %+v, %v; want the request that was applied", got, ok)
	}
	_ = stream.Wait()
	if _, stopped := stream.Stopped(); stopped {
		t.Fatal("the fixture is wrong: this stream wrapped up, so nothing was tested")
	}
}

// A stop that arrives after the stream has ended changes nothing: the turn
// finished first, and its outcome stands.
func TestAStopAfterTheStreamEndedIsNotACancellation(t *testing.T) {
	t.Parallel()

	a, err := saige.New(saige.Config{Name: "a", SystemPrompt: "p", Provider: offlineProviderFor(t)})
	if err != nil {
		t.Fatal(err)
	}
	stream := a.Invoke(t.Context(), []saigetypes.Message{saigetypes.UserMsg(saigetypes.Text("hi"))})
	drain(stream)
	if err := stream.Wait(); err != nil {
		t.Fatal(err)
	}

	s := newStopper(stream)
	s.apply(Stop{Reason: "too late"})
	if got, ok := s.cancelled(); ok {
		t.Fatalf("a finished stream was reported cancelled: %+v", got)
	}
	if _, stopped := stream.Stopped(); stopped {
		t.Fatal("a finished stream reports that it was stopped")
	}
}

func offlineProviderFor(t *testing.T) saigetypes.Provider {
	t.Helper()
	p, err := offlineProvider([][]saigetypes.Delta{saigetypes.PartDeltas(0, saigetypes.Text("done"))})(Spec{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
