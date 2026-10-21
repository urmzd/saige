package anchor

import agenttypes "github.com/urmzd/saige/agent/types"

// The wire codes of this package's sentinel errors, so errors.Is holds for
// them after an error crosses a process boundary (see
// types.RegisterWireSentinel). Codes are part of the wire contract: a
// change is a breaking change for every process that exchanges errors.
func init() {
	agenttypes.RegisterWireSentinel("durable.anchor.closed", ErrClosed)
	agenttypes.RegisterWireSentinel("durable.anchor.conflict", ErrConflict)
	agenttypes.RegisterWireSentinel("durable.anchor.failed", ErrFailed)
	agenttypes.RegisterWireSentinel("durable.anchor.indeterminate", ErrIndeterminate)
}
