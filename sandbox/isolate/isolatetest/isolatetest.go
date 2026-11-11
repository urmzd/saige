// Package isolatetest is the conformance suite every isolate.Driver runs:
// the behavior an Instance promises whatever enforces it, and, for drivers
// that isolate, the boundary itself (read-only root and project, no
// network, a non-root user with no capabilities, resource limits, syscalls
// a workload has no use for failing with EPERM, and compartments that
// share only the socket directory).
//
// Commands are POSIX shell; the compartment and syscall checks also need
// python3 (unix sockets, raw syscalls through ctypes) and are skipped where
// it is missing. The syscall checks know x86_64 and aarch64.
package isolatetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/sandbox/isolate"
	"github.com/urmzd/saige/sandbox/workspace"
)

// Options configures Run.
type Options struct {
	// Image is the Spec image; it needs /bin/sh and coreutils-like tools
	// (busybox is enough), and python3 for the compartment checks.
	Image string
	// Env is added to every command's environment (PATH, typically).
	Env map[string]string
	// TempDir makes a host directory the driver can reach, for a driver's
	// own tests beside the suite (a daemon may see only some host
	// directories); nil means t.TempDir.
	TempDir func(t testing.TB) string
	// Isolated runs the boundary checks. Set it for every driver that
	// claims to isolate.
	Isolated bool
}

// Run runs the suite against d.
func Run(t *testing.T, d isolate.Driver, opts Options) {
	s := suite{d: d, o: opts}
	t.Run("ExitCodeAndStreams", s.exitCodeAndStreams)
	t.Run("Stdin", s.stdin)
	t.Run("EnvIsNotInherited", s.envIsNotInherited)
	t.Run("FilesAreTheWorkDirectory", s.filesAreTheWorkDirectory)
	t.Run("ProjectAndMe", s.projectAndMe)
	t.Run("TimeoutEndsTheCommand", s.timeout)
	t.Run("ConcurrentExec", s.concurrent)
	t.Run("Close", s.close)
	t.Run("RejectsInvalid", s.rejectsInvalid)
	t.Run("Compartments", s.compartments)
	if opts.Isolated {
		t.Run("Boundary", s.boundary)
	}
}

type suite struct {
	d isolate.Driver
	o Options
}

