package tools

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
	"github.com/urmzd/saige/tools/exec"
)

func testRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("alpha\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// openExec runs code in a plain subprocess with the network allowed, so
// the tests do not depend on a network-isolating wrapper being present.
func openExec(root string, groups ...Group) HarnessOptions {
	return HarnessOptions{Root: root, Groups: groups, Network: exec.NetworkAllow, Languages: []exec.Language{exec.LanguageShell}}
}

func build(t *testing.T, opts HarnessOptions) (*Toolset, map[string]types.Tool) {
	t.Helper()
	set, err := Harness(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]types.Tool{}
	for _, tool := range set.Tools {
		byName[tool.Definition().Name] = tool
	}
	return set, byName
}

func TestHarnessGroups(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	root := testRoot(t)
	scratch := []string{"scratch_write", "scratch_read", "scratch_search"}
	tests := []struct {
		name   string
		groups []Group
		want   []string
	}{
		{"read only by default", nil, append([]string{"read_file", "list_dir", "glob", "grep"}, scratch...)},
		{"write", []Group{GroupWrite}, append([]string{"write_file", "edit_file"}, scratch...)},
		{"exec", []Group{GroupExec}, append([]string{"execute_code"}, scratch...)},
		{"web", []Group{GroupWeb}, append([]string{"fetch_url"}, scratch...)},
		{"all", AllGroups(), append([]string{"read_file", "list_dir", "glob", "grep", "write_file", "edit_file", "execute_code", "fetch_url"}, scratch...)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := openExec(root, tt.groups...)
			set, _ := build(t, opts)
			if got := set.Names(); !slices.Equal(got, tt.want) {
				t.Errorf("names = %v, want %v", got, tt.want)
			}
			if (set.Sandbox != nil) != slices.Contains(tt.groups, GroupExec) {
				t.Errorf("sandbox = %v with groups %v", set.Sandbox, tt.groups)
			}
		})
	}
}

func TestHarnessCapabilitiesAndApproval(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	_, tools := build(t, openExec(testRoot(t), AllGroups()...))
	want := map[string]types.ToolCapability{
		"read_file": types.ToolCapabilityRead, "list_dir": types.ToolCapabilityRead,
		"glob": types.ToolCapabilityRead, "grep": types.ToolCapabilityRead,
		"write_file": types.ToolCapabilityWrite, "edit_file": types.ToolCapabilityWrite,
		// A subprocess does not confine file changes to the workspace.
		"execute_code":  types.ToolCapabilityDestructive,
		"fetch_url":     types.ToolCapabilityRead,
		"scratch_write": types.ToolCapabilityWrite, "scratch_read": types.ToolCapabilityRead, "scratch_search": types.ToolCapabilityRead,
	}
	marked := []string{"write_file", "edit_file", "execute_code"}
	for name, tool := range tools {
		if got := tool.Definition().Capability; got != want[name] {
			t.Errorf("%s capability = %q, want %q", name, got, want[name])
		}
		mt, ok := tool.(*types.MarkedTool)
		if ok != slices.Contains(marked, name) {
			t.Errorf("%s marked = %v", name, ok)
			continue
		}
		if !ok {
			continue
		}
		m := mt.Markers[0]
		if m.Kind != "human_approval" || m.Meta["tool"] != name || !strings.HasSuffix(m.Message, ": "+name) {
			t.Errorf("%s marker = %+v, want an approval naming the harness tool", name, m)
		}
	}
}

