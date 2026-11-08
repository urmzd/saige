package runtime_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/urmzd/saige/durable/ledger"
	drun "github.com/urmzd/saige/durable/run"
	"github.com/urmzd/saige/flow/event"
	"github.com/urmzd/saige/flow/runtime"
)

// outbox stands in for a transactional outbox: it keeps the accepted event
// and writes the run to the ledger in one call, or does neither.
type outbox struct {
	ledger *ledger.Memory
	fail   error

	mu     sync.Mutex
	events []event.Envelope
}

func (o *outbox) Accept(ctx context.Context, e event.Envelope, r drun.Run) error {
	if o.fail != nil {
		return o.fail
	}
	if err := o.ledger.Accept(ctx, r); err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, e)
	return nil
}

// With an outbox, the accepted event goes out with the run's ledger row, not
// as a publish of its own, and the run then proceeds as usual.
func TestAcceptGoesThroughTheOutbox(t *testing.T) {
	t.Parallel()
	o := &outbox{}
	h := newHarness(t, newExecutor("done", event.Text{}), runtime.WithOutbox(o))
	o.ledger = h.ledger
	h.specs.revise("docs-bot", "you document things")
	h.work(t)

	ctx := context.Background()
	acc, err := accept(ctx, h.rt, tag("docs-bot", "Ev1", "update the docs"))
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if _, err := h.rt.Result(ctx, acc.RunID); err != nil {
		t.Fatalf("Result: %v", err)
	}
	o.mu.Lock()
	got := append([]event.Envelope(nil), o.events...)
	o.mu.Unlock()
	if len(got) != 1 || got[0].Type != event.TypeAccepted || got[0].Run != acc.RunID || got[0].Seq != runtime.SeqAccepted {
		t.Fatalf("outbox events = %+v, want the run's one accepted event", got)
	}
	if n := len(h.bus.ofKind(event.TypeAccepted)); n != 0 {
		t.Fatalf("the accepted event was also published on its own %d times", n)
	}
	if n := len(h.bus.ofKind(event.TypeCompleted)); n != 1 {
		t.Fatalf("completed events = %d, want 1", n)
	}
}

// An outbox that commits nothing leaves no run and no acceptance.
func TestAcceptThatTheOutboxRefusesStartsNothing(t *testing.T) {
	t.Parallel()
	boom := errors.New("the transaction rolled back")
	o := &outbox{fail: boom}
	h := newHarness(t, newExecutor("done", event.Text{}), runtime.WithOutbox(o))
	o.ledger = h.ledger
	h.specs.revise("docs-bot", "you document things")

	f := tag("docs-bot", "Ev1", "update the docs")
	acc, err := accept(context.Background(), h.rt, f)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if acc.RunID != "" {
		t.Fatalf("an acceptance was returned for a run that was never committed: %+v", acc)
	}
	if _, err := h.ledger.GetRun(context.Background(), f.runID()); !errors.Is(err, drun.ErrNotFound) {
		t.Fatalf("GetRun = %v, want ErrNotFound", err)
	}
	if n := len(h.bus.all()); n != 0 {
		t.Fatalf("%d events were published for a run that was never committed", n)
	}
}
