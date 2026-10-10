package privacy

import agenttypes "github.com/urmzd/saige/agent/types"

// The wire codes of this package's sentinel errors, so errors.Is holds for
// them after an error crosses a process boundary (see
// types.RegisterWireSentinel). Codes are part of the wire contract: never
// rename one.
func init() {
	agenttypes.RegisterWireSentinel("privacy.audio_out_refused", ErrAudioOutRefused)
	agenttypes.RegisterWireSentinel("privacy.media_refused", ErrMediaRefused)
}
