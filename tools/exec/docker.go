package exec

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrDockerUnavailable is returned by NewDocker when the docker CLI is not
// on PATH or its daemon does not answer.
var ErrDockerUnavailable = errors.New("exec: docker is not available")

// Docker defaults. Each can be changed with a DockerOption.
const (
	// DefaultDockerImage has a POSIX shell and python3.
	DefaultDockerImage = "python:3.13-slim"
	// DefaultDockerMemory caps a container's memory.
	DefaultDockerMemory = "1g"
	// DefaultDockerPids caps a container's process count.
	DefaultDockerPids = 256
	// dockerMount is where the workspace root appears in the container.
	dockerMount = "/workspace"
)

// hostEnv names variables that describe the host, not the container. They
// are not passed in; the image's own values apply, and HOME is /tmp.
var hostEnv = map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "USER": true, "LOGNAME": true, "SHELL": true}

// Docker runs each command in a fresh container: the workspace root mounted
// read-write at /workspace, everything else read-only except a private
// /tmp, all capabilities dropped, memory and process counts capped, and no
// network unless DockerAllowNetwork is given. The container is removed when
// the command ends, times out, or is cancelled.
//
// Files a command writes in the workspace are owned by the calling user,
// because the container runs with the caller's uid and gid.
type Docker struct {
	binary    string
	image     string
	shell     string
	root      string
	network   bool
	memory    string
	cpus      string
	pids      int
	maxOutput int
}

var _ Sandbox = (*Docker)(nil)

// DockerOption configures NewDocker.
type DockerOption func(*Docker) error

// DockerImage sets the image commands run in. It must provide the shell
// (sh by default) and whatever the commands call.
func DockerImage(image string) DockerOption {
	return func(d *Docker) error {
		if image == "" {
			return errors.New("exec: docker image is empty")
		}
		d.image = image
		return nil
	}
}

// DockerShell sets the shell run with -c inside the container.
func DockerShell(path string) DockerOption {
	return func(d *Docker) error { d.shell = path; return nil }
}

// DockerAllowNetwork gives containers the default bridge network. Without
// it they have no network interface besides loopback.
func DockerAllowNetwork() DockerOption {
	return func(d *Docker) error { d.network = true; return nil }
}

// DockerMemory sets the memory limit, in docker's syntax such as "512m".
func DockerMemory(limit string) DockerOption {
	return func(d *Docker) error { d.memory = limit; return nil }
}

// DockerCPUs sets the CPU limit, such as "1.5".
func DockerCPUs(n string) DockerOption {
	return func(d *Docker) error { d.cpus = n; return nil }
}

// DockerMaxOutput caps stdout and stderr, each, at n bytes.
func DockerMaxOutput(n int) DockerOption {
	return func(d *Docker) error {
		if n > 0 {
			d.maxOutput = n
		}
		return nil
	}
}

// DockerBinary sets the docker CLI to call, for example podman.
func DockerBinary(path string) DockerOption {
	return func(d *Docker) error { d.binary = path; return nil }
}

// NewDocker returns a sandbox that runs commands in containers with root
// mounted as the workspace. It fails with ErrDockerUnavailable when the CLI
// is missing or the daemon does not respond, so a caller can report that
// rather than fall back silently.
func NewDocker(root string, opts ...DockerOption) (*Docker, error) {
	abs, err := realDir(root)
	if err != nil {
		return nil, err
	}
	d := &Docker{
		binary:    "docker",
		image:     DefaultDockerImage,
		shell:     "sh",
		root:      abs,
		memory:    DefaultDockerMemory,
		pids:      DefaultDockerPids,
		maxOutput: DefaultMaxOutputBytes,
	}
	for _, o := range opts {
		if err := o(d); err != nil {
			return nil, err
		}
	}
	bin, err := osexec.LookPath(d.binary)
	if err != nil {
		return nil, fmt.Errorf("%w: %s not found on PATH", ErrDockerUnavailable, d.binary)
	}
	d.binary = bin
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := osexec.CommandContext(ctx, bin, "version", "--format", "{{.Server.Version}}").CombinedOutput() //nolint:gosec // fixed arguments
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrDockerUnavailable, strings.TrimSpace(string(out)))
	}
	return d, nil
}

// ErrMountNotShared is returned by CheckMount when the container does not
// see the workspace root, as happens when the docker host shares only some
// directories with the client, such as a VM that mounts only the home
// directory.
var ErrMountNotShared = errors.New("exec: the docker host does not share the workspace directory")

