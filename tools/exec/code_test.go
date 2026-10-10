package exec

import (
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urmzd/saige/agent/types"
)

// openPolicy lets a plain Subprocess run, so these tests do not depend on a
// network-isolating wrapper being present.
func openPolicy() Policy {
	p := DefaultPolicy()
	p.Network = NetworkAllow
	return p
}

func newCodeTool(t *testing.T, sb Sandbox, root string, policy Policy, langs ...Language) types.Tool {
	t.Helper()
	tool, err := NewCodeTool(context.Background(), sb, root, policy, CodeOptions{Languages: langs})
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func requireShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
}

func TestCodeToolRunsShellAndPython(t *testing.T) {
	requireShell(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "nums.txt"), []byte("3\n4\n5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sb, err := NewSubprocess()
	if err != nil {
		t.Fatal(err)
	}
	tool := newCodeTool(t, sb, root, openPolicy())
	ctx := context.Background()

	out, err := tool.Execute(ctx, map[string]any{"language": "shell", "code": "wc -l < nums.txt | tr -d ' '; echo err >&2; exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"exit code: 3", "stdout:\n3\n", "stderr:\nerr\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("shell output missing %q:\n%s", want, out)
		}
	}

	if !slices.Contains(tool.Definition().Parameters.Properties["language"].Enum, "python") {
		t.Skip("python is not installed")
	}
	// The code contains a quote, $, and backticks: the heredoc must pass it
	// through without the shell expanding anything.
	code := "import os\nnums = [int(x) for x in open('nums.txt')]\nprint('sum', sum(nums), '$HOME `id`', os.getcwd() == os.path.realpath('.'))"
	out, err = tool.Execute(ctx, map[string]any{"language": "python", "code": code})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "exit code: 0") || !strings.Contains(out, "sum 12 $HOME `id` True") {
		t.Errorf("python output:\n%s", out)
	}
}

func TestCodeToolRunsGo(t *testing.T) {
	requireShell(t)
	if _, err := osexec.LookPath("go"); err != nil {
		t.Skip("go is not installed")
	}
	root := t.TempDir()
	// A go.mod in the workspace must not affect the snippet's build.
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/other\n\ngo 1.99\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sb, err := NewSubprocess()
	if err != nil {
		t.Fatal(err)
	}
	tool := newCodeTool(t, sb, root, openPolicy(), LanguageGo)
	code := "package main\n\nimport (\"fmt\"; \"os\")\n\nfunc main() { _, err := os.Stat(\"go.mod\"); fmt.Println(\"in workspace:\", err == nil) }"
	out, err := tool.Execute(context.Background(), map[string]any{"language": "go", "code": code, "timeout_seconds": 120})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "exit code: 0") || !strings.Contains(out, "in workspace: true") {
		t.Errorf("go output:\n%s", out)
	}
}

func TestCodeToolLimits(t *testing.T) {
	requireShell(t)
	root := t.TempDir()
	sb, err := NewSubprocess(WithMaxOutput(100))
	if err != nil {
		t.Fatal(err)
	}
	policy := openPolicy()
	policy.MaxTimeout = time.Second
	tool := newCodeTool(t, sb, root, policy, LanguageShell)
	ctx := context.Background()

	out, err := tool.Execute(ctx, map[string]any{"language": "shell", "code": "yes x | head -c 5000"})
	if err != nil || !strings.Contains(out, "(output truncated)") || len(out) > 300 {
		t.Errorf("capped output = %q, %v", out, err)
	}

	start := time.Now()
	out, err = tool.Execute(ctx, map[string]any{"language": "shell", "code": "sleep 30", "timeout_seconds": 60})
	if err != nil || !strings.HasPrefix(out, "timed out") {
		t.Errorf("timeout output = %q, %v", out, err)
	}
	if time.Since(start) > 10*time.Second {
		t.Errorf("MaxTimeout did not cap the call: took %v", time.Since(start))
	}

	tests := []struct {
		name string
		args map[string]any
	}{
		{"empty code", map[string]any{"language": "shell", "code": " "}},
		{"language not offered", map[string]any{"language": "python", "code": "print(1)"}},
		{"denied program", map[string]any{"language": "shell", "code": "sudo id"}},
		{"cwd escapes root", map[string]any{"language": "shell", "code": "pwd", "cwd": "../"}},
	}
	for _, tt := range tests {
		if _, err := tool.Execute(ctx, tt.args); err == nil {
			t.Errorf("%s: want an error", tt.name)
		}
	}
	if _, err := tool.Execute(ctx, map[string]any{"language": "shell", "code": "sudo id"}); !errors.Is(err, ErrCommandDenied) {
		t.Errorf("denied program err = %v, want ErrCommandDenied", err)
	}
}

