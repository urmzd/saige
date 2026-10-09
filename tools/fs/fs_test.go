package fs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

// workspace builds a small tree under a temp root.
func workspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"main.go":            "package main\n\nfunc main() {}\n",
		"cmd/app/main.go":    "package main\n// TODO: wire flags\n",
		"cmd/app/app.txt":    "Hello\nhello\n",
		"docs/guide.md":      "# Guide\n",
		".git/config":        "TODO hidden\n",
		"node_modules/x.js":  "TODO vendored\n",
		"min.js":             strings.Repeat("x", 200<<10) + "\nneedle\n",
		"bin.dat":            "TODO\x00binary",
		"lines.txt":          numbered(10),
		"dup.txt":            "a b a b a\n",
		"nested/deep/f.json": "{}\n",
	}
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func numbered(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("|", i))
		b.WriteString("\n")
	}
	return b.String()
}

func toolsByName(t *testing.T, root string, opts ...Option) map[string]types.Tool {
	t.Helper()
	ts, err := NewTools(root, opts...)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]types.Tool{}
	for _, tool := range ts {
		m[tool.Definition().Name] = tool
	}
	return m
}

func TestNewTools(t *testing.T) {
	root := workspace(t)
	file := filepath.Join(root, "main.go")
	tests := []struct {
		name    string
		root    string
		opts    []Option
		want    []string
		wantErr bool
	}{
		{name: "read only by default", root: root, want: []string{"glob", "grep", "read"}},
		{name: "writes opt in", root: root, opts: []Option{AllowWrites()}, want: []string{"edit", "glob", "grep", "read", "write"}},
		{name: "root required", root: "", wantErr: true},
		{name: "root must exist", root: filepath.Join(root, "missing"), wantErr: true},
		{name: "root must be a directory", root: file, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, err := NewTools(tt.root, tt.opts...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			var names []string
			for _, tool := range ts {
				names = append(names, tool.Definition().Name)
			}
			sort.Strings(names)
			got := strings.Join(names, ",")
			if want := strings.Join(tt.want, ","); got != want {
				t.Errorf("tools = %s, want %s", got, want)
			}
		})
	}
	if _, err := NewTools(""); !errors.Is(err, ErrNoRoot) {
		t.Errorf("empty root err = %v, want ErrNoRoot", err)
	}
}

func TestWriteToolsCarryApprovalMarkers(t *testing.T) {
	tools := toolsByName(t, workspace(t), AllowWrites())
	for name, tool := range tools {
		mt, marked := tool.(*types.MarkedTool)
		capability := tool.Definition().Capability
		switch name {
		case "write", "edit":
			if !marked || len(mt.Markers) == 0 || mt.Markers[0].Kind != ApprovalKind {
				t.Errorf("%s must carry a %s marker", name, ApprovalKind)
			}
			if capability != types.ToolCapabilityWrite {
				t.Errorf("%s capability = %q, want write", name, capability)
			}
		default:
			if marked {
				t.Errorf("%s is read-only and must not be marked", name)
			}
			if capability != types.ToolCapabilityRead {
				t.Errorf("%s capability = %q, want read", name, capability)
			}
		}
	}
}

func TestRead(t *testing.T) {
	root := workspace(t)
	read := toolsByName(t, root, WithReadLimit(3))["read"]
	tests := []struct {
		name    string
		args    map[string]any
		want    []string
		wantNot []string
		wantErr bool
	}{
		{name: "first page", args: map[string]any{"path": "lines.txt"}, want: []string{"     1\tline |\n", "lines 1-3 of 10; use offset=4"}, wantNot: []string{"line ||||\n"}},
		{name: "next page", args: map[string]any{"path": "lines.txt", "offset": float64(4)}, want: []string{"     4\t", "use offset=7"}},
		{name: "json number args", args: map[string]any{"path": "lines.txt", "offset": json.Number("9"), "limit": json.Number("5")}, want: []string{"    10\t"}, wantNot: []string{"continue"}},
		{name: "past the end", args: map[string]any{"path": "lines.txt", "offset": float64(99)}, want: []string{"past the end"}},
		{name: "long line is truncated, not fatal", args: map[string]any{"path": "min.js"}, want: []string{"more chars)", "     2\tneedle"}},
		{name: "binary rejected", args: map[string]any{"path": "bin.dat"}, wantErr: true},
		{name: "directory rejected", args: map[string]any{"path": "docs"}, wantErr: true},
		{name: "traversal rejected", args: map[string]any{"path": "../etc/passwd"}, wantErr: true},
		{name: "missing path", args: map[string]any{}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := read.Execute(context.Background(), tt.args)
			checkResult(t, got, err, tt.want, tt.wantNot, tt.wantErr)
			if len(got) > 10<<10 {
				t.Errorf("output is %d bytes; long lines must be truncated", len(got))
			}
		})
	}
}

