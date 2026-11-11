package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urmzd/saige/sandbox/isolate"
	"github.com/urmzd/saige/sandbox/isolate/isolatetest"
	"github.com/urmzd/saige/sandbox/workspace"
)

// testImage has a shell, busybox tools and python3. Override it with
// SANDBOX_DOCKER_TEST_IMAGE.
const testImage = "python:3.13-alpine"

// The arguments every container starts with carry the whole boundary, so
// they are checked without a daemon.
func TestRunArgsLockTheContainerDown(t *testing.T) {
	d := &Driver{bin: "docker", runtime: RuntimeGVisor, idle: []string{"sleep", "infinity"}}
	args := d.runArgs("i-1", container{
		name: "playscape-i-1", compartment: isolate.Workload, image: "img", user: "1000:1000",
		resources: isolate.Resources{CPUMillis: 250}, network: "none", workDir: "/work", mounts: []string{"type=bind,source=/w,target=/work"},
	}, map[string]string{"team": "a"})
	joined := " " + strings.Join(args, " ") + " "
	for _, want := range []string{
		" --runtime runsc ", " --network none ", " --read-only ", " --cap-drop ALL ",
		" --security-opt no-new-privileges ", " --security-opt seccomp=/dev/stdin ", " --user 1000:1000 ", " --pids-limit 1088 ",
		" --memory 536870912 ", " --memory-swap 536870912 ", " --cpus 0.250 ",
		" --tmpfs /tmp:rw,nosuid,nodev,mode=1777,size=268435456 ", " --init ",
		" --label playscape.instance=i-1 ", " --label playscape.compartment=workload ", " --label team=a ",
		" --mount type=bind,source=/w,target=/work ", " --entrypoint sleep img infinity ",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("run args lack %q:\n%s", want, joined)
		}
	}
}

// Production cannot fall back to a runtime that shares the host kernel
// without saying so.
func TestNewRequiresASandboxRuntime(t *testing.T) {
	bin, err := os.Executable() // any binary: New does not run it
	if err != nil {
		t.Fatal(err)
	}
	for _, rt := range []string{RuntimeRunc, "crun", "io.containerd.runc.v2"} {
		if _, err := New(Config{Binary: bin, Runtime: rt}); !errors.Is(err, ErrInsecureRuntime) {
			t.Errorf("%s: err = %v, want ErrInsecureRuntime", rt, err)
		}
	}
	d, err := New(Config{Binary: bin})
	if err != nil || d.runtime != RuntimeGVisor {
		t.Fatalf("default runtime: %v, %v", d, err)
	}
	var log strings.Builder
	d, err = New(Config{Binary: bin, Runtime: RuntimeRunc, InsecureDevelopmentRuntime: true, Log: &log})
	if err != nil || d.runtime != RuntimeRunc {
		t.Fatalf("opted in: %v, %v", d, err)
	}
	if !strings.Contains(log.String(), "NOT a boundary") {
		t.Fatalf("no warning: %q", log.String())
	}
}

// The profile is Docker's default with the denied syscalls never allowed,
// whatever capabilities a rule is conditioned on.
func TestSeccompProfileDeniesTheListedSyscalls(t *testing.T) {
	var p struct {
		DefaultAction   string `json:"defaultAction"`
		DefaultErrnoRet *int   `json:"defaultErrnoRet"`
		Syscalls        []struct {
			Names  []string          `json:"names"`
			Action string            `json:"action"`
			Args   []json.RawMessage `json:"args"`
		} `json:"syscalls"`
	}
	if err := json.Unmarshal(seccompProfile, &p); err != nil {
		t.Fatal(err)
	}
	if p.DefaultAction != "SCMP_ACT_ERRNO" || p.DefaultErrnoRet == nil || *p.DefaultErrnoRet != 1 {
		t.Fatalf("default: %s %v, want SCMP_ACT_ERRNO with EPERM", p.DefaultAction, p.DefaultErrnoRet)
	}
	allowed := map[string]bool{}
	for _, r := range p.Syscalls {
		for _, n := range r.Names {
			if r.Action != "SCMP_ACT_ALLOW" {
				continue
			}
			if slices.Contains(deniedSyscalls, n) {
				t.Errorf("%s is allowed", n)
			}
			if n == "personality" && len(r.Args) == 0 {
				t.Error("personality is allowed with any argument")
			}
			allowed[n] = true
		}
	}
	// What a workload needs is still there.
	for _, n := range []string{"read", "write", "execve", "clone", "openat", "socket", "personality"} {
		if !allowed[n] {
			t.Errorf("%s is not allowed", n)
		}
	}
}

