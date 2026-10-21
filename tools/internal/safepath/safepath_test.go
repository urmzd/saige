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

func TestRel(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		requested string
		want      string
		wantErr   bool
	}{
		{"a.txt", "a.txt", false},
		{".", ".", false},
		{"dir/../a.txt", "a.txt", false},
		{filepath.Join(root, "x", "y"), filepath.Join("x", "y"), false},
		{root, ".", false},
		{"../x", "", true},
		{t.TempDir(), "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		got, err := Rel(root, tt.requested)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("Rel(%q) = %q, %v; want %q, error %v", tt.requested, got, err, tt.want, tt.wantErr)
		}
	}
}

// A symlink swapped after Open, which Resolve's check-then-use cannot
// catch, is refused when the path is used.
func TestOpenRefusesALinkSwappedAfterTheCheck(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "f"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "in"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "in", "f"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink("in", link); err != nil {
		t.Fatal(err)
	}
	r, rel, err := Open(root, "link/f")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if b, err := r.ReadFile(rel); err != nil || string(b) != "inside" {
		t.Fatalf("read through an inside link = %q, %v", b, err)
	}

	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if b, err := r.ReadFile(rel); err == nil {
		t.Fatalf("read through a swapped link reached %q", b)
	}
	if _, _, err := Open(root, "../x"); err == nil {
		t.Error("Open accepted a traversal")
	}
	if _, _, err := Open(filepath.Join(root, "missing"), "x"); err == nil {
		t.Error("Open accepted a missing root")
	}
}
