package mcp

import "os/exec"

// localCommand builds the child process for a local server.
//
// #nosec G204 -- spawning the configured executable is the local transport.
// Command and Args come from a ServerSpec the deployment constructs in Go or
// loads from its own configuration file; the spec is never deserialized from
// model output or any other untrusted source.
func localCommand(spec ServerSpec) *exec.Cmd {
	cmd := exec.Command(spec.Command, spec.Args...) // #nosec G204
	cmd.Env = spec.Env
	cmd.Dir = spec.WorkDir
	return cmd
}
