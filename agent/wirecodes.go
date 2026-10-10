package agent

import agenttypes "github.com/urmzd/saige/agent/types"

// The wire codes of this package's sentinel errors, so errors.Is holds for
// them after an error crosses a process boundary (see
// types.RegisterWireSentinel). Codes are part of the wire contract: never
// rename one.
func init() {
	agenttypes.RegisterWireSentinel("agent.ancestor_delegation", ErrAncestorDelegation)
	agenttypes.RegisterWireSentinel("agent.handoff_limit_exceeded", ErrHandoffLimitExceeded)
	agenttypes.RegisterWireSentinel("agent.hook_timeout", ErrHookTimeout)
	agenttypes.RegisterWireSentinel("agent.invalid_submission", ErrInvalidSubmission)
	agenttypes.RegisterWireSentinel("agent.marker_resolved", ErrMarkerResolved)
	agenttypes.RegisterWireSentinel("agent.no_json", ErrNoJSON)
	agenttypes.RegisterWireSentinel("agent.nothing_to_continue", ErrNothingToContinue)
	agenttypes.RegisterWireSentinel("agent.repeated_tool_calls", ErrRepeatedToolCalls)
	agenttypes.RegisterWireSentinel("agent.run_active", ErrRunActive)
	agenttypes.RegisterWireSentinel("agent.run_finished", ErrRunFinished)
	agenttypes.RegisterWireSentinel("agent.schema_invalid", ErrSchemaInvalid)
	agenttypes.RegisterWireSentinel("agent.spawn_unsupported", ErrSpawnUnsupported)
	agenttypes.RegisterWireSentinel("agent.submit_unsupported", ErrSubmitUnsupported)
	agenttypes.RegisterWireSentinel("agent.tool_error_limit", ErrToolErrorLimit)
	agenttypes.RegisterWireSentinel("agent.unknown_handle", ErrUnknownHandle)
	agenttypes.RegisterWireSentinel("agent.unknown_handoff_target", ErrUnknownHandoffTarget)
	agenttypes.RegisterWireSentinel("agent.unknown_marker", ErrUnknownMarker)
}
