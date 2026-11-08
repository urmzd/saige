// Package isolate defines the sandbox an agent run's tools execute in: an
// Instance, started by a Driver from a Spec, that runs commands behind a
// process isolation boundary and is destroyed when the run ends. Pool keeps
// started instances warm and leases each one to exactly one run.
//
// An instance mirrors a small filesystem layout. The run's own files are at
// WorkDir, read and write, and are what Files exposes to the host. A
// project's base snapshot is at ProjectDir, read only inside, and a person's
// volume at MeDir, read and write. Both start empty: each instance has its
// own, and the host fills them through Project and Me after the lease, so
// they never change what a spec is or which warm instance it gets.
// SocketDir is the one directory the workload and its connectors
// compartment share. Commands start in WorkDir, so the other mounts are also reachable as "../project", "../me" and "../run/playscape",
// which is how drivers without a mount namespace (package isolate/local)
// offer the same layout.
//
// The zero value of every Spec field is the most restrictive choice: no
// network, no host directories mounted, no connectors compartment, a
// non-root user, and small default resource limits. Nothing is unlimited.
//
// Orthogonality boundary: isolate knows nothing about tools, queues, nodes
// or the workspace policy (pkg/sandbox decides which workspace keys a tool
// may touch; isolate decides what a process can reach on the machine).
// Executor adapts an Instance into a runner.Executor; that is the only place
// instances meet runs.
package isolate

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/urmzd/saige/sandbox/workspace"
)

// Paths inside an instance.
const (
	// WorkDir holds the run's own files, read and write. Commands start
	// here.
	WorkDir = "/work"
	// ProjectDir holds the project's base snapshot, read only.
	ProjectDir = "/project"
	// MeDir holds the requester's volume, read and write.
	MeDir = "/me"
	// SocketDir is the directory the workload and connectors compartments
	// share, meant for unix sockets. It is the only thing they share.
	SocketDir = "/run/playscape"
)

// Defaults applied to zero Resources fields. They are deliberately small:
// a run that needs more must ask for it.
const (
	DefaultCPUMillis   = 1000
	DefaultMemoryBytes = 512 << 20
	DefaultPIDs        = 256
	DefaultDiskBytes   = 256 << 20
)

// DefaultTimeout bounds a command whose Command.Timeout is zero.
const DefaultTimeout = 5 * time.Minute

// LabelPrefix is reserved for the labels drivers set themselves; Spec.Labels
// may not use it.
const LabelPrefix = "playscape."

var (
	// ErrInvalidSpec is returned for a Spec that fails Validate.
	ErrInvalidSpec = errors.New("isolate: invalid spec")
	// ErrInvalidCommand is returned for a Command that fails Validate.
	ErrInvalidCommand = errors.New("isolate: invalid command")
	// ErrUnsupported is returned when a driver cannot enforce part of a
	// valid Spec. A driver never quietly starts an instance weaker than
	// its Spec asks for.
	ErrUnsupported = errors.New("isolate: unsupported by this driver")
	// ErrStart is returned when an instance, or a command in one, could
	// not be started.
	ErrStart = errors.New("isolate: start failed")
	// ErrTimeout is returned when a command exceeds its timeout.
	ErrTimeout = errors.New("isolate: timeout")
	// ErrClosed is returned by an instance or pool used after Close.
	ErrClosed = errors.New("isolate: closed")
)

// Driver starts instances. Implementations must be safe for concurrent use.
type Driver interface {
	// Start validates spec and starts one instance enforcing all of it,
	// or fails with ErrInvalidSpec, ErrUnsupported or ErrStart. The
	// caller owns the instance and must Close it.
	Start(ctx context.Context, spec Spec) (Instance, error)
}

// Instance is one started sandbox. It belongs to one run and is never
// handed to another. Implementations must be safe for concurrent use.
type Instance interface {
	// ID names the instance, unique per driver.
	ID() string
	// Exec runs cmd to completion inside the instance. A non-zero exit
	// is an ExitStatus, not an error; errors mean the command could not
	// start (ErrStart, ErrInvalidCommand), timed out (ErrTimeout), ctx
	// ended, or the instance is closed (ErrClosed).
	Exec(ctx context.Context, cmd Command, stdio Stdio) (ExitStatus, error)
	// Files is the instance's work directory as a workspace, for the
	// host to seed inputs and collect outputs. It is valid until Close.
	Files() workspace.Workspace
	// Project is the instance's ProjectDir as a workspace. It starts
	// empty. The host writes it; the workload can only read it. It is
	// valid until Close, which removes it.
	Project() workspace.Workspace
	// Me is the instance's MeDir as a workspace. It starts empty. The
	// host fills it with the person's files and reads back what the run
	// changed. It is valid until Close, which removes it.
	Me() workspace.Workspace
	// Close stops every process in the instance and destroys it with its
	// work directory. Calling it again returns the first result.
	Close(ctx context.Context) error
}

