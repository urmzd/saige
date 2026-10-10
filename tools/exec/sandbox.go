// Package exec provides a bash tool that runs shell commands behind a
// Sandbox.
//
// Three layers keep a command in bounds. The Sandbox is the isolation
// boundary: it decides what a running process can reach. The Policy is a
// guardrail checked before anything runs: allowed and denied programs, the
// environment passed through, the network rule, and time limits. The tool is
// always wrapped in a human_approval marker, so a consumer or gate decides
// whether each command runs.
package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"runtime"
	"sync"
	"time"
)

// Command is one shell invocation handed to a Sandbox.
type Command struct {
	// Script is passed to the shell with -c.
	Script string
	// Dir is the absolute working directory.
	Dir string
	// Env is the complete environment as KEY=VALUE pairs. The sandbox adds
	// nothing to it.
	Env []string
	// Timeout bounds the run. Zero means no limit beyond ctx.
	Timeout time.Duration
}

// Result is the outcome of a command that started.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	// TimedOut is true when Timeout ended the command.
	TimedOut bool
	// Truncated is true when output beyond the sandbox's cap was dropped.
	Truncated bool
}

// Isolation reports what a Sandbox enforces.
type Isolation struct {
	// Network is true when commands cannot open network connections.
	Network bool
	// Filesystem is true when commands can change files only under the
	// working directory they are given and in private temporary space, so
	// nothing outside the workspace is touched.
	Filesystem bool
}

// Sandbox runs commands. Run returns an error only when the command could
// not be run; a command that runs and fails reports it in Result.ExitCode.
type Sandbox interface {
	Run(ctx context.Context, cmd Command) (Result, error)
	Isolation() Isolation
}

// ErrNetworkIsolationUnavailable is returned when no wrapper that blocks
// network access is available on this platform.
var ErrNetworkIsolationUnavailable = errors.New("exec: network isolation is not available on this platform")

// DefaultMaxOutputBytes caps stdout and stderr, each, for a Subprocess.
const DefaultMaxOutputBytes = 64 << 10

// Subprocess runs commands as local child processes. It confines nothing by
// itself beyond the environment and working directory it is given; use
// NoNetwork or WithWrapper to add an isolation layer.
type Subprocess struct {
	shell     string
	wrapper   []string
	isolation Isolation
	maxOutput int
}

var _ Sandbox = (*Subprocess)(nil)

// SubprocessOption configures NewSubprocess.
type SubprocessOption func(*Subprocess) error

// WithShell sets the shell binary. The default is bash when it is on PATH,
// otherwise /bin/sh.
func WithShell(path string) SubprocessOption {
	return func(s *Subprocess) error {
		s.shell = path
		return nil
	}
}

// WithMaxOutput caps stdout and stderr, each, at n bytes.
func WithMaxOutput(n int) SubprocessOption {
	return func(s *Subprocess) error {
		if n > 0 {
			s.maxOutput = n
		}
		return nil
	}
}

// WithWrapper runs every command under argv, for example a container or
// namespace launcher, and declares what that wrapper isolates. The shell and
// its arguments are appended to argv.
func WithWrapper(isolation Isolation, argv ...string) SubprocessOption {
	return func(s *Subprocess) error {
		if len(argv) == 0 {
			return errors.New("exec: wrapper argv is empty")
		}
		s.wrapper = append([]string(nil), argv...)
		s.isolation = isolation
		return nil
	}
}

// NoNetwork runs commands under the platform's network-denying wrapper:
// sandbox-exec on macOS and unshare on Linux. It fails with
// ErrNetworkIsolationUnavailable when neither is present.
func NoNetwork() SubprocessOption {
	return func(s *Subprocess) error {
		argv, err := networkWrapper()
		if err != nil {
			return err
		}
		s.wrapper = argv
		s.isolation.Network = true
		return nil
	}
}

