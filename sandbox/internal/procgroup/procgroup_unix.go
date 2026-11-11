//go:build unix

package procgroup

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// Configure makes cmd the leader of a new process group and replaces its
// cancellation: when cmd's context ends, the group is sent SIGTERM, and
// SIGKILL grace later. Call it before cmd starts.
func Configure(cmd *exec.Cmd, grace time.Duration) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		pgid := cmd.Process.Pid // the leader's pid is the group's id
		if err := signal(pgid, syscall.SIGTERM); err != nil {
			return err
		}
		// The group is killed even if the leader exits at once: its
		// children are what the leader's exit does not end.
		time.AfterFunc(grace, func() { _ = signal(pgid, syscall.SIGKILL) })
		return nil
	}
}

// signal sends sig to every process in the group. A group with nobody left
// in it is not an error: there is nothing to end.
func signal(pgid int, sig syscall.Signal) error {
	if err := syscall.Kill(-pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