func TestReadSizeCap(t *testing.T) {
	root := workspace(t)
	read := toolsByName(t, root, WithMaxFileBytes(64))["read"]
	if _, err := read.Execute(context.Background(), map[string]any{"path": "min.js"}); err == nil {
		t.Fatal("expected a size error")
	}
}

func TestGlob(t *testing.T) {
	root := workspace(t)
	glob := toolsByName(t, root)["glob"]
	tests := []struct {
		name    string
		args    map[string]any
		want    []string
		wantNot []string
		wantErr bool
	}{
		{name: "double star", args: map[string]any{"pattern": "**/*.go"}, want: []string{"main.go", "cmd/app/main.go"}},
		{name: "single segment", args: map[string]any{"pattern": "*.go"}, want: []string{"main.go"}, wantNot: []string{"cmd/app/main.go"}},
		{name: "scoped to a directory", args: map[string]any{"pattern": "*", "path": "cmd/app"}, want: []string{"cmd/app/app.txt", "cmd/app/main.go"}, wantNot: []string{"docs/guide.md"}},
		{name: "skips vcs and vendored dirs", args: map[string]any{"pattern": "**/*"}, wantNot: []string{".git/config", "node_modules/x.js"}},
		{name: "no match", args: map[string]any{"pattern": "**/*.rs"}, want: []string{"No files matched."}},
		{name: "bad pattern", args: map[string]any{"pattern": "[x"}, wantErr: true},
		{name: "escape rejected", args: map[string]any{"pattern": "*", "path": ".."}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := glob.Execute(context.Background(), tt.args)
			checkResult(t, got, err, tt.want, tt.wantNot, tt.wantErr)
		})
	}

	capped := toolsByName(t, root, WithMaxResults(2))["glob"]
	got, err := capped.Execute(context.Background(), map[string]any{"pattern": "**/*"})
	checkResult(t, got, err, []string{"stopped at 2 paths"}, nil, false)
}

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern, name string
		want          bool
	}{
		{"**/*.go", "a.go", true},
		{"**/*.go", "a/b/c.go", true},
		{"a/**/c.go", "a/c.go", true},
		{"a/**/c.go", "a/x/y/c.go", true},
		{"a/**", "a/x/y", true},
		{"*.go", "a/b.go", false},
		{"a/?.txt", "a/b.txt", true},
		{"a/[bc].txt", "a/d.txt", false},
	}
	for _, tt := range tests {
		if got := matchGlob(tt.pattern, tt.name); got != tt.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}

func TestGrep(t *testing.T) {
	root := workspace(t)
	grep := toolsByName(t, root)["grep"]
	tests := []struct {
		name    string
		args    map[string]any
		want    []string
		wantNot []string
		wantErr bool
	}{
		{name: "match with line numbers", args: map[string]any{"pattern": "TODO"}, want: []string{"cmd/app/main.go:2: // TODO: wire flags"}, wantNot: []string{".git", "node_modules", "bin.dat:"}},
		{name: "binary file skipped and reported", args: map[string]any{"pattern": "TODO"}, want: []string{"files skipped"}},
		{name: "case sensitive by default", args: map[string]any{"pattern": "^hello", "path": "cmd/app"}, want: []string{"app.txt:2:"}, wantNot: []string{"app.txt:1:"}},
		{name: "ignore case", args: map[string]any{"pattern": "^hello", "path": "cmd/app", "ignore_case": true}, want: []string{"app.txt:1:", "app.txt:2:"}},
		{name: "glob on file name", args: map[string]any{"pattern": "package", "glob": "*.go"}, want: []string{"main.go:1:"}, wantNot: []string{"app.txt"}},
		{name: "glob on path", args: map[string]any{"pattern": "package", "glob": "cmd/**/*.go"}, want: []string{"cmd/app/main.go:1:"}, wantNot: []string{"\nmain.go:1:"}},
		{name: "single file", args: map[string]any{"pattern": "Guide", "path": "docs/guide.md"}, want: []string{"docs/guide.md:1: # Guide"}},
		{name: "match after a long line", args: map[string]any{"pattern": "^needle$"}, want: []string{"min.js:2: needle"}},
		{name: "no match", args: map[string]any{"pattern": "zzz-none"}, want: []string{"No matches found."}},
		{name: "bad regex", args: map[string]any{"pattern": "("}, wantErr: true},
		{name: "escape rejected", args: map[string]any{"pattern": "x", "path": "/etc"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := grep.Execute(context.Background(), tt.args)
			checkResult(t, got, err, tt.want, tt.wantNot, tt.wantErr)
		})
	}

	capped := toolsByName(t, root, WithMaxResults(1))["grep"]
	got, err := capped.Execute(context.Background(), map[string]any{"pattern": "a"})
	checkResult(t, got, err, []string{"stopped at 1 matches"}, nil, false)
}

