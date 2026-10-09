package exec

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	osexec "os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
)

func TestPrograms(t *testing.T) {
	tests := []struct {
		script string
		want   []string
	}{
		{"ls -la", []string{"ls"}},
		{"cd src && go test ./... | tee out.txt", []string{"cd", "go", "tee"}},
		{"FOO=1 BAR=2 make build; echo done", []string{"make", "echo"}},
		{"echo $(whoami) `id`", []string{"echo", "whoami", "id"}},
		{"/usr/bin/sudo rm -rf /", []string{"sudo"}},
		{"env -i FOO=1 sudo ls", []string{"env", "sudo"}},
		{"timeout 5 curl x", []string{"timeout", "curl"}},
		{"go build 2>&1 &> log", []string{"go"}},
		{"if true; then git status; fi", []string{"true", "git"}},
		{"(cd a; pwd) & wait", []string{"cd", "pwd", "wait"}},
		{"echo hi # sudo in a comment", []string{"echo"}},
		{"ls\nls\ncat f", []string{"ls", "cat"}},
	}
	for _, tt := range tests {
		if got := Programs(tt.script); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Programs(%q) = %v, want %v", tt.script, got, tt.want)
		}
	}
}

func TestCommandPolicyCheck(t *testing.T) {
	def := DefaultPolicy().Commands
	allow := CommandPolicy{Allow: []string{"go", "git", "ls"}, Deny: []string{"git"}}
	tests := []struct {
		name   string
		policy CommandPolicy
		script string
		denied bool
	}{
		{"default allows ordinary tools", def, "go test ./... && ls", false},
		{"default denies sudo", def, "ls; sudo reboot", true},
		{"default denies sudo by path", def, "/usr/bin/sudo id", true},
		{"default denies through env prefix", def, "env sudo id", true},
		{"allow list permits listed", allow, "go vet ./... | ls", false},
		{"allow list refuses others", allow, "go vet && curl x", true},
		{"deny wins over allow", allow, "git push", true},
		{"substitution is checked", allow, "ls $(curl x)", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.policy.Check(tt.script)
			if got := errors.Is(err, ErrCommandDenied); got != tt.denied {
				t.Errorf("Check(%q) = %v, denied want %v", tt.script, err, tt.denied)
			}
		})
	}
}

func TestEnvPolicyStartsEmpty(t *testing.T) {
	t.Setenv("SAIGE_TEST_SECRET", "s3cret")
	t.Setenv("SAIGE_TEST_KEEP", "kept")
	env := EnvPolicy{Inherit: []string{"SAIGE_TEST_KEEP", "SAIGE_TEST_UNSET"}, Set: map[string]string{"X": "1"}}.environment()
	want := []string{"SAIGE_TEST_KEEP=kept", "X=1"}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("environment = %v, want %v", env, want)
	}
}

// fakeSandbox records the command it is asked to run.
type fakeSandbox struct {
	isolation Isolation
	got       Command
	res       Result
	err       error
}

func (f *fakeSandbox) Run(_ context.Context, c Command) (Result, error) {
	f.got = c
	return f.res, f.err
}
func (f *fakeSandbox) Isolation() Isolation { return f.isolation }

func TestNewBashTool(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "f")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	isolated := &fakeSandbox{isolation: Isolation{Network: true}}
	open := &fakeSandbox{}
	allowNet := DefaultPolicy()
	allowNet.Network = NetworkAllow
	tests := []struct {
		name    string
		sb      Sandbox
		root    string
		policy  Policy
		wantErr error
	}{
		{name: "isolated sandbox with default policy", sb: isolated, root: root, policy: DefaultPolicy()},
		{name: "zero policy denies network", sb: open, root: root, policy: Policy{}, wantErr: ErrNetworkUnenforced},
		{name: "unisolated sandbox refused under deny", sb: open, root: root, policy: DefaultPolicy(), wantErr: ErrNetworkUnenforced},
		{name: "unisolated sandbox with network allowed", sb: open, root: root, policy: allowNet},
		{name: "no sandbox", sb: nil, root: root, policy: allowNet, wantErr: errAny},
		{name: "no root", sb: open, root: "", policy: allowNet, wantErr: errAny},
		{name: "root is a file", sb: open, root: file, policy: allowNet, wantErr: errAny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool, err := NewBashTool(tt.sb, tt.root, tt.policy)
			switch {
			case tt.wantErr == errAny:
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			case err != nil:
				t.Fatal(err)
			}
			mt, ok := tool.(*types.MarkedTool)
			if !ok || len(mt.Markers) != 1 || mt.Markers[0].Kind != ApprovalKind {
				t.Fatalf("bash must carry one %s marker, got %#v", ApprovalKind, tool)
			}
			if c := tool.Definition().Capability; c != types.ToolCapabilityDestructive {
				t.Errorf("capability = %q, want destructive", c)
			}
		})
	}
}

var errAny = errors.New("any error")

