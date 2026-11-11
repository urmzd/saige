package isolate_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/sandbox/isolate/docker"

	"github.com/urmzd/saige/flow/isolation"
	poolleaser "github.com/urmzd/saige/flow/isolation/isolate"
)

// testImage has sh, the busybox tools and python3.
const testImage = "python:3.13-alpine"

// dockerLeaser returns a Leaser over docker with runc, or skips: these tests
// need a reachable daemon and the test image.
func dockerLeaser(t *testing.T) (*poolleaser.Leaser, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("docker tests are skipped in -short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skip("the docker daemon is not reachable")
	}
	if exec.Command("docker", "image", "inspect", testImage).Run() != nil {
		if out, err := exec.Command("docker", "pull", testImage).CombinedOutput(); err != nil {
			t.Skipf("cannot pull %s: %v: %s", testImage, err, out)
		}
	}
	// Docker Desktop and Colima share directories under HOME with their
	// VM, and not the system temporary directory on macOS.
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(cache, "humagen-test")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, "t-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	// runc, the runtime Docker has on a developer's machine and in CI,
	// which sandbox accepts only as a development runtime.
	d, err := docker.New(docker.Config{Runtime: docker.RuntimeRunc, InsecureDevelopmentRuntime: true, Log: io.Discard, WorkRoot: dir})
	if err != nil {
		t.Fatal(err)
	}
	l, err := poolleaser.New(d, poolleaser.Config{Image: testImage, Labels: map[string]string{"humagen.test": t.Name()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = l.Close(ctx)
	})
	return l, dir
}

func sh(t *testing.T, inst isolation.Instance, in isolation.Compartment, script string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	st, err := inst.Exec(context.Background(), isolation.Command{Argv: []string{"sh", "-c", script}, In: in, Timeout: time.Minute},
		isolation.Stdio{Stdout: &out, Stderr: &out})
	if err != nil {
		t.Fatalf("exec %q: %v", script, err)
	}
	return out.String(), st.Code
}

// A run's shell reaches its work directory and nothing of the host's: not a
// host file beside the work directory, not the host's root, not the network.
// The connectors compartment does not see the work directory.
func TestDockerKeepsARunsToolsInsideItsInstance(t *testing.T) {
	l, root := dockerLeaser(t)
	ctx := context.Background()

	// A host secret next to where the instance's work directory is made.
	secret := filepath.Join(root, "host-secret.txt")
	if err := os.WriteFile(secret, []byte("do-not-read"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := isolation.NewRun(l, isolation.Request{Tenant: "acme", Run: "r1", Connectors: true}, nil)
	defer func() { _ = run.Release(ctx) }()
	inst, err := run.Instance(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if err := inst.Files().Write(ctx, "notes.md", strings.NewReader("seeded")); err != nil {
		t.Fatal(err)
	}
	if out, code := sh(t, inst, isolation.Workload, "cat notes.md && echo made > made.txt && pwd"); code != 0 || !strings.Contains(out, "seeded") || !strings.Contains(out, isolation.WorkDir) {
		t.Fatalf("the workload did not see its work directory: exit %d, %q", code, out)
	}
	r, err := inst.Files().Read(ctx, "made.txt")
	if err != nil {
		t.Fatalf("what the shell wrote is not in the work directory: %v", err)
	}
	_ = r.Close()

	for _, probe := range []string{
		"cat " + secret,
		"cat ../host-secret.txt",
		"ls " + root,
		"touch /etc/written",
		"wget -q -T 3 -O- http://example.com",
	} {
		if out, code := sh(t, inst, isolation.Workload, probe); code == 0 {
			t.Errorf("%q succeeded in the sandbox: %q", probe, out)
		}
	}
	if out, _ := sh(t, inst, isolation.Workload, "id -u"); strings.TrimSpace(out) == "0" {
		t.Error("the workload runs as root")
	}

	// A link the shell makes to a host path is not followed by the host.
	sh(t, inst, isolation.Workload, "ln -s "+secret+" escape.txt; ln -s /etc/passwd passwd.txt")
	for _, k := range []string{"escape.txt", "passwd.txt"} {
		if r, err := inst.Files().Read(ctx, k); err == nil {
			b := new(bytes.Buffer)
			_, _ = b.ReadFrom(r)
			_ = r.Close()
			t.Errorf("reading the link %s from the host returned %q", k, b.String())
		}
	}

	// The connectors compartment shares nothing with the workload but the
	// socket directory.
	if out, code := sh(t, inst, isolation.Connectors, "cat "+isolation.WorkDir+"/notes.md"); code == 0 {
		t.Errorf("the connectors compartment read the work directory: %q", out)
	}
}
