package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/urmzd/saige/sandbox/isolate"
	"github.com/urmzd/saige/sandbox/workspace"
)

// userOr returns user, or the default when it is empty. A remote daemon's
// containers do not share this machine's users, so it always gets
// DefaultUser.
func (d *Driver) userOr(user string) string {
	if user == "" && d.remote {
		return DefaultUser
	}
	return defaultUser(user)
}

// startRemote starts an instance on a remote daemon: its work, project and
// person directories are volumes on the daemon's machine.
func (d *Driver) startRemote(ctx context.Context, spec isolate.Spec) (isolate.Instance, error) {
	id := d.newID()
	inst := &instance{d: d, id: id, workload: namePrefix + id, remote: true}
	if err := inst.start(ctx, spec, d.userOr(spec.User)); err != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		_ = inst.destroy(cctx)
		return nil, err
	}
	return inst, nil
}

// createVolumes creates a remote instance's work, project and person
// volumes and returns their --mount values. The project volume is mounted
// read and write so the host can fill it as root; the workload's user
// cannot write it, because root owns it.
func (i *instance) createVolumes(ctx context.Context) ([]string, error) {
	var mounts []string
	for _, v := range []struct{ name, target string }{
		{"work", isolate.WorkDir}, {"project", isolate.ProjectDir}, {"me", isolate.MeDir},
	} {
		name := namePrefix + i.id + "-" + v.name
		if _, err := i.d.cli(ctx, nil, "volume", "create", "--driver", "local", "--label", InstanceLabel+"="+i.id, name); err != nil {
			return nil, fmt.Errorf("%w: docker: %s volume: %w", isolate.ErrStart, v.name, err)
		}
		i.volumes = append(i.volumes, name)
		mounts = append(mounts, "type=volume,source="+name+",target="+v.target)
	}
	return mounts, nil
}

// Exit codes the file scripts use.
const (
	exitNotFound = 3
)

// File scripts. Keys reach them as arguments, never in the script text.
// A key must be a regular file, not a link, as with workspace.Local.
const (
	// Files the host writes are readable by the workload whatever the
	// image's umask.
	writeScript  = `umask 022 && mkdir -p -- "$(dirname -- "$1")" && rm -f -- "$1" && exec cat > "$1"`
	readScript   = `[ -f "$1" ] && [ ! -L "$1" ] || exit 3; exec cat -- "$1"`
	deleteScript = `[ -f "$1" ] && [ ! -L "$1" ] || exit 3; exec rm -f -- "$1"`
	listScript   = `exec find . -type f -print0`
)

// Bounds on the file calls.
const (
	filesTimeout = isolate.DefaultTimeout
	maxListBytes = 16 << 20
)

// execFiles is one of a remote instance's directories, reached through its
// workload container: each call is one docker exec of a shell script in
// dir, as the instance's user, or as root for the project volume. The host
// never opens the files, so a link the workload plants is followed, if at
// all, inside the sandbox.
type execFiles struct {
	i    *instance
	dir  string
	root bool // run as root: the project volume, which only root may write
}

var _ workspace.Workspace = execFiles{}

// run runs script in the workload container with args, stdin and stdout,
// and returns the script's exit code. A failure of the CLI or the daemon is
// an error.
func (f execFiles) run(ctx context.Context, stdin io.Reader, stdout io.Writer, script string, args ...string) (int, error) {
	i := f.i
	i.mu.Lock()
	if i.closed {
		i.mu.Unlock()
		return 0, isolate.ErrClosed
	}
	i.running.Add(1)
	i.mu.Unlock()
	defer i.running.Done()

	ctx, cancel := context.WithTimeout(ctx, filesTimeout)
	defer cancel()
	argv := []string{"exec", flagWorkdir, f.dir}
	if f.root {
		argv = append(argv, "--user", "0")
	}
	if stdin != nil {
		argv = append(argv, "--interactive")
	}
	argv = append(argv, i.workload, "/bin/sh", "-c", script, "sh")
	cmd := i.d.command(ctx, append(argv, args...)...)
	cmd.Env = i.d.env
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	errTail := &tailBuffer{max: 4096}
	cmd.Stderr = errTail
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if i.isClosed() {
		return 0, isolate.ErrClosed
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return 0, err
	}
	code := cmd.ProcessState.ExitCode()
	if code != 0 && code != exitNotFound {
		if msg := lastLine(errTail.String()); msg != "" {
			return code, fmt.Errorf("exit %d: %s", code, msg)
		}
		return code, fmt.Errorf("exit %d", code)
	}
	return code, nil
}