// Spec describes an instance. Its zero value, given an Image, is the most
// restrictive instance a driver can start.
type Spec struct {
	// Image is the root filesystem: a container image for container
	// drivers. Required. The root filesystem is always read only.
	Image string
	// Resources bounds the workload compartment.
	Resources Resources
	// Network is the workload's network. The zero value is none at all.
	Network Network
	// User is the numeric "uid" or "uid:gid" processes run as. It may not
	// be root (0). Empty means the driver's default non-root user.
	User string
	// Labels are attached to the instance for the operator (for example
	// to find and remove leaked instances). Keys may not start with
	// LabelPrefix.
	Labels map[string]string
	// Connectors, when set, adds a second compartment for the person's
	// tool servers. It shares SocketDir with the workload and nothing
	// else: no files, no processes, no network.
	Connectors *ConnectorSpec
}

// Resources bounds a compartment. A zero field means its Default value;
// there is no way to ask for unlimited.
type Resources struct {
	// CPUMillis is CPU time in thousandths of a core.
	CPUMillis int64
	// MemoryBytes caps memory, with no swap beyond it.
	MemoryBytes int64
	// PIDs caps the number of processes and threads.
	PIDs int64
	// DiskBytes caps scratch space (the temporary directory). The work
	// directory is a host directory; see the driver for whether it is
	// bounded.
	DiskBytes int64
}

// WithDefaults returns r with every zero field set to its Default.
func (r Resources) WithDefaults() Resources {
	if r.CPUMillis == 0 {
		r.CPUMillis = DefaultCPUMillis
	}
	if r.MemoryBytes == 0 {
		r.MemoryBytes = DefaultMemoryBytes
	}
	if r.PIDs == 0 {
		r.PIDs = DefaultPIDs
	}
	if r.DiskBytes == 0 {
		r.DiskBytes = DefaultDiskBytes
	}
	return r
}

func (r Resources) validate() error {
	if r.CPUMillis < 0 || r.MemoryBytes < 0 || r.PIDs < 0 || r.DiskBytes < 0 {
		return fmt.Errorf("%w: negative resource limit", ErrInvalidSpec)
	}
	return nil
}

// Network is a compartment's network. The zero value has no network at all:
// only a loopback interface.
type Network struct {
	// Proxy is the egress gate (package isolate/egress) the compartment
	// reaches the outside through, as "host:port" seen from inside the
	// instance. The compartment gets no route anywhere else; drivers that
	// cannot enforce that return ErrUnsupported. Drivers set no proxy
	// variables: the gate's per-lease token is not part of the spec, so
	// the caller adds egress.ProxyEnv to each command.
	Proxy string
}

func (n Network) validate() error {
	if n.Proxy == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(n.Proxy)
	if err == nil {
		_, err = strconv.ParseUint(port, 10, 16)
	}
	if err != nil || host == "" || strings.ContainsAny(host, " /@") {
		return fmt.Errorf("%w: proxy %q: want host:port", ErrInvalidSpec, n.Proxy)
	}
	return nil
}

// ConnectorSpec describes the connectors compartment: the person's tool
// servers, which hold credentials that code in the workload must not read.
// It gets none of the workload's mounts; it starts in a scratch directory
// of its own.
type ConnectorSpec struct {
	// Image is the compartment's root filesystem; empty means Spec.Image.
	Image string
	// User follows Spec.User's rules; empty means the driver's default.
	User string
	// Resources bounds the compartment, separately from the workload.
	Resources Resources
	// Network is the compartment's network; the zero value is none.
	Network Network
}

// Compartment selects where a Command runs.
type Compartment int

const (
	// Workload is the compartment of the shell and the file tools. It is
	// the default.
	Workload Compartment = iota
	// Connectors is the compartment of the person's tool servers. Only an
	// instance whose Spec has Connectors has it.
	Connectors
)

func (c Compartment) String() string {
	switch c {
	case Workload:
		return "workload"
	case Connectors:
		return "connectors"
	}
	return "compartment(" + strconv.Itoa(int(c)) + ")"
}

// Validate reports whether s is well formed.
func (s Spec) Validate() error {
	if err := validImage(s.Image); err != nil {
		return err
	}
	if err := s.Resources.validate(); err != nil {
		return err
	}
	if err := s.Network.validate(); err != nil {
		return err
	}
	if _, _, err := ParseUser(s.User); err != nil {
		return err
	}
	for k, v := range s.Labels {
		if k == "" || strings.HasPrefix(k, LabelPrefix) || strings.ContainsAny(k, "=\x00\n") || strings.ContainsAny(v, "\x00\n") {
			return fmt.Errorf("%w: label %q", ErrInvalidSpec, k)
		}
	}
	if c := s.Connectors; c != nil {
		if c.Image != "" {
			if err := validImage(c.Image); err != nil {
				return err
			}
		}
		if _, _, err := ParseUser(c.User); err != nil {
			return err
		}
		if err := c.Resources.validate(); err != nil {
			return err
		}
		if err := c.Network.validate(); err != nil {
			return err
		}
	}
	return nil
}