func (s suite) start(t *testing.T, spec isolate.Spec) isolate.Instance {
	t.Helper()
	if spec.Image == "" {
		spec.Image = s.o.Image
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	inst, err := s.d.Start(ctx, spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := inst.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return inst
}

// result is one command's outcome.
type result struct {
	code           int
	stdout, stderr string
	err            error
}

func (s suite) sh(inst isolate.Instance, script string, mod ...func(*isolate.Command, *isolate.Stdio)) result {
	cmd := isolate.Command{Argv: []string{"/bin/sh", "-c", script}, Env: map[string]string{}, Timeout: time.Minute}
	for k, v := range s.o.Env {
		cmd.Env[k] = v
	}
	var out, errb bytes.Buffer
	stdio := isolate.Stdio{Stdout: &out, Stderr: &errb}
	for _, m := range mod {
		m(&cmd, &stdio)
	}
	st, err := inst.Exec(context.Background(), cmd, stdio)
	return result{code: st.Code, stdout: out.String(), stderr: errb.String(), err: err}
}

func must(t *testing.T, r result, script string) result {
	t.Helper()
	if r.err != nil || r.code != 0 {
		t.Fatalf("%q: exit %d, err %v, stderr %q", script, r.code, r.err, r.stderr)
	}
	return r
}

func (s suite) exitCodeAndStreams(t *testing.T) {
	inst := s.start(t, isolate.Spec{})
	if inst.ID() == "" {
		t.Fatal("empty instance ID")
	}
	r := s.sh(inst, "echo out; echo err >&2; exit 3")
	if r.err != nil || r.code != 3 || r.stdout != "out\n" || r.stderr != "err\n" {
		t.Fatalf("got %+v", r)
	}
}

func (s suite) stdin(t *testing.T) {
	inst := s.start(t, isolate.Spec{})
	r := must(t, s.sh(inst, "cat", func(_ *isolate.Command, io *isolate.Stdio) {
		io.Stdin = strings.NewReader("from stdin")
	}), "cat")
	if r.stdout != "from stdin" {
		t.Fatalf("stdout %q", r.stdout)
	}
}

func (s suite) envIsNotInherited(t *testing.T) {
	t.Setenv("SANDBOX_ISOLATETEST_HOST", "leak")
	inst := s.start(t, isolate.Spec{})
	r := must(t, s.sh(inst, "env", func(c *isolate.Command, _ *isolate.Stdio) {
		c.Env["GIVEN"] = "multi\nline"
	}), "env")
	if strings.Contains(r.stdout, "SANDBOX_ISOLATETEST_HOST") {
		t.Fatalf("host environment leaked: %q", r.stdout)
	}
	if !strings.Contains(r.stdout, "GIVEN=multi\nline") {
		t.Fatalf("the command's own variable is missing: %q", r.stdout)
	}
}

func (s suite) filesAreTheWorkDirectory(t *testing.T) {
	inst := s.start(t, isolate.Spec{})
	ctx := context.Background()
	if err := inst.Files().Write(ctx, "in/a.txt", strings.NewReader("seeded")); err != nil {
		t.Fatal(err)
	}
	must(t, s.sh(inst, "mkdir -p out && cat in/a.txt > out/b.txt && echo made > top.txt"), "copy")
	rc, err := inst.Files().Read(ctx, "out/b.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "seeded" {
		t.Fatalf("out/b.txt = %q", got)
	}
	keys, err := inst.Files().List(ctx, "")
	if err != nil || strings.Join(keys, ",") != "in/a.txt,out/b.txt,top.txt" {
		t.Fatalf("keys %v, %v", keys, err)
	}
	r := must(t, s.sh(inst, "pwd; cat a.txt", func(c *isolate.Command, _ *isolate.Stdio) { c.Dir = "in" }), "dir")
	if !strings.HasSuffix(strings.SplitN(r.stdout, "\n", 2)[0], "/in") || !strings.HasSuffix(r.stdout, "seeded") {
		t.Fatalf("Dir: stdout %q", r.stdout)
	}
}

// projectAndMe checks that each instance has its own project and person
// directories, empty at the start, that the host fills and reads back.
func (s suite) projectAndMe(t *testing.T) {
	ctx := context.Background()
	inst := s.start(t, isolate.Spec{})
	if err := inst.Project().Write(ctx, "memory/base.txt", strings.NewReader("base")); err != nil {
		t.Fatal(err)
	}
	if err := inst.Me().Write(ctx, "memory/old.txt", strings.NewReader("old")); err != nil {
		t.Fatal(err)
	}
	script := "cat ../project/memory/base.txt ../me/memory/old.txt; echo mine > ../me/memory/note.txt; rm ../me/memory/old.txt"
	if r := must(t, s.sh(inst, script), "project and me"); r.stdout != "baseold" {
		t.Fatalf("stdout %q", r.stdout)
	}
	if got := readKey(t, inst.Me(), "memory/note.txt"); got != "mine\n" {
		t.Fatalf("me/memory/note.txt = %q", got)
	}
	if keys, err := inst.Me().List(ctx, ""); err != nil || strings.Join(keys, ",") != "memory/note.txt" {
		t.Fatalf("me keys %v, %v", keys, err)
	}

	// A second instance of the same spec shares none of it.
	other := s.start(t, isolate.Spec{})
	for name, ws := range map[string]workspace.Workspace{"project": other.Project(), "me": other.Me()} {
		if keys, err := ws.List(ctx, ""); err != nil || len(keys) != 0 {
			t.Errorf("a fresh instance's %s holds %v, %v", name, keys, err)
		}
	}
	if r := s.sh(other, "ls -A ../project ../me"); r.err != nil || r.code != 0 || strings.Contains(r.stdout, "memory") {
		t.Errorf("a fresh instance sees another's files: %+v", r)
	}
}

func readKey(t *testing.T, ws workspace.Workspace, key string) string {
	t.Helper()
	rc, err := ws.Read(context.Background(), key)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (s suite) timeout(t *testing.T) {
	inst := s.start(t, isolate.Spec{})
	begin := time.Now()
	r := s.sh(inst, "sleep 30 & sleep 30; wait", func(c *isolate.Command, _ *isolate.Stdio) { c.Timeout = 500 * time.Millisecond })
	if !errors.Is(r.err, isolate.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", r.err)
	}
	if took := time.Since(begin); took > 20*time.Second {
		t.Fatalf("the timeout took %s to return", took)
	}
	r = must(t, s.sh(inst, "echo still here"), "after timeout")
	if r.stdout != "still here\n" {
		t.Fatalf("stdout %q", r.stdout)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	_, err := inst.Exec(ctx, isolate.Command{Argv: []string{"/bin/sh", "-c", "sleep 30"}, Env: s.o.Env}, isolate.Stdio{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: err = %v", err)
	}
}

func (s suite) concurrent(t *testing.T) {
	inst := s.start(t, isolate.Spec{})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for n := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := s.sh(inst, "echo "+strconv.Itoa(n))
			if r.err != nil || r.stdout != strconv.Itoa(n)+"\n" {
				errs <- fmt.Errorf("exec %d: %+v", n, r)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func (s suite) close(t *testing.T) {
	inst, err := s.d.Start(context.Background(), isolate.Spec{Image: s.o.Image})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := inst.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := inst.Close(ctx); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if r := s.sh(inst, "true"); !errors.Is(r.err, isolate.ErrClosed) {
		t.Fatalf("exec after close: %v", r.err)
	}
}

func (s suite) rejectsInvalid(t *testing.T) {
	ctx := context.Background()
	for name, spec := range map[string]isolate.Spec{
		"no image":  {},
		"root user": {Image: s.o.Image, User: "0"},
	} {
		if inst, err := s.d.Start(ctx, spec); !errors.Is(err, isolate.ErrInvalidSpec) {
			if inst != nil {
				_ = inst.Close(ctx)
			}
			t.Errorf("%s: err = %v, want ErrInvalidSpec", name, err)
		}
	}
	inst := s.start(t, isolate.Spec{})
	for name, cmd := range map[string]isolate.Command{
		"empty argv":     {},
		"escaping dir":   {Argv: []string{"true"}, Dir: "../etc"},
		"no connectors":  {Argv: []string{"true"}, In: isolate.Connectors},
		"bad env":        {Argv: []string{"true"}, Env: map[string]string{"A=B": "c"}},
		"negative limit": {Argv: []string{"true"}, Timeout: -1},
	} {
		if _, err := inst.Exec(ctx, cmd, isolate.Stdio{}); !errors.Is(err, isolate.ErrInvalidCommand) {
			t.Errorf("%s: err = %v, want ErrInvalidCommand", name, err)
		}
	}
	if r := s.sh(inst, "", func(c *isolate.Command, _ *isolate.Stdio) {
		c.Argv = []string{"/no/such/binary"}
	}); !errors.Is(r.err, isolate.ErrStart) {
		t.Errorf("missing binary: err = %v, want ErrStart", r.err)
	}
}

// server is a python3 program that accepts one connection on a unix socket
// in the shared directory and answers it with $REPLY.
const server = `
import os, socket
p = "../run/playscape/c.sock"
s = socket.socket(socket.AF_UNIX)
s.bind(p)
s.listen(1)
open("../run/playscape/ready", "w").close()
c, _ = s.accept()
c.sendall(os.environ["REPLY"].encode())
c.close()
`

const client = `
import socket
s = socket.socket(socket.AF_UNIX)
s.connect("../run/playscape/c.sock")
print(s.recv(100).decode())
`

func (s suite) compartments(t *testing.T) {
	inst := s.start(t, isolate.Spec{Connectors: &isolate.ConnectorSpec{}})
	if r := s.sh(inst, "command -v python3"); r.err != nil || r.code != 0 {
		t.Skip("python3 is not in the image")
	}
	done := make(chan result, 1)
	go func() {
		done <- s.sh(inst, "exec python3 -c '"+server+"'", func(c *isolate.Command, _ *isolate.Stdio) {
			c.In = isolate.Connectors
			c.Env["REPLY"] = "pong-s3cr3t"
		})
	}()
	must(t, s.sh(inst, "i=0; while [ ! -e ../run/playscape/ready ]; do i=$((i+1)); [ $i -gt 300 ] && exit 1; sleep 0.1; done"), "wait for the server")
	r := must(t, s.sh(inst, "python3 -c '"+client+"'"), "client")
	if r.stdout != "pong-s3cr3t\n" {
		t.Fatalf("over the socket: %q", r.stdout)
	}
	must(t, <-done, "server")

	if !s.o.Isolated {
		return
	}
	// The connectors compartment sees none of the workload's files.
	if r := s.sh(inst, "ls /work /project /me", func(c *isolate.Command, _ *isolate.Stdio) { c.In = isolate.Connectors }); r.err != nil || r.code == 0 {
		t.Errorf("connectors see the workload's mounts: %+v", r)
	}
	// The workload cannot read a connector's environment.
	go s.sh(inst, "sleep 30", func(c *isolate.Command, _ *isolate.Stdio) {
		c.In = isolate.Connectors
		c.Env["CONNECTOR_SECRET"] = "s3cr3t"
	})
	time.Sleep(time.Second)
	if r := s.sh(inst, "cat /proc/*/environ 2>/dev/null | tr '\\0' '\\n'; ps -o args 2>/dev/null || ps"); strings.Contains(r.stdout, "s3cr3t") || strings.Contains(r.stdout, "sleep 30") {
		t.Errorf("the workload sees connector processes: %q", r.stdout)
	}
}

func (s suite) boundary(t *testing.T) {
	res := isolate.Resources{CPUMillis: 500, MemoryBytes: 64 << 20, PIDs: 64, DiskBytes: 8 << 20}
	inst := s.start(t, isolate.Spec{Resources: res})
	if err := inst.Project().Write(context.Background(), "base.txt", strings.NewReader("base")); err != nil {
		t.Fatal(err)
	}
	checks := []struct{ name, script string }{
		{"root filesystem is read only", "! touch /etc/x 2>/dev/null"},
		{"project is read only", "! touch ../project/x 2>/dev/null && ! rm -f ../project/base.txt 2>/dev/null && [ -e ../project/base.txt ]"},
		{"user is not root", `[ "$(id -u)" != 0 ]`},
		{"no capabilities", `grep -q '^CapEff:[[:space:]]*0000000000000000$' /proc/self/status`},
		{"no new privileges", `grep -q '^NoNewPrivs:[[:space:]]*1$' /proc/self/status`},
		// /proc/net/dev, not /sys/class/net, which gVisor does not have.
		{"loopback only", `[ "$(awk -F: 'NR > 2 { gsub(/ /, "", $1); print $1 }' /proc/net/dev)" = lo ]`},
		// By behavior, whichever cgroup enforces it: of 100 processes,
		// fewer than 100 start, and the checks after this one show the
		// instance survived. Only builtins run after the forks, which
		// may have used up the limit.
		{"pids limit", `rm -f /tmp/pids; ( i=0; while [ $i -lt 100 ]; do sleep 30 & echo $! >> /tmp/pids; i=$((i+1)); done ) 2>/dev/null; n=0; while read p; do kill $p; n=$((n+1)); done < /tmp/pids; [ $n -lt 100 ]`},
		// cgroup v2, or the v1 layout gVisor presents.
		{"memory limit", `f=/sys/fs/cgroup/memory.max; [ -e $f ] || f=/sys/fs/cgroup/memory/memory.limit_in_bytes; [ "$(cat $f)" = 67108864 ]`},
		{"scratch is bounded", `! dd if=/dev/zero of=/tmp/fill bs=1048576 count=16 2>/dev/null`},
	}
	for _, c := range checks {
		if r := s.sh(inst, c.script); r.err != nil || r.code != 0 {
			t.Errorf("%s: %q: exit %d, err %v, stderr %q", c.name, c.script, r.code, r.err, r.stderr)
		}
	}
	t.Run("DeniedSyscalls", func(t *testing.T) { s.deniedSyscalls(t, inst) })
}

// syscalls calls each syscall a workload is denied, with arguments that
// would not fail with EPERM were it allowed where that is possible (ptrace
// TRACEME, process_vm_readv of itself and personality succeed; most others
// would fail with EFAULT or EBADF), and prints every one that did not fail
// with EPERM. Numbers are x86_64 and aarch64 (the generic table).
const syscalls = `
import ctypes, errno, os, platform, sys
nr = {
    "ptrace": (101, 117), "process_vm_readv": (310, 270), "kcmp": (312, 272),
    "unshare": (272, 97), "setns": (308, 268), "mount": (165, 40),
    "umount2": (166, 39), "pivot_root": (155, 41), "bpf": (321, 280),
    "keyctl": (250, 219), "add_key": (248, 217), "request_key": (249, 218),
    "perf_event_open": (298, 241), "userfaultfd": (323, 282),
    "kexec_load": (246, 104), "init_module": (175, 105),
    "finit_module": (313, 273), "delete_module": (176, 106),
    "open_by_handle_at": (304, 265), "name_to_handle_at": (303, 264),
    "swapon": (167, 224), "swapoff": (168, 225), "reboot": (169, 142),
    "acct": (163, 89), "personality": (135, 92),
}
args = {
    "ptrace": (0, 0, 0, 0), "process_vm_readv": (os.getpid(), 0, 0, 0, 0, 0),
    "kcmp": (os.getpid(), os.getpid(), 0, 0, 0), "unshare": (0x10000000,),
    "setns": (-1, 0), "keyctl": (0, -3, 0), "perf_event_open": (0, 0, -1, -1, 0),
    "finit_module": (-1, 0, 0), "open_by_handle_at": (-1, 0, 0),
    "name_to_handle_at": (-1, 0, 0, 0, 0), "personality": (0x0040000,),
}
arch = {"x86_64": 0, "aarch64": 1}.get(platform.machine())
if arch is None:
    print("unknown machine " + platform.machine())
    sys.exit(2)
libc = ctypes.CDLL(None, use_errno=True)
libc.syscall.restype = ctypes.c_long
bad = []
for name, n in sorted(nr.items()):
    ctypes.set_errno(0)
    a = [ctypes.c_long(v) for v in args.get(name, (0, 0, 0, 0, 0))]
    rc = libc.syscall(ctypes.c_long(n[arch]), *a)
    e = ctypes.get_errno()
    if rc != -1 or e != errno.EPERM:
        bad.append("%s=%d/%s" % (name, rc, errno.errorcode.get(e, e)))
print(" ".join(bad))
sys.exit(1 if bad else 0)
`

func (s suite) deniedSyscalls(t *testing.T, inst isolate.Instance) {
	if r := s.sh(inst, "command -v python3"); r.err != nil || r.code != 0 {
		t.Skip("python3 is not in the image")
	}
	r := s.sh(inst, "exec python3 -c '"+syscalls+"'")
	if r.err != nil || r.code != 0 {
		t.Errorf("syscalls not denied with EPERM: %s (exit %d, err %v, stderr %q)", strings.TrimSpace(r.stdout), r.code, r.err, r.stderr)
	}
}