func validKey(key string) error {
	if !workspace.ValidKey(key) {
		return fmt.Errorf("%w: %q", workspace.ErrInvalidKey, key)
	}
	return nil
}

// Read implements workspace.Workspace. The file is copied to a temporary
// file on this machine first, so a missing key is known before Read
// returns; closing the reader removes the copy.
func (f execFiles) Read(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp("", "playscape-read-")
	if err != nil {
		return nil, fmt.Errorf("workspace: read %q: %w", key, err)
	}
	code, err := f.run(ctx, nil, tmp, readScript, key)
	if err == nil && code == exitNotFound {
		err = fmt.Errorf("%w: %q", workspace.ErrNotFound, key)
	} else if err != nil {
		err = fmt.Errorf("workspace: read %q: %w", key, err)
	}
	if err == nil {
		_, err = tmp.Seek(0, io.SeekStart)
	}
	if err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return nil, err
	}
	return tempFile{tmp}, nil
}

// tempFile is a temporary file removed when closed.
type tempFile struct{ *os.File }

func (t tempFile) Close() error {
	return errors.Join(t.File.Close(), os.Remove(t.Name()))
}

// Write implements workspace.Workspace. A link at key is replaced, not
// written through.
func (f execFiles) Write(ctx context.Context, key string, r io.Reader) error {
	if err := validKey(key); err != nil {
		return err
	}
	if r == nil {
		r = strings.NewReader("")
	}
	if _, err := f.run(ctx, r, nil, writeScript, key); err != nil {
		return fmt.Errorf("workspace: write %q: %w", key, err)
	}
	return nil
}

// List implements workspace.Workspace: regular files only, as with
// workspace.Local.
func (f execFiles) List(ctx context.Context, prefix string) ([]string, error) {
	if prefix != "" && !workspace.ValidKey(prefix) {
		return nil, fmt.Errorf("%w: %q", workspace.ErrInvalidKey, prefix)
	}
	out := &limitBuffer{max: maxListBytes}
	if _, err := f.run(ctx, nil, out, listScript); err != nil {
		return nil, fmt.Errorf("workspace: list %q: %w", prefix, err)
	}
	if out.over {
		return nil, fmt.Errorf("workspace: list %q: more than %d bytes of names", prefix, maxListBytes)
	}
	keys := []string{}
	for _, name := range bytes.Split(out.buf.Bytes(), []byte{0}) {
		key, ok := strings.CutPrefix(string(name), "./")
		if !ok || !workspace.ValidKey(key) {
			continue
		}
		if prefix == "" || key == prefix || strings.HasPrefix(key, prefix+"/") {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys, nil
}

// Delete implements workspace.Workspace.
func (f execFiles) Delete(ctx context.Context, key string) error {
	if err := validKey(key); err != nil {
		return err
	}
	code, err := f.run(ctx, nil, nil, deleteScript, key)
	if err != nil {
		return fmt.Errorf("workspace: delete %q: %w", key, err)
	}
	if code == exitNotFound {
		return fmt.Errorf("%w: %q", workspace.ErrNotFound, key)
	}
	return nil
}

// limitBuffer keeps up to max bytes and notes whether more were written.
type limitBuffer struct {
	max  int
	buf  bytes.Buffer
	over bool
}

func (l *limitBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); len(p) > room {
		l.over = true
		l.buf.Write(p[:max(room, 0)])
		return len(p), nil
	}
	return l.buf.Write(p)
}
