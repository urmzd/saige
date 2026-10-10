package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
	"github.com/urmzd/saige/agent/workspace"
	"github.com/urmzd/saige/internal/must"
	"github.com/urmzd/saige/tools"
	"github.com/urmzd/saige/tools/exec"
)

// harnessAgent builds an agent with every harness group over root. Code
// runs in a plain subprocess, so the test does not depend on a network
// wrapper being present.
func harnessAgent(t *testing.T, root string, p types.Provider, opts ...Option) *Agent {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	h := tools.HarnessOptions{Root: root, Groups: tools.AllGroups(), Network: exec.NetworkAllow, Languages: []exec.Language{exec.LanguageShell}}
	return must.Get(New(Config{Name: "harness", Provider: p}, append([]Option{WithHarnessTools(h)}, opts...)...))
}

func TestWithHarnessToolsRegistersToolset(t *testing.T) {
	root := t.TempDir()
	extra := &types.ToolFunc{Def: types.ToolDef{Name: "extra"}, Fn: func(context.Context, map[string]any) (string, error) { return "", nil }}
	a := must.Get(New(Config{Name: "h", Tools: types.NewToolRegistry(extra)}, WithHarnessTools(tools.HarnessOptions{Root: root})))
	var names []string
	for _, d := range a.tools.Definitions() {
		names = append(names, d.Name)
	}
	want := []string{"extra", "glob", "grep", "list_dir", "read_file", "scratch_read", "scratch_search", "scratch_write"}
	if !slices.Equal(names, want) {
		t.Errorf("tools = %v, want %v", names, want)
	}
	if a.cfg.Workspace == nil {
		t.Error("the toolset's workspace must become the agent's")
	}

	// An explicit workspace wins, whatever the option order.
	ws := workspace.NewMemory()
	a = must.Get(New(Config{Name: "h"}, WithHarnessTools(tools.HarnessOptions{Root: root}), WithWorkspace(ws)))
	if a.cfg.Workspace != ws {
		t.Error("WithWorkspace must win over the toolset's workspace")
	}

	// No tools unless asked for.
	if n := len(must.Get(New(Config{Name: "plain"})).tools.Definitions()); n != 0 {
		t.Errorf("a plain agent has %d tools, want 0", n)
	}
}

func TestWithHarnessToolsErrorFailsRuns(t *testing.T) {
	a := must.Get(New(Config{Name: "h", Provider: oneCallPerTurn()}, WithHarnessTools(tools.HarnessOptions{})))
	stream := a.Invoke(context.Background(), []types.Message{types.UserMsg(types.Text("hi"))})
	for range stream.Deltas() {
	}
	if err := stream.Wait(); !errors.Is(err, tools.ErrNoRoot) {
		t.Errorf("run err = %v, want ErrNoRoot", err)
	}
	if _, err := a.RunDurable(context.Background(), nil, []types.Message{types.UserMsg(types.Text("hi"))}, a.Tree().Active()); !errors.Is(err, tools.ErrNoRoot) {
		t.Errorf("durable run err = %v, want ErrNoRoot", err)
	}
}

func TestHarnessWriteAndExecNeedApproval(t *testing.T) {
	root := t.TempDir()
	p := oneCallPerTurn(
		call("w1", "write_file", map[string]any{"path": "a.txt", "content": "A"}),
		call("x1", "execute_code", map[string]any{"language": "shell", "code": "echo ran > ran.txt"}),
		call("r1", "read_file", map[string]any{"path": "missing.txt"}),
	)
	a := harnessAgent(t, root, p)
	asked, _ := drive(t, a, func(types.MarkerDelta) Resolution { return Resolution{Approved: false, Message: "no"} })
	if !slices.Equal(asked, []string{"w1", "x1"}) {
		t.Errorf("asked about %v, want the write and the exec, not the read", asked)
	}
	for _, f := range []string{"a.txt", "ran.txt"} {
		if _, err := os.Stat(filepath.Join(root, f)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists after its call was denied", f)
		}
	}
}

func TestHarnessGrantCoversWritesNotUnconfinedExec(t *testing.T) {
	root := t.TempDir()
	p := oneCallPerTurn(
		call("w1", "write_file", map[string]any{"path": "a.txt", "content": "A"}),
		call("w2", "edit_file", map[string]any{"path": "a.txt", "old_string": "A", "new_string": "B"}),
		call("x1", "execute_code", map[string]any{"language": "shell", "code": "cat a.txt > b.txt"}),
		call("x2", "execute_code", map[string]any{"language": "shell", "code": "cat b.txt > c.txt"}),
	)
	a := harnessAgent(t, root, p, WithApprovalPolicy(ApprovalPolicy{}))
	asked, _ := drive(t, a, approveWith(&types.GrantRequest{Scope: types.GrantSession}))
	// The session grant from w1 covers the edit. execute_code runs in a
	// subprocess that can change files anywhere, so it is destructive-class
	// and asked about every time.
	if !slices.Equal(asked, []string{"w1", "x1", "x2"}) {
		t.Errorf("asked about %v, want w1, x1, x2", asked)
	}
	if b, err := os.ReadFile(filepath.Join(root, "c.txt")); err != nil || strings.TrimSpace(string(b)) != "B" {
		t.Errorf("c.txt = %q, %v", b, err)
	}
}

func TestHarnessSpillsIntoAgentWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(strings.Repeat("row of data\n", 4000)), 0o644); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewMemory()
	p := oneCallPerTurn(call("r1", "read_file", map[string]any{"path": "big.txt", "limit": 4000}))
	a := harnessAgent(t, root, p, WithWorkspace(ws))
	_, deltas := drive(t, a, approveWith(nil))
	var result string
	for _, d := range deltas {
		if e, ok := d.(types.ToolExecEndDelta); ok && e.ToolCallID == "r1" {
			result = e.Result
		}
	}
	if !strings.Contains(result, "saige-artifact://") || len(result) > 4<<10 {
		t.Errorf("read result was not spilled (%d bytes)", len(result))
	}
	if refs, err := ws.List(context.Background()); err != nil || len(refs) != 1 {
		t.Errorf("agent workspace refs = %v, %v", refs, err)
	}
}

// confinedSandbox declares the isolation of a container sandbox and counts
// the commands it is given after the language probe.
type confinedSandbox struct{ runs int }

func (s *confinedSandbox) Isolation() exec.Isolation {
	return exec.Isolation{Network: true, Filesystem: true}
}

func (s *confinedSandbox) Run(_ context.Context, c exec.Command) (exec.Result, error) {
	if !strings.Contains(c.Script, "command -v") {
		s.runs++
	}
	return exec.Result{Stdout: "ok\n"}, nil
}

func TestHarnessGrantCoversConfinedExec(t *testing.T) {
	sb := &confinedSandbox{}
	p := oneCallPerTurn(
		call("x1", "execute_code", map[string]any{"language": "shell", "code": "make"}),
		call("x2", "execute_code", map[string]any{"language": "shell", "code": "make test"}),
	)
	h := tools.HarnessOptions{Root: t.TempDir(), Groups: []tools.Group{tools.GroupExec}, Sandbox: sb}
	a := must.Get(New(Config{Name: "h", Provider: p}, WithHarnessTools(h), WithApprovalPolicy(ApprovalPolicy{})))
	asked, _ := drive(t, a, approveWith(&types.GrantRequest{Scope: types.GrantTool}))
	if !slices.Equal(asked, []string{"x1"}) || sb.runs != 2 {
		t.Errorf("asked about %v with %d runs; a tool grant must cover execute_code on a confining sandbox", asked, sb.runs)
	}
}
