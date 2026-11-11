package runtime

import (
	"context"

	"github.com/urmzd/saige/durable/run"
	"github.com/urmzd/saige/flow/event"
)

// Outbox registers a run in the ledger and publishes the event that
// announces it in one commit, so a crash can never leave an accepted event
// for a run the ledger does not hold, or a run nobody was told about.
// flow/runschema.Outbox is the Postgres one: the ledger row and the bus row
// written in one transaction (bus.TxPublisher).
//
// Accept must be idempotent the way its halves are: a run the ledger holds is
// left as it is, and an event the bus has seen is not published again.
type Outbox interface {
	Accept(ctx context.Context, e event.Envelope, r run.Run) error
}

// WithOutbox has every run accepted through o: the run's accepted event and
// its ledger row commit together. Without it the event is published first
// and the run registered after, which a crash in between can split. o must
// write to the ledger and the bus the Runtime was built with.
func WithOutbox(o Outbox) Option {
	return func(r *Runtime) { r.outbox = o }
}
