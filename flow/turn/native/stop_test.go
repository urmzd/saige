package native_test

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/agenttest"
	saigetypes "github.com/urmzd/saige/agent/types"

	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/turn/native"
)

// held is a model that says part of an answer and then keeps the call open
// until it is cancelled, which is what a model looks like to anyone who
// presses stop: mid-sentence. Calls after the first answer from a script.
type held struct {
	partial string
	then    [][]saigetypes.Delta

	mu    sync.Mutex
	calls int
}

func (h *held) Stream(ctx context.Context, _ saigetypes.Request) (<-chan saigetypes.Delta, error) {
	h.mu.Lock()
	n := h.calls
	h.calls++
	h.mu.Unlock()

	ch := make(chan saigetypes.Delta, 8)
	go func() {
		defer close(ch)
		if n > 0 {
			if n-1 < len(h.then) {
				for _, d := range h.then[n-1] {
					ch <- d
				}
			}
			return
		}
		// The part is started and never ended: the call is still streaming.
		for _, d := range saigetypes.PartDeltas(0, saigetypes.Text(h.partial))[:2] {
			ch <- d
		}
		<-ctx.Done()
	}()
	return ch, nil
}

func (h *held) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

func heldRunner(t *testing.T, h *held) *native.Runner {
	t.Helper()
	r, err := native.New(offlineRevision(1, "You document things."),
		native.WithProvider(native.ProviderOffline, func(native.Spec) (saigetypes.Provider, error) { return h, nil }))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// The reason the feature exists: a turn asked to stop while the model is
// mid-answer ends without an error, says it was stopped, why and by whom, and
// keeps the text that had already streamed. It is not a failure, so nothing
// that retries failures will run it again, and it is not a completion, so
// nothing will mistake half an answer for a whole one.
func TestAStopMidStreamEndsTheTurnStoppedAndKeepsWhatWasSaid(t *testing.T) {
	t.Parallel()

	model := &held{partial: "The deploy failed because"}
	r := heldRunner(t, model)

	stop := make(chan native.Stop, 1)
	var said sync.Once
	type outcome struct {
		res native.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := r.Run(context.Background(), native.Turn{Text: "why did the deploy fail?", Stop: stop},
			func(_ context.Context, c event.Body) error {
				if c.Type() == event.TypeText {
					// Stop only once the partial text is out, so the test
					// stops a turn that is demonstrably mid-stream.
					said.Do(func() { stop <- native.Stop{Reason: "Stopped by a user", By: "slack:U1"} })
				}
				return nil
			})
		done <- outcome{res, err}
	}()

	var got outcome
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the turn did not end after it was asked to stop")
	}
	if got.err != nil {
		t.Fatalf("a stopped turn returned an error, which a caller would retry: %v", got.err)
	}
	if got.res.Stopped == nil {
		t.Fatal("Result.Stopped is nil: the turn reads as completed")
	}
	if *got.res.Stopped != (native.Stop{Reason: "Stopped by a user", By: "slack:U1"}) {
		t.Errorf("Stopped = %+v, want the reason and the asker that were sent", *got.res.Stopped)
	}
	if got.res.Text != "The deploy failed because" {
		t.Errorf("Text = %q, want the partial answer", got.res.Text)
	}
	if got.res.Prompt != "why did the deploy fail?" {
		t.Errorf("Prompt = %q, want the request as it was sent", got.res.Prompt)
	}
	if n := model.count(); n != 1 {
		t.Errorf("the model was called %d times, want 1: a stopped turn asks nothing more", n)
	}
}

// A stop that is already waiting when the turn begins costs nothing: no model
// call is made. The request is still reported, so the caller can record what
// was asked next to the fact that it went unanswered.
func TestAStopWaitingBeforeTheTurnBeginsNeverCallsTheModel(t *testing.T) {
	t.Parallel()

	model := &held{partial: "never said"}
	r := heldRunner(t, model)

	stop := make(chan native.Stop, 1)
	stop <- native.Stop{Reason: "changed my mind", By: "U1"}

	chunks, res := collect(t, r, native.Turn{
		Text: "summarise the thread", Meta: map[string]string{"channel": "ops"}, Stop: stop,
	})
	if res.Stopped == nil || res.Stopped.Reason != "changed my mind" {
		t.Fatalf("Stopped = %+v, want the waiting request", res.Stopped)
	}
	if res.Text != "" || len(chunks) != 0 {
		t.Errorf("a turn stopped before it began produced text %q and %d chunks", res.Text, len(chunks))
	}
	if n := model.count(); n != 0 {
		t.Errorf("the model was called %d times, want 0", n)
	}
	if want := "Context from the trigger:\n- channel: ops\n\nsummarise the thread"; res.Prompt != want {
		t.Errorf("Prompt = %q, want %q", res.Prompt, want)
	}
}

// A turn that could have been stopped and was not is an ordinary turn: it
// completes, and nothing is left reading the stop channel afterwards.
func TestATurnNobodyStopsCompletes(t *testing.T) {
	t.Parallel()

	r, err := native.New(offlineRevision(1, "You document things."),
		native.WithScript(agenttest.TextResponse("the docs are updated")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stop := make(chan native.Stop, 1)
	_, res := collect(t, r, native.Turn{Text: "update the docs", Stop: stop})
	if res.Stopped != nil {
		t.Fatalf("Stopped = %+v on a turn nobody stopped", res.Stopped)
	}
	if res.Text != "the docs are updated" {
		t.Errorf("Text = %q", res.Text)
	}

	// A request that arrives after the turn returned is nobody's: it must
	// still be in the channel, not consumed by a goroutine the turn left
	// behind.
	stop <- native.Stop{Reason: "too late"}
	select {
	case <-stop:
	case <-time.After(time.Second):
		t.Fatal("a stop sent after the turn returned was consumed by something")
	}
}

// A turn that panics (here, in the caller's emit) unwinds through Run, and the
// goroutine that watches for a stop goes with it rather than waiting forever
// on a channel nobody will close.
func TestAPanickingTurnLeavesNoStopWatcherBehind(t *testing.T) {
	r, err := native.New(offlineRevision(1, "You document things."),
		native.WithScript(agenttest.TextResponse("the docs are updated")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	baseline := runtime.NumGoroutine()
	for range 5 {
		func() {
			defer func() { _ = recover() }()
			_, _ = r.Run(context.Background(), native.Turn{Text: "update the docs", Stop: make(chan native.Stop)},
				func(context.Context, event.Body) error { panic("emit bug") })
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines after five panicking turns, baseline %d", runtime.NumGoroutine(), baseline)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