func TestBashToolBuildsCommand(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	policy := DefaultPolicy()
	policy.Timeout = time.Minute
	policy.MaxTimeout = 2 * time.Minute
	policy.Env = EnvPolicy{Set: map[string]string{"A": "1"}}
	tests := []struct {
		name        string
		args        map[string]any
		wantDir     string
		wantTimeout time.Duration
		wantErr     bool
	}{
		{name: "defaults", args: map[string]any{"command": "ls"}, wantDir: root, wantTimeout: time.Minute},
		{name: "cwd inside root", args: map[string]any{"command": "ls", "cwd": "sub"}, wantDir: filepath.Join(root, "sub"), wantTimeout: time.Minute},
		{name: "timeout requested", args: map[string]any{"command": "ls", "timeout_seconds": float64(5)}, wantDir: root, wantTimeout: 5 * time.Second},
		{name: "timeout as json number", args: map[string]any{"command": "ls", "timeout_seconds": json.Number("7")}, wantDir: root, wantTimeout: 7 * time.Second},
		{name: "timeout capped", args: map[string]any{"command": "ls", "timeout_seconds": float64(9999)}, wantDir: root, wantTimeout: 2 * time.Minute},
		{name: "cwd escape refused", args: map[string]any{"command": "ls", "cwd": ".."}, wantErr: true},
		{name: "denied program refused", args: map[string]any{"command": "sudo ls"}, wantErr: true},
		{name: "empty command", args: map[string]any{"command": "  "}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb := &fakeSandbox{isolation: Isolation{Network: true}, res: Result{ExitCode: 0, Stdout: "ok"}}
			tool, err := NewBashTool(sb, root, policy)
			if err != nil {
				t.Fatal(err)
			}
			out, err := tool.Execute(context.Background(), tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if sb.got.Script != "" {
					t.Error("a refused command reached the sandbox")
				}
				return
			}
			realDir, _ := filepath.EvalSymlinks(sb.got.Dir)
			wantDir, _ := filepath.EvalSymlinks(tt.wantDir)
			if realDir != wantDir {
				t.Errorf("dir = %q, want %q", sb.got.Dir, tt.wantDir)
			}
			if sb.got.Timeout != tt.wantTimeout {
				t.Errorf("timeout = %v, want %v", sb.got.Timeout, tt.wantTimeout)
			}
			if !reflect.DeepEqual(sb.got.Env, []string{"A=1"}) {
				t.Errorf("env = %v, want only the policy's variables", sb.got.Env)
			}
			if !strings.Contains(out, "exit code: 0") || !strings.Contains(out, "ok") {
				t.Errorf("output = %q", out)
			}
		})
	}
}

func TestSubprocess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	t.Setenv("SAIGE_TEST_SECRET", "s3cret")
	root := t.TempDir()
	sb, err := NewSubprocess(WithMaxOutput(16))
	if err != nil {
		t.Fatal(err)
	}
	policy := DefaultPolicy()
	policy.Network = NetworkAllow
	tool, err := NewBashTool(sb, root, policy)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		args    map[string]any
		want    []string
		wantNot []string
		maxTime time.Duration
	}{
		{name: "stdout and exit code", args: map[string]any{"command": "echo hi"}, want: []string{"exit code: 0", "stdout:\nhi\n"}},
		{name: "non-zero exit is a result", args: map[string]any{"command": "echo oops >&2; exit 3"}, want: []string{"exit code: 3", "stderr:\noops"}},
		{name: "runs in the root", args: map[string]any{"command": "[ \"$(pwd -P)\" = \"$(cd '" + root + "' && pwd -P)\" ] && echo same"}, want: []string{"same"}},
		{name: "secrets are not inherited", args: map[string]any{"command": "echo ${SAIGE_TEST_SECRET:-unset}"}, want: []string{"unset"}, wantNot: []string{"s3cret"}},
		{name: "output is capped", args: map[string]any{"command": "printf '%050d' 0"}, want: []string{"(output truncated)"}},
		{name: "timeout kills the process group", args: map[string]any{"command": "sleep 30 & sleep 30", "timeout_seconds": 0.3}, want: []string{"timed out"}, maxTime: 10 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Now()
			out, err := tool.Execute(context.Background(), tt.args)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if tt.maxTime > 0 && time.Since(start) > tt.maxTime {
				t.Errorf("took %v, want under %v", time.Since(start), tt.maxTime)
			}
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("output missing %q:\n%s", w, out)
				}
			}
			for _, w := range tt.wantNot {
				if strings.Contains(out, w) {
					t.Errorf("output contains %q:\n%s", w, out)
				}
			}
		})
	}
}

func TestSubprocessCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	sb, err := NewSubprocess()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	_, err = sb.Run(ctx, Command{Script: "sleep 30", Dir: t.TempDir(), Env: []string{"PATH=" + os.Getenv("PATH")}})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("cancel took %v", time.Since(start))
	}
}

func TestNoNetworkWrapper(t *testing.T) {
	sb, err := NewSubprocess(NoNetwork())
	if errors.Is(err, ErrNetworkIsolationUnavailable) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !sb.Isolation().Network {
		t.Fatal("NoNetwork must declare network isolation")
	}
	res, err := sb.Run(context.Background(), Command{Script: "echo ok", Dir: t.TempDir(), Env: []string{"PATH=" + os.Getenv("PATH")}})
	if err != nil || strings.TrimSpace(res.Stdout) != "ok" {
		// Unprivileged namespaces may be disabled on this host.
		t.Skipf("network wrapper cannot run here: %v %+v", err, res)
	}
}

func TestNoNetworkBlocksConnections(t *testing.T) {
	if _, err := osexec.LookPath("nc"); err != nil {
		t.Skip("nc not installed")
	}
	isolated, err := NewSubprocess(NoNetwork())
	if err != nil {
		t.Skip(err)
	}
	plain, err := NewSubprocess()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	script := "nc -z -w 2 127.0.0.1 " + port
	cmd := Command{Script: script, Dir: t.TempDir(), Env: []string{"PATH=" + os.Getenv("PATH")}, Timeout: 10 * time.Second}

	res, err := plain.Run(context.Background(), cmd)
	if err != nil || res.ExitCode != 0 {
		t.Skipf("nc cannot reach a local listener here: %v %+v", err, res)
	}
	res, err = isolated.Run(context.Background(), cmd)
	if err != nil {
		t.Skipf("network wrapper cannot run here: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("connection succeeded under NoNetwork: %+v", res)
	}
}
