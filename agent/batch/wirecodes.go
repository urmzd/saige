package batch

import agenttypes "github.com/urmzd/saige/agent/types"

// The wire codes of this package's sentinel errors, so errors.Is holds for
// them after an error crosses a process boundary (see
// types.RegisterWireSentinel). Codes are part of the wire contract: never
// rename one.
func init() {
	agenttypes.RegisterWireSentinel("batch.duplicate_id", ErrDuplicateID)
	agenttypes.RegisterWireSentinel("batch.indeterminate", ErrIndeterminate)
	agenttypes.RegisterWireSentinel("batch.job_conflict", ErrJobConflict)
	agenttypes.RegisterWireSentinel("batch.job_failed", ErrJobFailed)
	agenttypes.RegisterWireSentinel("batch.job_not_found", ErrJobNotFound)
	agenttypes.RegisterWireSentinel("batch.manifest_mismatch", ErrManifestMismatch)
	agenttypes.RegisterWireSentinel("batch.not_ended", ErrNotEnded)
	agenttypes.RegisterWireSentinel("batch.not_submitted", ErrNotSubmitted)
	agenttypes.RegisterWireSentinel("batch.submit_incomplete", ErrSubmitIncomplete)
}
