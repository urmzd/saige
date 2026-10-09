//go:build unix

package exec

import (
	osexec "os/exec"
	"syscall"
)

// setProcessGroup starts the command in its own process group and makes
// cancellation kill the whole group.
func setProcessGroup(cmd *osexec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// killGroup kills every process left in the command's group after the
// shell exits. A group that is already gone is not an error.
func killGroup(cmd *osexec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
