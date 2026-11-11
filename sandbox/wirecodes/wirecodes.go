// Package wirecodes gives the sandbox packages' sentinel errors their wire
// codes, so errors.Is holds for them after an error crosses a process
// boundary (see types.RegisterWireSentinel).
//
// The sandbox packages import only the standard library, so they do not
// register themselves. Import this package for its side effect wherever
// sandbox errors reach an agent's errors:
//
//	import _ "github.com/urmzd/saige/sandbox/wirecodes"
//
// Codes are part of the wire contract: a change is a breaking change for
// every process that exchanges errors.
package wirecodes

import (
	agenttypes "github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/sandbox/controlplane"
	"github.com/urmzd/saige/sandbox/isolate"
	"github.com/urmzd/saige/sandbox/isolate/docker"
	"github.com/urmzd/saige/sandbox/isolate/egress"
	"github.com/urmzd/saige/sandbox/ngac"
	"github.com/urmzd/saige/sandbox/runner"
	"github.com/urmzd/saige/sandbox/sandbox"
	"github.com/urmzd/saige/sandbox/workspace"
)

func init() {
	for code, err := range map[string]error{
		"sandbox.controlplane.closed":       controlplane.ErrClosed,
		"sandbox.controlplane.exists":       controlplane.ErrExists,
		"sandbox.controlplane.full":         controlplane.ErrFull,
		"sandbox.controlplane.not_found":    controlplane.ErrNotFound,
		"sandbox.controlplane.unknown_task": controlplane.ErrUnknownTask,

		"sandbox.isolate.closed":          isolate.ErrClosed,
		"sandbox.isolate.invalid_command": isolate.ErrInvalidCommand,
		"sandbox.isolate.invalid_spec":    isolate.ErrInvalidSpec,
		"sandbox.isolate.start":           isolate.ErrStart,
		"sandbox.isolate.timeout":         isolate.ErrTimeout,
		"sandbox.isolate.unsupported":     isolate.ErrUnsupported,

		"sandbox.isolate.docker.insecure_runtime": docker.ErrInsecureRuntime,

		"sandbox.egress.denied":             egress.ErrDenied,
		"sandbox.egress.duplicate_grant":    egress.ErrDuplicateGrant,
		"sandbox.egress.invalid_policy":     egress.ErrInvalidPolicy,
		"sandbox.egress.non_public_address": egress.ErrNonPublicAddress,

		"sandbox.ngac.exists":    ngac.ErrExists,
		"sandbox.ngac.not_found": ngac.ErrNotFound,

		"sandbox.runner.invalid_request": runner.ErrInvalidRequest,
		"sandbox.runner.start":           runner.ErrStart,
		"sandbox.runner.timeout":         runner.ErrTimeout,

		"sandbox.denied": sandbox.ErrDenied,

		"sandbox.workspace.invalid_key": workspace.ErrInvalidKey,
		"sandbox.workspace.not_found":   workspace.ErrNotFound,
	} {
		agenttypes.RegisterWireSentinel(code, err)
	}
}
