// Package isolationtest is a Leaser for tests. Its instances run commands as
// host processes in a temporary directory: there is NO isolation. It records
// every lease, command and close, so a test can check which run leased what,
// which compartment a command ran in, and that every lease was released.
package isolationtest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/urmzd/saige/sandbox/workspace"

	"github.com/urmzd/saige/flow/isolation"
)

// Leaser starts host-process instances under Dir (a temporary directory
// when empty). The zero value is ready to use. It is safe for concurrent use.
type Leaser struct {
	// Dir is where instance directories are made.
	Dir string
	// Fail, when set, fails every lease with it.
	Fail error

	mu        sync.Mutex
	instances []*Instance
	n         atomic.Int64
}

var _ isolation.Leaser = (*Leaser)(nil)

// Lease implements isolation.Leaser.
func (l *Leaser) Lease(ctx context.Context, req isolation.Request) (isolation.Instance, error) {
	if l.Fail != nil {
		return nil, l.Fail
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(l.Dir, "instance-")
	if err != nil {
		return nil, err
	}
	// The layout of an instance: commands start in work, and reach the
	// project and person directories as ../project and ../me.
	inst := &Instance{id: fmt.Sprintf("test-%d", l.n.Add(1)), req: req, dir: dir, work: filepath.Join(dir, "work")}
	for name, ws := range map[string]**workspace.Local{"work": &inst.ws, "project": &inst.project, "me": &inst.me} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
			return nil, err
		}
		if *ws, err = workspace.NewLocal(filepath.Join(dir, name)); err != nil {
			return nil, err
		}
	}
	l.mu.Lock()
	l.instances = append(l.instances, inst)
	l.mu.Unlock()
	return inst, nil
}

// Instances lists every instance leased, in order.
func (l *Leaser) Instances() []*Instance {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*Instance(nil), l.instances...)
}

// Open lists the instances leased and not closed.
func (l *Leaser) Open() []*Instance {
	var out []*Instance
	for _, i := range l.Instances() {
		if !i.Closed() {
			out = append(out, i)
		}
	}
	return out
}

// Exec is one command an instance ran.
type Exec struct {
	Argv []string
	Env  map[string]string
	In   isolation.Compartment
}

// Instance is a host-process instance.
type Instance struct {
	id   string
	req  isolation.Request
	dir  string
	work string
	ws   *workspace.Local
	// project and me are the instance's ProjectDir and MeDir.
	project, me *workspace.Local

	mu     sync.Mutex
	execs  []Exec
	closed bool
}

var _ isolation.Instance = (*Instance)(nil)

// ID implements isolation.Instance.
func (i *Instance) ID() string { return i.id }

// Request is what the instance was leased for.
func (i *Instance) Request() isolation.Request { return i.req }

// WorkDir is the instance's work directory on the host.
func (i *Instance) WorkDir() string { return i.work }

// Execs lists the commands the instance ran.
func (i *Instance) Execs() []Exec {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]Exec(nil), i.execs...)
}

// Closed reports whether the instance was closed.
func (i *Instance) Closed() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.closed
}

// Exec implements isolation.Instance. The command's environment is exactly
// cmd.Env; argv[0] is found on the host's PATH.
func (i *Instance) Exec(ctx context.Context, cmd isolation.Command, stdio isolation.Stdio) (isolation.ExitStatus, error) {
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return isolation.ExitStatus{}, errors.New("isolationtest: closed")
	}
	if cmd.In == isolation.Connectors && !i.req.Connectors {
		i.mu.Unlock()
		return isolation.ExitStatus{}, isolation.ErrNoConnectors
	}
	i.execs = append(i.execs, Exec{Argv: append([]string(nil), cmd.Argv...), Env: cmd.Env, In: cmd.In})
	i.mu.Unlock()
	if len(cmd.Argv) == 0 {
		return isolation.ExitStatus{}, errors.New("isolationtest: empty argv")
	}
	if cmd.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cmd.Timeout)
		defer cancel()
	}
	bin, err := exec.LookPath(cmd.Argv[0])
	if err != nil {
		return isolation.ExitStatus{}, err
	}
	c := exec.CommandContext(ctx, bin, cmd.Argv[1:]...) //nolint:gosec // a test leaser runs the command it is handed
	c.Dir = filepath.Join(i.work, filepath.FromSlash(cmd.Dir))
	if cmd.In == isolation.Connectors {
		c.Dir = i.dir
	}
	keys := make([]string, 0, len(cmd.Env))
	for k := range cmd.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	c.Env = []string{}
	for _, k := range keys {
		c.Env = append(c.Env, k+"="+cmd.Env[k])
	}
	c.Stdin, c.Stdout, c.Stderr = stdio.Stdin, stdio.Stdout, stdio.Stderr
	err = c.Run()
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return isolation.ExitStatus{}, ctx.Err()
	case errors.As(err, &exit):
		return isolation.ExitStatus{Code: exit.ExitCode()}, nil
	case err != nil && strings.Contains(err.Error(), "file already closed"):
		return isolation.ExitStatus{}, nil
	}
	return isolation.ExitStatus{}, err
}

// Files implements isolation.Instance.
func (i *Instance) Files() workspace.Workspace { return i.ws }

// Project implements isolation.Instance. Nothing stops a command writing it.
func (i *Instance) Project() workspace.Workspace { return i.project }

// Me implements isolation.Instance.
func (i *Instance) Me() workspace.Workspace { return i.me }

// HostDir is the instance's directory on the host, holding work, project
// and me.
func (i *Instance) HostDir() string { return i.dir }

// Close implements isolation.Instance. It removes the instance's directory.
func (i *Instance) Close(context.Context) error {
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return nil
	}
	i.closed = true
	i.mu.Unlock()
	_ = i.ws.Close()
	_ = i.project.Close()
	_ = i.me.Close()
	return os.RemoveAll(i.dir)
}