// Under gVisor the host cgroup's PIDs limit bounds gVisor's own threads,
// so it gets room for them; the workload's limit is set in the sandbox.
func TestHostPIDsLimit(t *testing.T) {
	if got := (&Driver{runtime: RuntimeRunc}).hostPIDs(64); got != 64 {
		t.Errorf("runc: %d", got)
	}
	if got := (&Driver{runtime: RuntimeGVisor}).hostPIDs(64); got != 64*gvisorThreadsPerTask+gvisorThreadsBase {
		t.Errorf("runsc: %d", got)
	}
}

func TestBindRefusesCSVInjection(t *testing.T) {
	if _, err := bind("/a,target=/etc", "/work", false); !errors.Is(err, isolate.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
	m, err := bind("/p", "/project", true)
	if err != nil || m != "type=bind,source=/p,target=/project,readonly" {
		t.Fatalf("%q, %v", m, err)
	}
}

func TestDefaultUserIsNeverRoot(t *testing.T) {
	if got := defaultUser("7:7"); got != "7:7" {
		t.Fatalf("explicit user replaced: %q", got)
	}
	uid, _, err := isolate.ParseUser(defaultUser(""))
	if err != nil || uid <= 0 {
		t.Fatalf("default user %q: %v", defaultUser(""), err)
	}
}

func TestCLIFailures(t *testing.T) {
	for _, c := range []struct {
		code   int
		stderr string
		want   bool
	}{
		{127, "OCI runtime exec failed: exec failed: executable file not found", true},
		{127, "sh: nope: not found", false},
		{1, "Error response from daemon: container x is not running", true},
		{1, "some failure", false},
		{3, "Error response from daemon:", false},
	} {
		if got := cliFailed(c.code, c.stderr); got != c.want {
			t.Errorf("cliFailed(%d, %q) = %v", c.code, c.stderr, got)
		}
	}
}

func TestStartRefusesWhatItCannotEnforce(t *testing.T) {
	d := &Driver{bin: "docker", runtime: RuntimeRunc}
	for _, spec := range []isolate.Spec{
		{Image: "img", Network: isolate.Network{Proxy: "gate:3128"}},
		{Image: "img", Connectors: &isolate.ConnectorSpec{Network: isolate.Network{Proxy: "gate:3128"}}},
	} {
		if _, err := d.Start(context.Background(), spec); !errors.Is(err, isolate.ErrUnsupported) {
			t.Fatalf("a proxy without an egress network: err = %v", err)
		}
	}
}

// dockerDriver returns a driver against a reachable daemon with the test
// image pulled, or skips; with SANDBOX_DOCKER_TEST_REQUIRED set it fails
// instead, so a CI job cannot pass by skipping.
func dockerDriver(t *testing.T, cfg Config) (*Driver, isolatetest.Options) {
	t.Helper()
	skip := t.Skip
	if os.Getenv("SANDBOX_DOCKER_TEST_REQUIRED") != "" {
		skip = t.Fatal
	}
	if testing.Short() {
		skip("docker tests are skipped in -short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		skip("docker is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		skip("the docker daemon is not reachable")
	}
	image := os.Getenv("SANDBOX_DOCKER_TEST_IMAGE")
	if image == "" {
		image = testImage
	}
	if exec.Command("docker", "image", "inspect", image).Run() != nil {
		if out, err := exec.Command("docker", "pull", image).CombinedOutput(); err != nil {
			skip(fmt.Sprintf("cannot pull %s: %v: %s", image, err, out))
		}
	}
	// Docker Desktop and Colima share only some host directories with
	// their VM; the user cache directory is under HOME, which they do.
	base := os.Getenv("SANDBOX_DOCKER_TEST_DIR")
	if base == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			t.Fatal(err)
		}
		base = filepath.Join(cache, "playscape-test")
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	tempDir := func(t testing.TB) string {
		dir, err := os.MkdirTemp(base, "t-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		return dir
	}
	cfg.WorkRoot = tempDir(t)
	// The runtime under test: runc by default, which needs the
	// development opt-in, or SANDBOX_DOCKER_TEST_RUNTIME (runsc in CI).
	if cfg.Runtime == "" {
		cfg.Runtime = os.Getenv("SANDBOX_DOCKER_TEST_RUNTIME")
	}
	if cfg.Runtime == "" {
		cfg.Runtime = RuntimeRunc
	}
	cfg.InsecureDevelopmentRuntime = !sandboxRuntimes[cfg.Runtime]
	cfg.Log = io.Discard
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d.track = func(id string) { started.Store(id, true) }
	return d, isolatetest.Options{
		Image:    image,
		Env:      map[string]string{"PATH": "/usr/local/bin:/usr/bin:/bin"},
		TempDir:  tempDir,
		Isolated: true,
	}
}

func TestConformance(t *testing.T) {
	d, opts := dockerDriver(t, Config{})
	isolatetest.Run(t, d, opts)
	assertNoLeftovers(t, d)
}

// Every container runs under the driver's runtime.
func TestRuntimeIsHonored(t *testing.T) {
	d, opts := dockerDriver(t, Config{})
	inst, err := d.Start(context.Background(), isolate.Spec{Image: opts.Image, Connectors: &isolate.ConnectorSpec{}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inst.Close(context.Background()) }()
	i := inst.(*instance)
	out, err := exec.Command("docker", "inspect", "--format", "{{.HostConfig.Runtime}}", i.workload, i.connectors).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(out)); len(got) != 2 || got[0] != d.runtime || got[1] != d.runtime {
		t.Fatalf("runtimes %q, want %s", got, d.runtime)
	}
	t.Logf("runtime %s", d.runtime)
}

// A runtime the daemon does not have fails the start, so the runtime flag
// is passed and honored rather than silently dropped.
func TestUnknownRuntimeFailsToStart(t *testing.T) {
	d, opts := dockerDriver(t, Config{Runtime: "playscape-no-such-runtime"})
	inst, err := d.Start(context.Background(), isolate.Spec{Image: opts.Image})
	if !errors.Is(err, isolate.ErrStart) {
		if inst != nil {
			_ = inst.Close(context.Background())
		}
		t.Fatalf("err = %v, want ErrStart", err)
	}
	assertNoLeftovers(t, d)
}

func TestCloseRemovesEverything(t *testing.T) {
	d, opts := dockerDriver(t, Config{})
	inst, err := d.Start(context.Background(), isolate.Spec{Image: opts.Image, Connectors: &isolate.ConnectorSpec{}})
	if err != nil {
		t.Fatal(err)
	}
	root := inst.(*instance).root
	if err := inst.Me().Write(context.Background(), "memory/note.md", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if err := inst.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("instance directory survived: %v", err)
	}
	assertNoLeftovers(t, d)
}

// A pool over the real driver: leases are fresh instances, and nothing is
// left once leases are released and the pool is closed.
func TestPoolOverDocker(t *testing.T) {
	d, opts := dockerDriver(t, Config{})
	pool := isolate.NewPool(d, isolate.PoolConfig{Warm: 2, Max: 4})
	spec := isolate.Spec{Image: opts.Image}
	ctx := context.Background()
	seen := map[string]bool{}
	for range 4 {
		l, err := pool.Lease(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		if seen[l.ID()] {
			t.Fatalf("instance %s leased twice", l.ID())
		}
		seen[l.ID()] = true
		st, err := l.Exec(ctx, isolate.Command{Argv: []string{"/bin/sh", "-c", "test ! -e marker && touch marker"}}, isolate.Stdio{})
		if err != nil || st.Code != 0 {
			t.Fatalf("a leased instance was not fresh: %+v, %v", st, err)
		}
		if err := l.Release(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.Close(ctx); err != nil {
		t.Fatal(err)
	}
	assertNoLeftovers(t, d)
}

// started holds every instance ID this test process's drivers made, so
// assertNoLeftovers ignores containers another process (a parallel run in
// another checkout) owns on the same daemon.
var started sync.Map

// assertNoLeftovers checks that no container, volume or work directory of
// an instance this process started is left. The docker tests in one
// process run one at a time, so any of them still there is a leak.
func assertNoLeftovers(t *testing.T, d *Driver) {
	t.Helper()
	for _, args := range [][]string{
		{"ps", "--all", "--filter", "label=" + InstanceLabel, "--format", `{{.Names}} {{.Label "` + InstanceLabel + `"}}`},
		{"volume", "ls", "--filter", "label=" + InstanceLabel, "--format", `{{.Name}} {{.Label "` + InstanceLabel + `"}}`},
	} {
		out, err := exec.Command("docker", args...).Output()
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			name, id, _ := strings.Cut(line, " ")
			if _, ours := started.Load(id); ours {
				t.Errorf("left behind: %s", name)
			}
		}
	}
	if entries, _ := os.ReadDir(d.workRoot); len(entries) > 0 {
		t.Errorf("work directories left behind: %d", len(entries))
	}
}

func TestHostArgs(t *testing.T) {
	tls := t.TempDir()
	for _, f := range []string{"ca.pem", "cert.pem", "key.pem"} {
		if err := os.WriteFile(filepath.Join(tls, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		host, tls string
		want      string // joined flags, or "" for an error
	}{
		{"", "", "  "},
		{"unix:///run/sandbox.sock", "", " --host unix:///run/sandbox.sock "},
		{"ssh://me@sandbox", "", " --host ssh://me@sandbox "},
		{"tcp://sandbox:2376", tls, " --host tcp://sandbox:2376 --tlsverify --tlscacert " + tls + "/ca.pem --tlscert " + tls + "/cert.pem --tlskey " + tls + "/key.pem "},
		// A daemon on plain TCP is root for anyone who reaches it.
		{"tcp://sandbox:2375", "", ""},
		{"tcp://sandbox:2376", t.TempDir(), ""}, // no certificates
		{"tcp://sandbox:2376", "rel", ""},
		{"", tls, ""},
		{"http://sandbox", "", ""},
		{"sandbox:2376", "", ""},
		{"--tlsverify=false", "", ""},
		{"unix:///a b", "", ""},
	} {
		args, err := hostArgs(c.host, c.tls)
		got := " " + strings.Join(args, " ") + " "
		if c.want == "" {
			if err == nil {
				t.Errorf("hostArgs(%q, %q) = %q, want an error", c.host, c.tls, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("hostArgs(%q, %q) = %q, %v, want %q", c.host, c.tls, got, err, c.want)
		}
	}
}

// A remote daemon does not share this machine's users.
func TestRemoteDefaultUser(t *testing.T) {
	d := &Driver{bin: "docker", runtime: RuntimeRunc, remote: true}
	if got := d.userOr(""); got != DefaultUser {
		t.Errorf("remote default user %q, want %s", got, DefaultUser)
	}
}

// remoteDriver is dockerDriver driving the local daemon as if it were
// remote: through its endpoint, with the work directory in a volume.
func remoteDriver(t *testing.T) (*Driver, isolatetest.Options) {
	t.Helper()
	dockerDriver(t, Config{}) // skips without a daemon
	out, err := exec.Command("docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
	if err != nil {
		t.Fatalf("docker context inspect: %v", err)
	}
	d, opts := dockerDriver(t, Config{Host: strings.TrimSpace(string(out))})
	if !d.remote {
		t.Fatal("the driver is not remote")
	}
	return d, opts
}

func TestRemoteConformance(t *testing.T) {
	d, opts := remoteDriver(t)
	isolatetest.Run(t, d, opts)
	assertNoLeftovers(t, d)
}

// Files on a remote instance treat links the workload plants as it does
// not own: never read, listed or written through, and files outlive a
// command that timed out.
func TestRemoteFilesIgnorePlantedLinks(t *testing.T) {
	d, opts := remoteDriver(t)
	ctx := context.Background()
	inst, err := d.Start(ctx, isolate.Spec{Image: opts.Image})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inst.Close(ctx) }()
	sh := func(script string, timeout time.Duration) error {
		st, err := inst.Exec(ctx, isolate.Command{Argv: []string{"/bin/sh", "-c", script}, Env: opts.Env, Timeout: timeout}, isolate.Stdio{})
		if err == nil && st.Code != 0 {
			err = fmt.Errorf("exit %d", st.Code)
		}
		return err
	}
	if err := sh("echo secret > /tmp/target && ln -s /etc/hostname leak && ln -s /tmp/target over && mkdir d && echo x > d/f", time.Minute); err != nil {
		t.Fatal(err)
	}
	files := inst.Files()
	if _, err := files.Read(ctx, "leak"); !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("read through a link: err = %v, want ErrNotFound", err)
	}
	if err := files.Delete(ctx, "leak"); !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("delete a link: err = %v, want ErrNotFound", err)
	}
	if keys, err := files.List(ctx, ""); err != nil || strings.Join(keys, ",") != "d/f" {
		t.Errorf("list = %v, %v, want only d/f", keys, err)
	}
	if keys, err := files.List(ctx, "d"); err != nil || strings.Join(keys, ",") != "d/f" {
		t.Errorf("list d = %v, %v", keys, err)
	}
	if keys, err := files.List(ctx, "dd"); err != nil || len(keys) != 0 {
		t.Errorf("list dd = %v, %v, want none (a sibling prefix)", keys, err)
	}
	if err := files.Write(ctx, "over", strings.NewReader("mine")); err != nil {
		t.Fatal(err)
	}
	if err := sh(`[ ! -L over ] && [ "$(cat over)" = mine ] && [ "$(cat /tmp/target)" = secret ]`, time.Minute); err != nil {
		t.Errorf("write went through the link: %v", err)
	}
	if _, err := files.Read(ctx, "../etc/passwd"); !errors.Is(err, workspace.ErrInvalidKey) {
		t.Errorf("traversal: err = %v", err)
	}
	if _, err := files.Read(ctx, "missing"); !errors.Is(err, workspace.ErrNotFound) {
		t.Errorf("missing: err = %v", err)
	}
	// A timed-out command restarts the container; the volume stays.
	if err := sh("sleep 30", 300*time.Millisecond); !errors.Is(err, isolate.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	rc, err := files.Read(ctx, "over")
	if err != nil {
		t.Fatalf("after a restart: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "mine" {
		t.Fatalf("after a restart: %q", got)
	}
	if err := files.Delete(ctx, "over"); err != nil {
		t.Fatal(err)
	}
	if err := inst.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := files.List(ctx, ""); !errors.Is(err, isolate.ErrClosed) {
		t.Errorf("list after close: err = %v, want ErrClosed", err)
	}
	assertNoLeftovers(t, d)
}