// CheckMount runs one container to confirm it sees the workspace root as
// the host does. A bind mount of a directory the docker host cannot reach
// gives the container an empty directory, so commands would seem to work
// while their files never reach the workspace.
func (d *Docker) CheckMount(ctx context.Context) error {
	token, err := containerName()
	if err != nil {
		return err
	}
	probe := filepath.Join(d.root, "."+token)
	if err := os.WriteFile(probe, []byte(token), 0o600); err != nil {
		return fmt.Errorf("exec: write mount probe: %w", err)
	}
	defer func() { _ = os.Remove(probe) }()
	res, err := d.Run(ctx, Command{Script: "cat ." + token, Dir: d.root, Timeout: 2 * time.Minute})
	if err != nil {
		return err
	}
	if strings.TrimSpace(res.Stdout) != token {
		return fmt.Errorf("%w: %s (share it with the docker host or use a workspace inside a shared directory)", ErrMountNotShared, d.root)
	}
	return nil
}

// Isolation implements Sandbox.
func (d *Docker) Isolation() Isolation { return Isolation{Network: !d.network, Filesystem: true} }

// Run implements Sandbox. Command.Dir must lie under the workspace root.
func (d *Docker) Run(ctx context.Context, c Command) (Result, error) {
	dir, err := realDir(c.Dir)
	if err != nil {
		return Result{}, err
	}
	rel, err := filepath.Rel(d.root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return Result{}, fmt.Errorf("exec: %s is outside the docker workspace %s", c.Dir, d.root)
	}
	name, err := containerName()
	if err != nil {
		return Result{}, err
	}
	runCtx := ctx
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}

	cmd := osexec.CommandContext(runCtx, d.binary, d.args(name, rel, c)...) //nolint:gosec // running the approved command is the tool's purpose
	// The CLI needs the host environment to find its daemon. Command
	// variables reach the container by name only, so their values are not
	// visible in the process list.
	cmd.Env = append(os.Environ(), containerEnv(c.Env)...)
	cmd.Stdin = nil
	stdout := &cappedBuffer{max: d.maxOutput}
	stderr := &cappedBuffer{max: d.maxOutput}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = outputGrace
	err = cmd.Run()
	if runCtx.Err() != nil {
		// Killing the CLI does not stop the container.
		d.remove(name)
	}

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
	// 125 is docker's own failure, such as a missing image, not the
	// command's.
	if res.ExitCode == 125 {
		return res, fmt.Errorf("exec: docker run: %s", strings.TrimSpace(res.Stderr))
	}
	var exitErr *osexec.ExitError
	if err != nil && !errors.As(err, &exitErr) && !errors.Is(err, osexec.ErrWaitDelay) {
		return res, fmt.Errorf("exec: %w", err)
	}
	return res, nil
}

func (d *Docker) args(name, rel string, c Command) []string {
	network := "none"
	if d.network {
		network = "bridge"
	}
	args := []string{
		"run", "--rm", "--name", name,
		"--network", network,
		"--read-only", "--tmpfs", "/tmp:rw,exec,nosuid,size=512m",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--pids-limit", strconv.Itoa(d.pids),
		"-v", d.root + ":" + dockerMount,
		"-w", filepath.ToSlash(filepath.Join(dockerMount, rel)),
		"-e", "HOME=/tmp",
	}
	if d.memory != "" {
		args = append(args, "--memory", d.memory)
	}
	if d.cpus != "" {
		args = append(args, "--cpus", d.cpus)
	}
	// Getuid is -1 where there are no uids, such as Windows.
	if uid := os.Getuid(); uid >= 0 {
		args = append(args, "--user", strconv.Itoa(uid)+":"+strconv.Itoa(os.Getgid()))
	}
	for _, kv := range containerEnv(c.Env) {
		k, _, _ := strings.Cut(kv, "=")
		args = append(args, "-e", k)
	}
	return append(args, d.image, d.shell, "-c", c.Script)
}

// remove force-removes a container whose command was stopped.
func (d *Docker) remove(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = osexec.CommandContext(ctx, d.binary, "rm", "-f", name).Run() //nolint:gosec // fixed arguments
}

// containerEnv drops variables that name host locations.
func containerEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if ok && k != "" && !hostEnv[k] {
			out = append(out, kv)
		}
	}
	return out
}

func containerName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("exec: %w", err)
	}
	return "saige-exec-" + hex.EncodeToString(b[:]), nil
}

// realDir returns dir as an absolute path with symlinks resolved, after
// checking that it is a directory.
func realDir(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("exec: working directory is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("exec: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("exec: %w", err)
	}
	if info, err := os.Stat(real); err != nil || !info.IsDir() {
		return "", fmt.Errorf("exec: %s is not a directory", dir)
	}
	return real, nil
}
