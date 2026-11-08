// Package runner defines the run executor: the seam that runs one command
// to completion somewhere, streams its logs back, and reports its exit
// code. Implementations decide where "somewhere" is: Local runs a host
// process (no isolation, for development and tests); package runner/k8s
// runs one Kubernetes Job per run as the isolation boundary.
//
// Orthogonality boundary: runner knows nothing about queues, nodes, or the
// workspace policy (pkg/sandbox is the NGAC enforcement point, unrelated to
// process isolation). Shell adapts an Executor into a tool.Tool; that is
// the only place runs meet tools.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/urmzd/saige/sandbox/internal/procgroup"
)

// killGrace is how long the processes of a timed-out or cancelled local run
// have between SIGTERM and SIGKILL.
const killGrace = time.Second

var (
	// ErrInvalidRequest is returned for a malformed Request.
	ErrInvalidRequest = errors.New("runner: invalid request")
	// ErrTimeout is returned when a run exceeds its timeout.
	ErrTimeout = errors.New("runner: timeout")
	// ErrStart is returned when a run could not be started (bad image,
	// missing binary, rejected by the cluster).
	ErrStart = errors.New("runner: start failed")
)

// Resources bounds one run. Quantities use Kubernetes notation ("500m",
// "256Mi"); empty fields fall back to the executor's defaults.
type Resources struct {
	CPU              string
	Memory           string
	EphemeralStorage string
}

// Request describes one run.
type Request struct {
	// ID names the run. Executors that need a DNS-safe name derive one
	// from it; empty means the executor generates one.
	ID string
	// Image is the container image. Executors without containers ignore
	// it; an empty value means the executor's default.
	Image string
	// Command is the full argv. It is never passed through a shell unless
	// argv[0] is one.
	Command []string
	// Env is the run's entire environment. Nothing is inherited.
	Env map[string]string
	// Timeout bounds the run; zero means the executor's default.
	Timeout time.Duration
	// Resources bounds CPU, memory, and scratch disk.
	Resources Resources
}

// Validate reports whether r can be run.
func (r Request) Validate() error {
	if len(r.Command) == 0 || r.Command[0] == "" {
		return fmt.Errorf("%w: empty command", ErrInvalidRequest)
	}
	if r.Timeout < 0 {
		return fmt.Errorf("%w: negative timeout", ErrInvalidRequest)
	}
	return nil
}

// Result is the outcome of a run that started and exited.
type Result struct {
	ExitCode int
}

// Executor runs commands to completion.
type Executor interface {
	// Run executes req, copying its combined stdout and stderr to logs as
	// they are produced, and returns once it exits. A non-zero exit is a
	// Result, not an error; errors mean the run could not start, timed
	// out (ErrTimeout), or ctx ended. Implementations must be safe for
	// concurrent use.
	Run(ctx context.Context, req Request, logs io.Writer) (Result, error)
}

// DefaultTimeout bounds a run whose Request.Timeout is zero.
const DefaultTimeout = 5 * time.Minute

// Local runs requests as host processes. It is not a sandbox: the process
// has the caller's privileges. Use it for development and tests; use
// runner/k8s for isolation. Image and Resources are ignored.
type Local struct {
	// Dir is the working directory of every run. Empty runs each one in a
	// fresh temporary directory removed afterwards. A set Dir belongs to
	// the caller: it is neither created nor removed, and concurrent runs
	// share it.
	Dir string
}

// Run implements Executor.
func (l Local) Run(ctx context.Context, req Request, logs io.Writer) (Result, error) {
	if err := req.Validate(); err != nil {
		return Result{}, err
	}
	timeout := req.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	dir := l.Dir
	if dir == "" {
		tmp, err := os.MkdirTemp("", "playscape-run-*")
		if err != nil {
			return Result{}, fmt.Errorf("runner: local: workdir: %w", err)
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		dir = tmp
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, req.Command[0], req.Command[1:]...) //nolint:gosec // running the request's command is this executor's job
	cmd.Dir = dir
	cmd.Env = make([]string, 0, len(req.Env))
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	w := &syncWriter{w: logs}
	cmd.Stdout, cmd.Stderr = w, w
	cmd.WaitDelay = 2 * time.Second
	// A timeout or cancel ends everything the command started, not only
	// the command: see package procgroup.
	procgroup.Configure(cmd, killGrace)

	runErr := cmd.Run()
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Result{}, fmt.Errorf("%w: after %s", ErrTimeout, timeout)
		}
		return Result{}, fmt.Errorf("runner: local: %w", ctx.Err())
	}
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return Result{}, fmt.Errorf("%w: %w", ErrStart, runErr)
	}
	return Result{ExitCode: cmd.ProcessState.ExitCode()}, nil
}