func validImage(image string) error {
	if image == "" {
		return fmt.Errorf("%w: no image", ErrInvalidSpec)
	}
	if strings.HasPrefix(image, "-") || strings.ContainsAny(image, " \t\n\x00") {
		return fmt.Errorf("%w: image %q", ErrInvalidSpec, image)
	}
	return nil
}

// ParseUser parses a Spec user: empty, "uid" or "uid:gid", numeric and not
// root. An empty user returns -1, -1; a missing gid returns -1.
func ParseUser(user string) (uid, gid int, err error) {
	if user == "" {
		return -1, -1, nil
	}
	u, g, hasGID := strings.Cut(user, ":")
	uid, err = strconv.Atoi(u)
	if err != nil || uid <= 0 {
		return 0, 0, fmt.Errorf("%w: user %q: want a numeric, non-root uid", ErrInvalidSpec, user)
	}
	gid = -1
	if hasGID {
		gid, err = strconv.Atoi(g)
		if err != nil || gid <= 0 {
			return 0, 0, fmt.Errorf("%w: user %q: want a numeric, non-root gid", ErrInvalidSpec, user)
		}
	}
	return uid, gid, nil
}

// Key identifies the instances s describes: two specs with the same key
// start interchangeable instances. Zero resources and their defaults have
// the same key.
func (s Spec) Key() string {
	s.Resources = s.Resources.WithDefaults()
	if s.Connectors != nil {
		c := *s.Connectors
		c.Resources = c.Resources.WithDefaults()
		s.Connectors = &c
	}
	raw, err := json.Marshal(s) // map keys are sorted, so this is canonical
	if err != nil {
		panic("isolate: marshal spec: " + err.Error()) // only plain fields
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}

// Command is one command to run in an instance.
type Command struct {
	// Argv is the full argv. It is never passed through a shell unless
	// Argv[0] is one.
	Argv []string
	// Env is the command's environment. Nothing from the host is
	// inherited; container drivers add the image's own environment.
	Env map[string]string
	// Dir is the working directory relative to WorkDir, as a slash
	// separated path; empty is WorkDir itself. It applies to the
	// workload only: a connectors command starts in its own scratch
	// directory.
	Dir string
	// Timeout bounds the command; zero means DefaultTimeout.
	Timeout time.Duration
	// In is the compartment the command runs in.
	In Compartment
}

// Validate reports whether c is well formed.
func (c Command) Validate() error {
	if len(c.Argv) == 0 || c.Argv[0] == "" {
		return fmt.Errorf("%w: empty argv", ErrInvalidCommand)
	}
	for _, a := range c.Argv {
		if strings.ContainsRune(a, 0) {
			return fmt.Errorf("%w: NUL in argv", ErrInvalidCommand)
		}
	}
	if c.Timeout < 0 {
		return fmt.Errorf("%w: negative timeout", ErrInvalidCommand)
	}
	if c.Dir != "" && !workspace.ValidKey(c.Dir) {
		return fmt.Errorf("%w: dir %q: want a relative path inside the work directory", ErrInvalidCommand, c.Dir)
	}
	if c.Dir != "" && c.In != Workload {
		return fmt.Errorf("%w: dir is only for the workload", ErrInvalidCommand)
	}
	if c.In != Workload && c.In != Connectors {
		return fmt.Errorf("%w: unknown compartment %d", ErrInvalidCommand, c.In)
	}
	for k, v := range c.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
			return fmt.Errorf("%w: env %q", ErrInvalidCommand, k)
		}
	}
	return nil
}

// TimeoutOrDefault returns c.Timeout, or DefaultTimeout when it is zero.
func (c Command) TimeoutOrDefault() time.Duration {
	if c.Timeout == 0 {
		return DefaultTimeout
	}
	return c.Timeout
}

// Stdio connects a command's standard streams. A nil Stdin reads as empty;
// nil Stdout or Stderr discards. Stdout and Stderr may be the same writer:
// drivers serialize writes to it.
type Stdio struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// ExitStatus is the outcome of a command that started and exited.
type ExitStatus struct {
	Code int
}

// NewID returns a random instance ID: "i-" and 16 hex digits.
func NewID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand)
	return "i-" + hex.EncodeToString(b[:])
}
