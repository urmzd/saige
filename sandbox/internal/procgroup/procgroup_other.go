//go:build !unix

package procgroup

import (
	"os/exec"
	"time"
)

// Configure leaves cmd as exec configures it: without process groups only
// the started process is ended on cancellation.
func Configure(cmd *exec.Cmd, grace time.Duration) {}