func TestWrite(t *testing.T) {
	root := workspace(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	write := toolsByName(t, root, AllowWrites(), WithMaxFileBytes(1<<20))["write"]
	tests := []struct {
		name    string
		args    map[string]any
		check   string // relative path whose content must equal content
		wantErr bool
	}{
		{name: "new file with parents", args: map[string]any{"path": "out/new/a.txt", "content": "hi\n"}, check: "out/new/a.txt"},
		{name: "overwrite", args: map[string]any{"path": "main.go", "content": "package x\n"}, check: "main.go"},
		{name: "empty content allowed", args: map[string]any{"path": "empty.txt", "content": ""}, check: "empty.txt"},
		{name: "content required", args: map[string]any{"path": "x.txt"}, wantErr: true},
		{name: "directory rejected", args: map[string]any{"path": "docs", "content": "x"}, wantErr: true},
		{name: "traversal rejected", args: map[string]any{"path": "../escape.txt", "content": "x"}, wantErr: true},
		{name: "symlink escape rejected", args: map[string]any{"path": "link/f.txt", "content": "x"}, wantErr: true},
		{name: "too large", args: map[string]any{"path": "big.txt", "content": strings.Repeat("x", 2<<20)}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := write.Execute(context.Background(), tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.check == "" {
				return
			}
			got, err := os.ReadFile(filepath.Join(root, tt.check))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.args["content"] {
				t.Errorf("content = %q, want %q", got, tt.args["content"])
			}
		})
	}
	if _, err := os.Stat(filepath.Join(outside, "f.txt")); err == nil {
		t.Error("write followed a symlink out of the root")
	}
}

func TestEdit(t *testing.T) {
	tests := []struct {
		name    string
		args    map[string]any
		want    string
		wantErr bool
	}{
		{name: "unique replace", args: map[string]any{"old_string": "func main() {}", "new_string": "func main() { run() }"}, want: "package main\n\nfunc main() { run() }\n"},
		{name: "not found", args: map[string]any{"old_string": "nope", "new_string": "x"}, wantErr: true},
		{name: "identical", args: map[string]any{"old_string": "main", "new_string": "main"}, wantErr: true},
		{name: "ambiguous without replace_all", args: map[string]any{"old_string": "main", "new_string": "x"}, wantErr: true},
		{name: "replace_all", args: map[string]any{"old_string": "main", "new_string": "x", "replace_all": true}, want: "package x\n\nfunc x() {}\n"},
		{name: "delete text", args: map[string]any{"old_string": "\n\nfunc main() {}", "new_string": ""}, want: "package main\n"},
		{name: "missing args", args: map[string]any{"old_string": "x"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := workspace(t)
			edit := toolsByName(t, root, AllowWrites())["edit"]
			args := map[string]any{"path": "main.go"}
			for k, v := range tt.args {
				args[k] = v
			}
			_, err := edit.Execute(context.Background(), args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			got, _ := os.ReadFile(filepath.Join(root, "main.go"))
			if tt.wantErr {
				if string(got) != "package main\n\nfunc main() {}\n" {
					t.Errorf("a failed edit changed the file: %q", got)
				}
				return
			}
			if string(got) != tt.want {
				t.Errorf("file = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEditKeepsPermissions(t *testing.T) {
	root := workspace(t)
	p := filepath.Join(root, "run.sh")
	if err := os.WriteFile(p, []byte("echo hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	edit := toolsByName(t, root, AllowWrites())["edit"]
	if _, err := edit.Execute(context.Background(), map[string]any{"path": "run.sh", "old_string": "hi", "new_string": "bye"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755", info.Mode().Perm())
	}
}

func checkResult(t *testing.T, got string, err error, want, wantNot []string, wantErr bool) {
	t.Helper()
	if (err != nil) != wantErr {
		t.Fatalf("err = %v, wantErr %v (output %q)", err, wantErr, got)
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("output missing %q:\n%s", w, got)
		}
	}
	for _, w := range wantNot {
		if strings.Contains(got, w) {
			t.Errorf("output unexpectedly contains %q:\n%s", w, got)
		}
	}
}
