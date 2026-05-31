package research

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setupRoot creates a temp root with a known file and an "outside" sibling
// directory that contains a secret the root must never be able to read.
func setupRoot(t *testing.T) (root, outsideSecret string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "nested.txt"), []byte("nested\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outsideSecret = filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideSecret, []byte("TOPSECRET\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, outsideSecret
}

func TestReadFileTool_PathTraversal(t *testing.T) {
	root, outsideSecret := setupRoot(t)

	// A symlink inside root that points at the outside secret.
	escapeLink := filepath.Join(root, "escape.txt")
	if err := os.Symlink(outsideSecret, escapeLink); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	tool := NewReadFileTool(root)

	tests := []struct {
		name    string
		path    string
		wantErr bool
		wantSub string // substring expected in successful output
	}{
		{name: "valid relative", path: "ok.txt", wantErr: false, wantSub: "hello"},
		{name: "valid nested", path: "sub/nested.txt", wantErr: false, wantSub: "nested"},
		{name: "dotdot traversal", path: "../outside/secret.txt", wantErr: true},
		{name: "deep dotdot traversal", path: "../../../../etc/passwd", wantErr: true},
		{name: "nested dotdot escape", path: "sub/../../outside/secret.txt", wantErr: true},
		{name: "absolute outside root", path: outsideSecret, wantErr: true},
		{name: "absolute etc passwd", path: "/etc/passwd", wantErr: true},
		{name: "symlink escape", path: "escape.txt", wantErr: true},
		{name: "empty path", path: "", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tool.Execute(context.Background(), map[string]any{"path": tc.path})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for path %q, got output: %q", tc.path, out)
				}
				if strings.Contains(out, "TOPSECRET") {
					t.Fatalf("leaked secret content for path %q", tc.path)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for path %q: %v", tc.path, err)
			}
			if !strings.Contains(out, tc.wantSub) {
				t.Fatalf("path %q: output %q does not contain %q", tc.path, out, tc.wantSub)
			}
		})
	}
}

func TestReadFileTool_AbsolutePathInsideRootAllowed(t *testing.T) {
	root, _ := setupRoot(t)
	tool := NewReadFileTool(root)

	abs := filepath.Join(root, "ok.txt")
	out, err := tool.Execute(context.Background(), map[string]any{"path": abs})
	if err != nil {
		t.Fatalf("absolute path inside root should be allowed: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("expected file content, got %q", out)
	}
}

func TestFileSearchTool_PathTraversal(t *testing.T) {
	root, outsideSecret := setupRoot(t)

	tool := NewFileSearchTool(root)

	tests := []struct {
		name    string
		args    map[string]any
		wantErr bool
	}{
		{name: "search within root", args: map[string]any{"pattern": "hello"}, wantErr: false},
		{name: "scoped subdir", args: map[string]any{"pattern": "nested", "path": "sub"}, wantErr: false},
		{name: "dotdot path escape", args: map[string]any{"pattern": "x", "path": "../outside"}, wantErr: true},
		{name: "absolute path escape", args: map[string]any{"pattern": "x", "path": filepath.Dir(outsideSecret)}, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tool.Execute(context.Background(), tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got output: %q", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestFileSearchTool_SymlinkEscapeNotMatched ensures a symlink inside root that
// points at an outside secret is not searched/returned.
func TestFileSearchTool_SymlinkEscapeNotMatched(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink behavior differs on windows")
	}
	root, outsideSecret := setupRoot(t)

	link := filepath.Join(root, "leak.txt")
	if err := os.Symlink(outsideSecret, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	tool := NewFileSearchTool(root)
	out, err := tool.Execute(context.Background(), map[string]any{"pattern": "TOPSECRET"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "TOPSECRET") {
		t.Fatalf("symlink escape leaked secret: %q", out)
	}
}
