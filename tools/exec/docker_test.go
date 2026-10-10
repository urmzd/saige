package exec

import (
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dockerTestImage is small and has sh and python3. SAIGE_TEST_DOCKER_IMAGE
// overrides it.
const dockerTestImage = "python:3.13-alpine"

// newTestDocker returns a Docker sandbox over root, or skips the test when
// Docker or the image is unavailable.
func newTestDocker(t *testing.T, root string, opts ...DockerOption) *Docker {
	t.Helper()
	image := os.Getenv("SAIGE_TEST_DOCKER_IMAGE")
	if image == "" {
		image = dockerTestImage
	}
	d, err := NewDocker(root, append([]DockerOption{DockerImage(image)}, opts...)...)
	if errors.Is(err, ErrDockerUnavailable) {
		t.Skipf("docker unavailable: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if osexec.CommandContext(ctx, d.binary, "image", "inspect", image).Run() != nil {
		if out, err := osexec.CommandContext(ctx, d.binary, "pull", image).CombinedOutput(); err != nil {
			t.Skipf("cannot pull %s: %v %s", image, err, out)
		}
	}
	if err := d.CheckMount(ctx); errors.Is(err, ErrMountNotShared) {
		t.Skipf("set TMPDIR to a directory the docker host shares: %v", err)
	} else if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestNewDockerUnavailable(t *testing.T) {
	_, err := NewDocker(t.TempDir(), DockerBinary("saige-no-such-docker"))
	if !errors.Is(err, ErrDockerUnavailable) {
		t.Fatalf("err = %v, want ErrDockerUnavailable", err)
	}
}

func TestDockerArgs(t *testing.T) {
	root := t.TempDir()
	real, _ := filepath.EvalSymlinks(root)
	d := &Docker{binary: "docker", image: "img", shell: "sh", root: real, memory: "1g", pids: 64}
	args := strings.Join(d.args("n1", "sub", Command{Script: "ls", Env: []string{"PATH=/host/bin", "LANG=C", "SECRET=hunter2-value"}}), " ")
	for _, want := range []string{
		"--network none", "--read-only", "--cap-drop ALL", "--security-opt no-new-privileges",
		"--pids-limit 64", "--memory 1g", "-v " + real + ":/workspace", "-w /workspace/sub",
		"-e LANG", "-e SECRET", "img sh -c ls",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q: %s", want, args)
		}
	}
	if strings.Contains(args, "PATH") || strings.Contains(args, "hunter2-value") {
		t.Errorf("host PATH or a variable's value leaked into the arguments: %s", args)
	}
	if d.Isolation() != (Isolation{Network: true, Filesystem: true}) {
		t.Errorf("isolation = %+v", d.Isolation())
	}
	d.network = true
	if !strings.Contains(strings.Join(d.args("n1", ".", Command{}), " "), "--network bridge") || d.Isolation().Network {
		t.Error("DockerAllowNetwork must use the bridge network and drop network isolation")
	}
}

func TestDockerSandbox(t *testing.T) {
	root := t.TempDir()
	d := newTestDocker(t, root)
	ctx := context.Background()
	run := func(script string, timeout time.Duration) Result {
		t.Helper()
		res, err := d.Run(ctx, Command{Script: script, Dir: root, Env: []string{"GREETING=hi"}, Timeout: timeout})
		if err != nil {
			t.Fatalf("%q: %v", script, err)
		}
		return res
	}

	res := run(`echo "$GREETING" > out.txt; pwd`, time.Minute)
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "/workspace" {
		t.Fatalf("result = %+v", res)
	}
	if b, err := os.ReadFile(filepath.Join(root, "out.txt")); err != nil || string(b) != "hi\n" {
		t.Errorf("workspace file = %q, %v", b, err)
	}

	if res := run("touch /etc/saige-test", time.Minute); res.ExitCode == 0 {
		t.Error("a write outside the workspace must fail")
	}
	if res := run("echo ok > /tmp/x && cat /tmp/x", time.Minute); strings.TrimSpace(res.Stdout) != "ok" {
		t.Errorf("/tmp must be writable: %+v", res)
	}
	netProbe := `python3 -c "import socket; socket.create_connection(('1.1.1.1', 53), timeout=3)"`
	if res := run(netProbe, time.Minute); res.ExitCode == 0 {
		t.Error("a network connection must fail without DockerAllowNetwork")
	}

	start := time.Now()
	res = run("sleep 60", 2*time.Second)
	if !res.TimedOut || time.Since(start) > 30*time.Second {
		t.Errorf("timeout result = %+v after %v", res, time.Since(start))
	}

	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if res, err := d.Run(ctx, Command{Script: "pwd", Dir: sub}); err != nil || strings.TrimSpace(res.Stdout) != "/workspace/sub" {
		t.Errorf("subdirectory run = %+v, %v", res, err)
	}
	if _, err := d.Run(ctx, Command{Script: "pwd", Dir: t.TempDir()}); err == nil {
		t.Error("a directory outside the workspace must be refused")
	}
}

func TestDockerCodeTool(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "nums.txt"), []byte("1\n2\n3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := newTestDocker(t, root)
	tool := newCodeTool(t, d, root, DefaultPolicy())
	if c := tool.Definition().Capability; c != "write" {
		t.Errorf("capability on a confining sandbox = %q, want write", c)
	}
	ctx := context.Background()
	out, err := tool.Execute(ctx, map[string]any{"language": "python", "code": "print(sum(int(x) for x in open('nums.txt')))"})
	if err != nil || !strings.Contains(out, "stdout:\n6\n") {
		t.Errorf("python in docker = %q, %v", out, err)
	}
	out, err = tool.Execute(ctx, map[string]any{"language": "shell", "code": "wc -l < nums.txt"})
	if err != nil || !strings.Contains(out, "3") {
		t.Errorf("shell in docker = %q, %v", out, err)
	}
}
