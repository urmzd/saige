//go:build !unix

package exec

import osexec "os/exec"

// setProcessGroup is a no-op where process groups are not available;
// cancellation kills only the shell.
func setProcessGroup(*osexec.Cmd) {}

// killGroup is a no-op where process groups are not available.
func killGroup(*osexec.Cmd) {}
