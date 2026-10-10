package store

import agenttypes "github.com/urmzd/saige/agent/types"

// The wire codes of this package's sentinel errors, so errors.Is holds for
// them after an error crosses a process boundary (see
// types.RegisterWireSentinel). Codes are part of the wire contract: never
// rename one.
func init() {
	agenttypes.RegisterWireSentinel("eval.store.duplicate_unit", ErrDuplicateUnit)
	agenttypes.RegisterWireSentinel("eval.store.invalid_id", ErrInvalidID)
	agenttypes.RegisterWireSentinel("eval.store.not_found", ErrNotFound)
	agenttypes.RegisterWireSentinel("eval.store.run_exists", ErrRunExists)
}
