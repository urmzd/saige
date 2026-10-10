package exec

import agenttypes "github.com/urmzd/saige/agent/types"

// The wire codes of this package's sentinel errors, so errors.Is holds for
// them after an error crosses a process boundary (see
// types.RegisterWireSentinel). Codes are part of the wire contract: never
// rename one.
func init() {
	agenttypes.RegisterWireSentinel("tools.exec.command_denied", ErrCommandDenied)
	agenttypes.RegisterWireSentinel("tools.exec.docker_unavailable", ErrDockerUnavailable)
	agenttypes.RegisterWireSentinel("tools.exec.mount_not_shared", ErrMountNotShared)
	agenttypes.RegisterWireSentinel("tools.exec.network_isolation_unavailable", ErrNetworkIsolationUnavailable)
	agenttypes.RegisterWireSentinel("tools.exec.network_unenforced", ErrNetworkUnenforced)
	agenttypes.RegisterWireSentinel("tools.exec.no_languages", ErrNoLanguages)
}