// probeSandbox reports the isolation it is given and records commands.
type probeSandbox struct {
	iso  Isolation
	out  string
	cmds []Command
}

func (f *probeSandbox) Isolation() Isolation { return f.iso }

func (f *probeSandbox) Run(_ context.Context, c Command) (Result, error) {
	f.cmds = append(f.cmds, c)
	return Result{Stdout: f.out}, nil
}

func TestCodeToolCapabilityAndApproval(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name    string
		iso     Isolation
		network NetworkPolicy
		want    types.ToolCapability
		wantErr error
	}{
		{"confined, no network", Isolation{Network: true, Filesystem: true}, NetworkDeny, types.ToolCapabilityWrite, nil},
		{"confined, network allowed", Isolation{Network: true, Filesystem: true}, NetworkAllow, types.ToolCapabilityDestructive, nil},
		{"unconfined files", Isolation{Network: true}, NetworkDeny, types.ToolCapabilityDestructive, nil},
		{"network denied but not enforced", Isolation{}, NetworkDeny, "", ErrNetworkUnenforced},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := DefaultPolicy()
			policy.Network = tt.network
			sb := &probeSandbox{iso: tt.iso, out: "python3\ngo\n"}
			tool, err := NewCodeTool(context.Background(), sb, root, policy, CodeOptions{})
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			mt, ok := tool.(*types.MarkedTool)
			if !ok || len(mt.Markers) != 1 || mt.Markers[0].Kind != ApprovalKind {
				t.Fatalf("execute_code must carry a %s marker", ApprovalKind)
			}
			def := tool.Definition()
			if def.Name != CodeToolName || def.Capability != tt.want {
				t.Errorf("definition = %s/%s, want %s/%s", def.Name, def.Capability, CodeToolName, tt.want)
			}
			if got := def.Parameters.Properties["language"].Enum; !slices.Equal(got, []string{"shell", "python", "go"}) {
				t.Errorf("languages = %v", got)
			}
		})
	}
}

func TestCodeToolLanguageSelection(t *testing.T) {
	root := t.TempDir()
	sb := &probeSandbox{iso: Isolation{Network: true}, out: "python3\n"}
	tool, err := NewCodeTool(context.Background(), sb, root, DefaultPolicy(), CodeOptions{Languages: []Language{LanguageGo, LanguagePython}})
	if err != nil {
		t.Fatal(err)
	}
	if got := tool.Definition().Parameters.Properties["language"].Enum; !slices.Equal(got, []string{"python"}) {
		t.Errorf("languages = %v, want only python: go is missing", got)
	}
	if _, err := NewCodeTool(context.Background(), sb, root, DefaultPolicy(), CodeOptions{Languages: []Language{LanguageGo}}); !errors.Is(err, ErrNoLanguages) {
		t.Errorf("err = %v, want ErrNoLanguages", err)
	}

	// The python wrapper's interpreter passes the command policy.
	policy := DefaultPolicy()
	policy.Commands.Deny = append(policy.Commands.Deny, "python3")
	denied, err := NewCodeTool(context.Background(), sb, root, policy, CodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := denied.Execute(context.Background(), map[string]any{"language": "python", "code": "print(1)"}); !errors.Is(err, ErrCommandDenied) {
		t.Errorf("err = %v, want ErrCommandDenied", err)
	}
}

func TestCodeScriptQuotesCode(t *testing.T) {
	code := "print('SAIGE_CODE_x')\n"
	s, err := codeScript(LanguagePython, code)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.text, "<<'SAIGE_CODE_") || !strings.Contains(s.text, code) {
		t.Errorf("script = %q", s.text)
	}
	if _, err := codeScript("ruby", "puts 1"); err == nil {
		t.Error("unknown language must fail")
	}
}
