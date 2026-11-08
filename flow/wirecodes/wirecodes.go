// Package wirecodes gives the flow packages' sentinel errors their wire
// codes, so errors.Is holds for them after an error crosses a process
// boundary (see types.RegisterWireSentinel).
//
// The flow packages do not register themselves, so the leaves stay free of
// saige's types. Import this package for its side effect wherever flow
// errors reach an agent's errors or a client:
//
//	import _ "github.com/urmzd/saige/flow/wirecodes"
//
// Codes are part of the wire contract: a change is a breaking change for
// every process that exchanges errors.
package wirecodes

import (
	agenttypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/flow/address"
	"github.com/urmzd/saige/flow/attachment"
	attachmentintake "github.com/urmzd/saige/flow/attachment/intake"
	attachmentresolve "github.com/urmzd/saige/flow/attachment/resolve"
	"github.com/urmzd/saige/flow/blob"
	"github.com/urmzd/saige/flow/bus"
	"github.com/urmzd/saige/flow/connector"
	"github.com/urmzd/saige/flow/definition"
	"github.com/urmzd/saige/flow/event"
	eventlegacy "github.com/urmzd/saige/flow/event/legacy"
	"github.com/urmzd/saige/flow/isolation"
	"github.com/urmzd/saige/flow/runtime"
	"github.com/urmzd/saige/flow/session"
	"github.com/urmzd/saige/flow/signature"
	"github.com/urmzd/saige/flow/thread"
	"github.com/urmzd/saige/flow/turn"
	turnnative "github.com/urmzd/saige/flow/turn/native"
	turnsandbox "github.com/urmzd/saige/flow/turn/sandbox"
)

func init() {
	for code, err := range map[string]error{
		"flow.address.invalid":                      address.ErrInvalid,
		"flow.attachment.invalid":                   attachment.ErrInvalid,
		"flow.attachment.intake.blocked_address":    attachmentintake.ErrBlockedAddress,
		"flow.attachment.intake.not_allowed":        attachmentintake.ErrNotAllowed,
		"flow.attachment.intake.refused":            attachmentintake.ErrRefused,
		"flow.attachment.intake.status":             attachmentintake.ErrStatus,
		"flow.attachment.intake.too_many_redirects": attachmentintake.ErrTooManyRedirects,
		"flow.attachment.resolve.not_applicable":    attachmentresolve.ErrNotApplicable,
		"flow.attachment.resolve.unavailable":       attachmentresolve.ErrUnavailable,
		"flow.blob.corrupt":                         blob.ErrCorrupt,
		"flow.blob.digest_mismatch":                 blob.ErrDigestMismatch,
		"flow.blob.invalid":                         blob.ErrInvalid,
		"flow.blob.not_found":                       blob.ErrNotFound,
		"flow.blob.too_large":                       blob.ErrTooLarge,
		"flow.bus.closed":                           bus.ErrClosed,
		"flow.bus.invalid":                          bus.ErrInvalid,
		"flow.bus.lease_lost":                       bus.ErrLeaseLost,
		"flow.bus.slow_consumer":                    bus.ErrSlowConsumer,
		"flow.connector.not_found":                  connector.ErrNotFound,
		"flow.connector.undeliverable":              connector.ErrUndeliverable,
		"flow.connector.unsupported":                connector.ErrUnsupported,
		"flow.definition.conflict":                  definition.ErrConflict,
		"flow.definition.exists":                    definition.ErrExists,
		"flow.definition.invalid":                   definition.ErrInvalid,
		"flow.definition.not_found":                 definition.ErrNotFound,
		"flow.definition.pin":                       definition.ErrPin,
		"flow.definition.retired":                   definition.ErrRetired,
		"flow.event.invalid":                        event.ErrInvalid,
		"flow.event.legacy.dropped":                 eventlegacy.ErrDropped,
		"flow.event.legacy.unknown":                 eventlegacy.ErrUnknown,
		"flow.isolation.no_connectors":              isolation.ErrNoConnectors,
		"flow.isolation.released":                   isolation.ErrReleased,
		"flow.isolation.unisolated":                 isolation.ErrUnisolated,
		"flow.runtime.answer_unavailable":           runtime.ErrAnswerUnavailable,
		"flow.runtime.compact_unavailable":          runtime.ErrCompactUnavailable,
		"flow.runtime.hold_unavailable":             runtime.ErrHoldUnavailable,
		"flow.runtime.invalid":                      runtime.ErrInvalid,
		"flow.runtime.no_question":                  runtime.ErrNoQuestion,
		"flow.runtime.not_a_child":                  runtime.ErrNotAChild,
		"flow.runtime.not_allowed":                  runtime.ErrNotAllowed,
		"flow.runtime.not_held":                     runtime.ErrNotHeld,
		"flow.runtime.question_closed":              runtime.ErrQuestionClosed,
		"flow.runtime.seq_exhausted":                runtime.ErrSeqExhausted,
		"flow.runtime.stop_unavailable":             runtime.ErrStopUnavailable,
		"flow.runtime.stopped":                      runtime.ErrStopped,
		"flow.session.budget":                       session.ErrBudget,
		"flow.session.denied":                       session.ErrDenied,
		"flow.session.invalid":                      session.ErrInvalid,
		"flow.session.limit":                        session.ErrLimit,
		"flow.session.not_found":                    session.ErrNotFound,
		"flow.thread.gap":                           thread.ErrGap,
		"flow.thread.invalid":                       thread.ErrInvalid,
		"flow.turn.invalid":                         turn.ErrInvalid,
		"flow.turn.native.invalid":                  turnnative.ErrInvalid,
		"flow.turn.native.no_such_tool":             turnnative.ErrNoSuchTool,
		"flow.turn.native.no_such_tool_group":       turnnative.ErrNoSuchToolGroup,
		"flow.turn.sandbox.invalid":                 turnsandbox.ErrInvalid,
		"flow.signature.malformed":                  signature.ErrMalformed,
		"flow.signature.no_secret":                  signature.ErrNoSecret,
		"flow.signature.signature_mismatch":         signature.ErrSignatureMismatch,
		"flow.signature.timestamp_skew":             signature.ErrTimestampSkew,
	} {
		agenttypes.RegisterWireSentinel(code, err)
	}
}