func networkWrapper() ([]string, error) {
	switch runtime.GOOS {
	case "darwin":
		if p, err := osexec.LookPath("sandbox-exec"); err == nil {
			return []string{p, "-p", "(version 1)(allow default)(deny network*)"}, nil
		}
	case "linux":
		if p, err := osexec.LookPath("unshare"); err == nil {
			return []string{p, "--net", "--map-root-user", "--"}, nil
		}
	}
	return nil, ErrNetworkIsolationUnavailable
}

// NewSubprocess returns a subprocess sandbox.
func NewSubprocess(opts ...SubprocessOption) (*Subprocess, error) {
	s := &Subprocess{maxOutput: DefaultMaxOutputBytes}
	if p, err := osexec.LookPath("bash"); err == nil {
		s.shell = p
	} else {
		s.shell = "/bin/sh"
	}
	for _, o := range opts {
		if err := o(s); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Isolation implements Sandbox.
func (s *Subprocess) Isolation() Isolation { return s.isolation }

// Run implements Sandbox. The command runs in its own process group, and
// the whole group is killed when the shell exits, when Timeout fires, or
// when ctx is cancelled, so background children do not outlive the call.
func (s *Subprocess) Run(ctx context.Context, c Command) (Result, error) {
	if c.Dir == "" {
		return Result{}, errors.New("exec: working directory is required")
	}
	runCtx := ctx
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}

	argv := append(append([]string(nil), s.wrapper...), s.shell, "-c", c.Script)
	cmd := osexec.CommandContext(runCtx, argv[0], argv[1:]...) //nolint:gosec // running the approved command is the tool's purpose
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.Stdin = nil
	setProcessGroup(cmd)
	cmd.WaitDelay = outputGrace

	// The pipes are handed to the child as files, so Wait returns as soon
	// as the shell exits instead of waiting for every holder of the write
	// ends. The group is then killed and the readers drain what is left.
	stdout := &cappedBuffer{max: s.maxOutput}
	stderr := &cappedBuffer{max: s.maxOutput}
	outR, outW, err := os.Pipe()
	if err != nil {
		return Result{}, fmt.Errorf("exec: %w", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		return Result{}, fmt.Errorf("exec: %w", err)
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	var drained sync.WaitGroup
	drained.Add(2)
	go drain(&drained, outR, stdout)
	go drain(&drained, errR, stderr)

	err = cmd.Start()
	_ = outW.Close()
	_ = errW.Close()
	if err == nil {
		err = cmd.Wait()
		killGroup(cmd)
	}
	waitDrained(&drained, outR, errR)

	res := Result{
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		Truncated: stdout.truncated || stderr.truncated,
		ExitCode:  -1,
	}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if runCtx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		return res, nil
	}
	var exitErr *osexec.ExitError
	if err != nil && !errors.As(err, &exitErr) && !errors.Is(err, osexec.ErrWaitDelay) {
		return res, fmt.Errorf("exec: %w", err)
	}
	return res, nil
}

// outputGrace bounds how long Run waits for output after the shell exits.
// It matters only for a process that left the group and still holds the
// output pipes; the group itself is killed at once.
const outputGrace = 2 * time.Second

func drain(wg *sync.WaitGroup, r io.Reader, dst *cappedBuffer) {
	defer wg.Done()
	_, _ = io.Copy(dst, r)
}

// waitDrained waits for both readers to reach EOF, closing the read ends
// after outputGrace so a process outside the group cannot stall the call.
func waitDrained(wg *sync.WaitGroup, readers ...*os.File) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(outputGrace):
		for _, r := range readers {
			_ = r.Close()
		}
		<-done
	}
	for _, r := range readers {
		_ = r.Close()
	}
}

// cappedBuffer keeps the first max bytes written and records whether more
// arrived.
type cappedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room < len(p) {
		if room > 0 {
			b.buf.Write(p[:room])
		}
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *cappedBuffer) String() string { return b.buf.String() }

// environ returns the parent's value for each name present in it.
func environ(names []string) []string {
	var out []string
	for _, n := range names {
		if v, ok := os.LookupEnv(n); ok {
			out = append(out, n+"="+v)
		}
	}
	return out
}