func TestHarnessPathConfinement(t *testing.T) {
	root := testRoot(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	_, tools := build(t, HarnessOptions{Root: root, Groups: []Group{GroupRead, GroupWrite}})
	ctx := context.Background()

	if out, err := tools["read_file"].Execute(ctx, map[string]any{"path": "notes.txt"}); err != nil || !strings.Contains(out, "alpha") {
		t.Fatalf("read inside root = %q, %v", out, err)
	}
	refused := []struct {
		tool string
		args map[string]any
	}{
		{"read_file", map[string]any{"path": "../" + filepath.Base(outside) + "/secret.txt"}},
		{"read_file", map[string]any{"path": filepath.Join(outside, "secret.txt")}},
		{"read_file", map[string]any{"path": "escape/secret.txt"}},
		{"list_dir", map[string]any{"path": ".."}},
		{"list_dir", map[string]any{"path": "escape"}},
		{"grep", map[string]any{"pattern": "secret", "path": "escape"}},
		{"glob", map[string]any{"pattern": "*", "path": outside}},
		{"write_file", map[string]any{"path": filepath.Join(outside, "new.txt"), "content": "x"}},
		{"write_file", map[string]any{"path": "escape/new.txt", "content": "x"}},
		{"edit_file", map[string]any{"path": "escape/secret.txt", "old_string": "secret", "new_string": "x"}},
	}
	for _, r := range refused {
		if _, err := tools[r.tool].Execute(ctx, r.args); err == nil {
			t.Errorf("%s %v must be refused", r.tool, r.args)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a write escaped the root")
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "secret.txt")); string(b) != "secret" {
		t.Error("an edit escaped the root")
	}
}

func TestHarnessSpillsLargeOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	root := testRoot(t)
	ws := workspace.NewMemory()
	opts := openExec(root, GroupExec)
	opts.Workspace = ws
	opts.Spill = workspace.SpillOptions{MaxBytes: 4 << 10, PreviewBytes: 512}
	set, tools := build(t, opts)
	if set.Workspace != ws {
		t.Fatal("the toolset must use the workspace it was given")
	}
	ctx := context.Background()

	out, err := tools["execute_code"].Execute(ctx, map[string]any{"language": "shell", "code": "i=0; while [ $i -lt 2000 ]; do echo line-$i; i=$((i+1)); done"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 1024 || !strings.Contains(out, "saige-artifact://") || !strings.Contains(out, "scratch_read") {
		t.Fatalf("large output was not spilled (%d bytes):\n%s", len(out), out)
	}
	refs, err := ws.List(ctx)
	if err != nil || len(refs) != 1 {
		t.Fatalf("workspace refs = %v, %v", refs, err)
	}
	full, err := ws.Read(ctx, refs[0], 0, 0)
	if err != nil || !strings.Contains(string(full), "line-1999") {
		t.Errorf("spilled artifact is incomplete: %d bytes, %v", len(full), err)
	}
	hits, err := tools["scratch_search"].Execute(ctx, map[string]any{"query": "line-1999"})
	if err != nil || !strings.Contains(hits, "line-1999") {
		t.Errorf("scratch_search = %q, %v", hits, err)
	}

	// The marker stays outermost on a spilled tool.
	if _, ok := tools["execute_code"].(*types.MarkedTool); !ok {
		t.Error("spilling must keep the approval marker outermost")
	}

	small, err := tools["execute_code"].Execute(ctx, map[string]any{"language": "shell", "code": "echo small"})
	if err != nil || strings.Contains(small, "saige-artifact://") {
		t.Errorf("small output = %q, %v", small, err)
	}

	opts.NoSpill = true
	_, whole := build(t, opts)
	out, err = whole["execute_code"].Execute(ctx, map[string]any{"language": "shell", "code": "i=0; while [ $i -lt 2000 ]; do echo line-$i; i=$((i+1)); done"})
	if err != nil || !strings.Contains(out, "line-1999") {
		t.Errorf("NoSpill must return the whole result: %d bytes, %v", len(out), err)
	}
}

func TestHarnessOutputCap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	opts := openExec(testRoot(t), GroupExec)
	opts.MaxOutputBytes = 256
	opts.NoSpill = true
	_, tools := build(t, opts)
	out, err := tools["execute_code"].Execute(context.Background(), map[string]any{"language": "shell", "code": "yes abc | head -c 100000"})
	if err != nil || !strings.Contains(out, "(output truncated)") || len(out) > 512 {
		t.Errorf("capped output = %d bytes, %v:\n%s", len(out), err, out)
	}
}

func TestHarnessCore(t *testing.T) {
	set, _ := build(t, HarnessOptions{Root: testRoot(t), Groups: []Group{GroupRead, GroupWrite}})
	if got := set.Core(); !slices.Equal(got, []string{"read_file", "list_dir", "grep", "scratch_read"}) {
		t.Errorf("core = %v", got)
	}
}

func TestHarnessErrors(t *testing.T) {
	root := testRoot(t)
	ctx := context.Background()
	if _, err := Harness(ctx, HarnessOptions{}); !errors.Is(err, ErrNoRoot) {
		t.Errorf("no root err = %v", err)
	}
	if _, err := Harness(ctx, HarnessOptions{Root: filepath.Join(root, "missing")}); err == nil {
		t.Error("a missing root must fail")
	}
	if _, err := Harness(ctx, HarnessOptions{Root: root, Groups: []Group{GroupExec}, SandboxKind: "vm"}); err == nil {
		t.Error("an unknown sandbox must fail")
	}
	if _, err := Harness(ctx, HarnessOptions{Root: root, Groups: []Group{GroupExec}, Network: "sometimes"}); err == nil {
		t.Error("an unknown network policy must fail")
	}
	if _, err := Harness(ctx, HarnessOptions{Root: root, Groups: []Group{GroupExec}, SandboxKind: SandboxDocker, Network: exec.NetworkAllow,
		DockerImage: "saige-test/none:latest"}); err == nil {
		t.Error("a docker sandbox that cannot run must fail")
	}
}

func TestParseGroups(t *testing.T) {
	got, err := ParseGroups(" read, exec ,read,")
	if err != nil || !slices.Equal(got, []Group{GroupRead, GroupExec}) {
		t.Errorf("ParseGroups = %v, %v", got, err)
	}
	if _, err := ParseGroups("read,shell"); err == nil {
		t.Error("an unknown group must fail")
	}
}
