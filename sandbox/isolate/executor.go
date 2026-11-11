package isolate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/urmzd/saige/sandbox/runner"
)

// Executor runs runner requests in one Instance, so runner.Shell and other
// runner callers execute inside the sandbox instead of on the host. Each
// Run is one Exec in the workload compartment, starting in the work
// directory; every run shares the instance's files and its processes until
// the instance is closed.
//
// The instance was started from a Spec, so a Request's Image and Resources
// cannot change it: they are ignored. A Request's ID is ignored too.
type Executor struct {
	Instance Instance
}

var _ runner.Executor = Executor{}

// Run implements runner.Executor. Stdout and stderr are copied to logs as
// one stream. Errors match the runner sentinels: runner.ErrInvalidRequest,
// runner.ErrTimeout and runner.ErrStart, as well as the isolate ones.
func (e Executor) Run(ctx context.Context, req runner.Request, logs io.Writer) (runner.Result, error) {
	if err := req.Validate(); err != nil {
		return runner.Result{}, err
	}
	w := &syncWriter{w: logs}
	st, err := e.Instance.Exec(ctx, Command{Argv: req.Command, Env: req.Env, Timeout: req.Timeout}, Stdio{Stdout: w, Stderr: w})
	switch {
	case err == nil:
		return runner.Result{ExitCode: st.Code}, nil
	case errors.Is(err, ErrTimeout):
		return runner.Result{}, fmt.Errorf("%w: %w", runner.ErrTimeout, err)
	case errors.Is(err, ErrInvalidCommand):
		return runner.Result{}, fmt.Errorf("%w: %w", runner.ErrInvalidRequest, err)
	case errors.Is(err, ErrStart), errors.Is(err, ErrClosed):
		return runner.Result{}, fmt.Errorf("%w: %w", runner.ErrStart, err)
	}
	return runner.Result{}, err
}

// syncWriter serializes writes from concurrent stdout and stderr copiers.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
