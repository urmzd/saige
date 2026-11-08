// Package local is an isolate.Driver with NO isolation. Its instances run
// commands as host processes, with the privileges of the program that
// started them, in a temporary directory laid out like an instance. Use it
// in tests and on a developer's machine only; never for code you would not
// run yourself.
//
// What it does give: the instance layout (the work directory, with the
// project, the person volume and the socket directory reachable as
// "../project", "../me" and "../run/playscape"), an environment of exactly
// the command's variables, timeouts that end every process a command
// started, and removal of the directory at Close. What it does not: a
// read-only project directory or root, network limits, a non-root user, resource limits, or any
// separation between the workload and connectors compartments. Every Start
// says so on Driver.Log.
//
// Orthogonality boundary: this package owns host processes and a scratch
// directory, nothing else.
package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/urmzd/saige/sandbox/internal/procgroup"
	"github.com/urmzd/saige/sandbox/isolate"
	"github.com/urmzd/saige/sandbox/workspace"
)

// killGrace is how long a timed-out command's processes have between
// SIGTERM and SIGKILL.
const killGrace = time.Second

// Driver starts unisolated instances. The zero value is ready to use.
type Driver struct {
	// Dir is where instance directories are created; empty means the
	// system temporary directory.
	Dir string
	// Log receives the warning every Start writes; nil means os.Stderr.
	Log io.Writer
}

var _ isolate.Driver = Driver{}

// Start implements isolate.Driver. It validates spec, then lays out a fresh
// directory. It enforces none of the spec's limits and says so on d.Log.
func (d Driver) Start(_ context.Context, spec isolate.Spec) (isolate.Instance, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	id := isolate.NewID()
	root, err := os.MkdirTemp(d.Dir, "playscape-"+id+"-")
	if err != nil {
		return nil, fmt.Errorf("%w: local: %w", isolate.ErrStart, err)
	}
	inst := &instance{id: id, root: root, connectors: spec.Connectors != nil, done: make(chan struct{})}
	if err := inst.layout(); err != nil {
		_ = closeAll(inst.files, inst.project, inst.me)
		_ = os.RemoveAll(root)
		return nil, fmt.Errorf("%w: local: %w", isolate.ErrStart, err)
	}
	log := d.Log
	if log == nil {
		log = os.Stderr
	}
	fmt.Fprintf(log, "sandbox: isolate/local: instance %s is NOT isolated: its commands run as host processes with this program's privileges\n", id)
	return inst, nil
}

type instance struct {
	id         string
	root       string
	connectors bool // the spec asked for a connectors compartment
	files      *workspace.Local
	project    *workspace.Local
	me         *workspace.Local

	mu      sync.Mutex
	closed  bool
	running sync.WaitGroup
	done    chan struct{} // closed by Close: cancels running commands
	once    sync.Once
	err     error
}

func (i *instance) layout() error {
	for _, dir := range []string{"work", "project", "me", "tmp", "connectors", filepath.Join("run", "playscape")} {
		if err := os.MkdirAll(filepath.Join(i.root, dir), 0o700); err != nil {
			return err
		}
	}
	for dir, ws := range map[string]**workspace.Local{"work": &i.files, "project": &i.project, "me": &i.me} {
		l, err := workspace.NewLocal(filepath.Join(i.root, dir))
		if err != nil {
			return err
		}
		*ws = l
	}
	return nil
}

func (i *instance) ID() string                 { return i.id }
func (i *instance) Files() workspace.Workspace { return i.files }

// Project implements isolate.Instance. Nothing stops a command writing it.
func (i *instance) Project() workspace.Workspace { return i.project }

// Me implements isolate.Instance.
func (i *instance) Me() workspace.Workspace { return i.me }

// Exec implements isolate.Instance.
func (i *instance) Exec(ctx context.Context, c isolate.Command, stdio isolate.Stdio) (isolate.ExitStatus, error) {
	if err := c.Validate(); err != nil {
		return isolate.ExitStatus{}, err
	}
	if c.In == isolate.Connectors && !i.connectors {
		return isolate.ExitStatus{}, fmt.Errorf("%w: the instance has no connectors compartment", isolate.ErrInvalidCommand)
	}
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return isolate.ExitStatus{}, isolate.ErrClosed
	}
	i.running.Add(1)
	i.mu.Unlock()
	defer i.running.Done()

	dir := filepath.Join(i.root, "work", filepath.FromSlash(c.Dir))
	if c.In == isolate.Connectors {
		dir = filepath.Join(i.root, "connectors")
	}
	timeout := c.TimeoutOrDefault()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	go func() { // Close ends the command too
		select {
		case <-i.done:
			cancel()
		case <-ctx.Done():
		}
	}()

	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...) //nolint:gosec // running the caller's command is this driver's job
	cmd.Dir = dir
	cmd.Env = make([]string, 0, len(c.Env))
	for k, v := range c.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdin = stdio.Stdin
	cmd.Stdout, cmd.Stderr = streams(stdio)
	cmd.WaitDelay = 2 * time.Second
	procgroup.Configure(cmd, killGrace)

	runErr := cmd.Run()
	if ctx.Err() != nil {
		select {
		case <-i.done:
			return isolate.ExitStatus{}, isolate.ErrClosed
		default:
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return isolate.ExitStatus{}, fmt.Errorf("%w: after %s", isolate.ErrTimeout, timeout)
		}
		return isolate.ExitStatus{}, fmt.Errorf("isolate: local: %w", ctx.Err())
	}
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return isolate.ExitStatus{}, fmt.Errorf("%w: %w", isolate.ErrStart, runErr)
	}
	return isolate.ExitStatus{Code: cmd.ProcessState.ExitCode()}, nil
}

// streams returns the command's stdout and stderr, discarding nil ones.
// When both are the same writer, exec.Cmd serializes writes to it.
func streams(s isolate.Stdio) (io.Writer, io.Writer) {
	out, errw := s.Stdout, s.Stderr
	if out == nil {
		out = io.Discard
	}
	if errw == nil {
		errw = io.Discard
	}
	return out, errw
}

// Close implements isolate.Instance: it ends every running command, waits
// for them, and removes the instance directory with its project and person
// directories.
func (i *instance) Close(context.Context) error {
	i.once.Do(func() {
		i.mu.Lock()
		i.closed = true
		i.mu.Unlock()
		close(i.done)
		i.running.Wait()
		i.err = errors.Join(closeAll(i.files, i.project, i.me), os.RemoveAll(i.root))
		if i.err != nil {
			i.err = fmt.Errorf("isolate: local: close %s: %w", i.id, i.err)
		}
	})
	return i.err
}

// closeAll closes the workspaces that were opened.
func closeAll(ws ...*workspace.Local) error {
	var errs []error
	for _, w := range ws {
		if w != nil {
			errs = append(errs, w.Close())
		}
	}
	return errors.Join(errs...)
}
