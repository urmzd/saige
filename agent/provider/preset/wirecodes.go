package preset

import agenttypes "github.com/urmzd/saige/agent/types"

// The wire codes of this package's sentinel errors, so errors.Is holds for
// them after an error crosses a process boundary (see
// types.RegisterWireSentinel). Codes are part of the wire contract: never
// rename one.
func init() {
	agenttypes.RegisterWireSentinel("provider.preset.no_local_model", ErrNoLocalModel)
}
