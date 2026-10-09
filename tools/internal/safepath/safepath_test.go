package safepath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolve(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name         string
		requested    string
		requireExist bool
		wantErr      bool
	}{
		{"relative file", "a.txt", true, false},
		{"root itself", ".", true, false},
		{"traversal", "../x", false, true},
		{"absolute outside", outside, false, true},
		{"symlink escape", "escape/f", false, true},
		{"missing leaf allowed", "new/dir/f.txt", false, false},
		{"missing leaf required", "nope.txt", true, true},
		{"empty", "", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(root, tt.requested, tt.requireExist)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Resolve(%q) err = %v, wantErr %v", tt.requested, err, tt.wantErr)
			}
			if err == nil && !filepath.IsAbs(got) {
				t.Errorf("Resolve(%q) = %q, want an absolute path", tt.requested, got)
			}
		})
	}
}
