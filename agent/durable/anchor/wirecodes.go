package anchor

import (
	agenttypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/durable/run"
)

// The wire codes of this package's sentinel errors, so errors.Is holds for
// them after an error crosses a process boundary (see
// types.RegisterWireSentinel). Codes are part of the wire contract: a
// change is a breaking change for every process that exchanges errors.
//
// The engine's own sentinels are registered here too: durable/run imports
// only the standard library, and this package is where they reach an
// agent's errors (a parked run's error wraps run.ErrParked).
func init() {
	agenttypes.RegisterWireSentinel("durable.anchor.closed", ErrClosed)
	agenttypes.RegisterWireSentinel("durable.anchor.conflict", ErrConflict)
	agenttypes.RegisterWireSentinel("durable.anchor.failed", ErrFailed)
	agenttypes.RegisterWireSentinel("durable.anchor.indeterminate", ErrIndeterminate)

	agenttypes.RegisterWireSentinel("durable.run.already_recorded", run.ErrAlreadyRecorded)
	agenttypes.RegisterWireSentinel("durable.run.already_terminal", run.ErrAlreadyTerminal)
	agenttypes.RegisterWireSentinel("durable.run.max_attempts", run.ErrMaxAttempts)
	agenttypes.RegisterWireSentinel("durable.run.non_deterministic", run.ErrNonDeterministic)
	agenttypes.RegisterWireSentinel("durable.run.not_found", run.ErrNotFound)
	agenttypes.RegisterWireSentinel("durable.run.parked", run.ErrParked)
	agenttypes.RegisterWireSentinel("durable.run.superseded", run.ErrSuperseded)
}
