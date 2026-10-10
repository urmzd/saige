package local

import agenttypes "github.com/urmzd/saige/agent/types"

// The wire codes of this package's sentinel errors, so errors.Is holds for
// them after an error crosses a process boundary (see
// types.RegisterWireSentinel). Codes are part of the wire contract: never
// rename one.
func init() {
	agenttypes.RegisterWireSentinel("durable.local.busy", ErrBusy)
	agenttypes.RegisterWireSentinel("durable.local.closed", ErrClosed)
	agenttypes.RegisterWireSentinel("durable.local.conflict", ErrConflict)
	agenttypes.RegisterWireSentinel("durable.local.indeterminate", ErrIndeterminate)
	agenttypes.RegisterWireSentinel("durable.local.no_notifier", ErrNoNotifier)
	agenttypes.RegisterWireSentinel("durable.local.signal", ErrSignal)
}
