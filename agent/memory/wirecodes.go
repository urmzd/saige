package memory

import agenttypes "github.com/urmzd/saige/agent/types"

// The wire codes of this package's sentinel errors, so errors.Is holds for
// them after an error crosses a process boundary (see
// types.RegisterWireSentinel). Codes are part of the wire contract: never
// rename one.
func init() {
	agenttypes.RegisterWireSentinel("memory.kind_not_allowed", ErrKindNotAllowed)
	agenttypes.RegisterWireSentinel("memory.no_scope", ErrNoScope)
	agenttypes.RegisterWireSentinel("memory.not_found", ErrNotFound)
	agenttypes.RegisterWireSentinel("memory.read_only", ErrReadOnly)
	agenttypes.RegisterWireSentinel("memory.sensitive", ErrSensitive)
	agenttypes.RegisterWireSentinel("memory.unsupported", ErrUnsupported)
}
