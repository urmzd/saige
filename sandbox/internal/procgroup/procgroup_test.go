//go:build unix

package procgroup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// GrandchildPID reads the pid a test command wrote to path.
func GrandchildPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(path)
		if err == nil && strings.TrimSpace(string(raw)) != "" {
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatalf("pid file: %v", err)
			}
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatal("the command never wrote its child's pid")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Gone waits, bounded, for pid to stop existing.
func Gone(t *testing.T, pid int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("process %d is still running %s after the command was cancelled", pid, within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Cancelling a configured command ends the processes it started, one that
// ignores SIGTERM included, not only the command itself.
func TestCancelEndsTheWholeGroup(t *testing.T) {
	for _, tc := range []struct{ name, child string }{
		{"a child that stops on SIGTERM", "sleep 30"},
		{"a child that ignores SIGTERM", "trap '' TERM; sleep 30"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pid")
			ctx, cancel := context.WithCancel(context.Background())
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "/bin/sh -c \""+tc.child+"\" & echo $! > "+pidFile+"; wait")
			cmd.WaitDelay = 2 * time.Second
			Configure(cmd, 200*time.Millisecond)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			pid := GrandchildPID(t, pidFile)
			cancel()
			_ = cmd.Wait()
			Gone(t, pid, 5*time.Second)
		})
	}
}
