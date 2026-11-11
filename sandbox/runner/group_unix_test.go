//go:build unix

package runner

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A run that times out ends the processes its command started, not only the
// command, one that ignores SIGTERM included.
func TestLocalTimeoutEndsTheProcessesTheCommandStarted(t *testing.T) {
	for _, child := range []string{"sleep 30", "trap '' TERM; sleep 30"} {
		t.Run(child, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pid")
			_, err := Local{}.Run(context.Background(), Request{
				Command: []string{"/bin/sh", "-c", `/bin/sh -c "` + child + `" & echo $! > ` + pidFile + `; wait`},
				Env:     map[string]string{"PATH": os.Getenv("PATH")},
				Timeout: 300 * time.Millisecond,
			}, io.Discard)
			if !errors.Is(err, ErrTimeout) {
				t.Fatalf("err = %v, want ErrTimeout", err)
			}
			raw, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatalf("the command never started its child: %v", err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				if time.Now().After(deadline) {
					_ = syscall.Kill(pid, syscall.SIGKILL)
					t.Fatalf("the child %d is still running after the run timed out", pid)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
