//go:build unix

package exec

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSubprocessKillsBackgroundChildren(t *testing.T) {
	sb, err := NewSubprocess()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		script string
		exit   int
	}{
		{name: "after a clean exit", script: "sleep 47 & echo $!", exit: 0},
		{name: "after a failing exit", script: "sleep 47 & echo $!; exit 4", exit: 4},
		{name: "when the child holds stderr", script: "sleep 47 >&2 & echo $!", exit: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Now()
			res, err := sb.Run(context.Background(), Command{
				Script: tt.script,
				Dir:    t.TempDir(),
				Env:    []string{"PATH=" + os.Getenv("PATH")},
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if took := time.Since(start); took > time.Second {
				t.Errorf("Run took %v, want it to return when the shell exits", took)
			}
			if res.ExitCode != tt.exit {
				t.Errorf("exit code = %d, want %d", res.ExitCode, tt.exit)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(res.Stdout))
			if err != nil {
				t.Fatalf("stdout %q is not a pid", res.Stdout)
			}
			deadline := time.Now().Add(3 * time.Second)
			for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				if time.Now().After(deadline) {
					_ = syscall.Kill(pid, syscall.SIGKILL)
					t.Fatalf("background child %d is still alive after Run returned", pid)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}
