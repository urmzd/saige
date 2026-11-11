package workspace_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/urmzd/saige/sandbox/workspace"
)

func TestLocalRoundTrip(t *testing.T) {
	ctx := context.Background()
	ws, err := workspace.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if err := ws.Write(ctx, "runs/1/out.txt", strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	rc, err := ws.Read(ctx, "runs/1/out.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(got) != "hello" {
		t.Fatalf("read back %q, %v", got, err)
	}

	if err := ws.Delete(ctx, "runs/1/out.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Read(ctx, "runs/1/out.txt"); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}
}

func TestLocalList(t *testing.T) {
	ctx := context.Background()
	ws, err := workspace.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"runs/2/a", "runs/1/a", "logs/x"} {
		if err := ws.Write(ctx, k, strings.NewReader("v")); err != nil {
			t.Fatal(err)
		}
	}

	keys, err := ws.List(ctx, "runs")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"runs/1/a", "runs/2/a"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("List(runs) = %v, want %v", keys, want)
	}

	all, err := ws.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("List(\"\") = %v, want 3 keys", all)
	}
}

func TestLocalRejectsInvalidKeys(t *testing.T) {
	ctx := context.Background()
	ws, err := workspace.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"", "/abs", "a/../b", "./x", "trailing/"} {
		if err := ws.Write(ctx, k, strings.NewReader("v")); !errors.Is(err, workspace.ErrInvalidKey) {
			t.Errorf("Write(%q): want ErrInvalidKey, got %v", k, err)
		}
	}
}

// escapeFixture is a workspace whose directory holds links to a sibling
// directory outside it: "out" to the directory, "link" to a file in it.
func escapeFixture(t *testing.T) (ws *workspace.Local, outside string) {
	t.Helper()
	base := t.TempDir()
	dir, outside := filepath.Join(base, "ws"), filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	if err := os.Symlink(outside, filepath.Join(dir, "out")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	return ws, outside
}

func TestLocalRefusesSymlinkEscapes(t *testing.T) {
	ctx := context.Background()
	ws, outside := escapeFixture(t)

	if rc, err := ws.Read(ctx, "out/secret"); err == nil {
		rc.Close()
		t.Fatal("read through a linked directory escaped the workspace")
	}
	if rc, err := ws.Read(ctx, "link"); err == nil {
		rc.Close()
		t.Fatal("read through a linked file escaped the workspace")
	}
	if err := ws.Write(ctx, "out/planted", strings.NewReader("x")); err == nil {
		t.Fatal("write through a linked directory succeeded")
	}
	if _, err := os.Stat(filepath.Join(outside, "planted")); !os.IsNotExist(err) {
		t.Fatalf("write landed outside the workspace: %v", err)
	}
	if err := ws.Delete(ctx, "out/secret"); err == nil {
		t.Fatal("delete through a linked directory succeeded")
	}
	if err := ws.Delete(ctx, "link"); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("delete of a link: want ErrNotFound, got %v", err)
	}
	keys, err := ws.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("List reported links or what they point at: %v", keys)
	}

	// Writing at a link's key replaces the link; it never writes through.
	if err := ws.Write(ctx, "link", strings.NewReader("mine")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(outside, "secret")); string(got) != "secret" {
		t.Fatalf("write through a linked file changed it to %q", got)
	}
	rc, err := ws.Read(ctx, "link")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "mine" {
		t.Fatalf("read back %q", got)
	}
}

func TestLocalListByPrefixAndDirectories(t *testing.T) {
	ctx := context.Background()
	ws, err := workspace.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"runs/1/a", "runs-archive/a", "runsx"} {
		if err := ws.Write(ctx, k, strings.NewReader("v")); err != nil {
			t.Fatal(err)
		}
	}
	keys, err := ws.List(ctx, "runs")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(keys, []string{"runs/1/a"}) {
		t.Fatalf("List(runs) = %v", keys)
	}
	if keys, _ := ws.List(ctx, "runs/1/a"); !reflect.DeepEqual(keys, []string{"runs/1/a"}) {
		t.Fatalf("List(exact key) = %v", keys)
	}
	if _, err := ws.Read(ctx, "runs/1"); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("read of a directory: want ErrNotFound, got %v", err)
	}
	if err := ws.Delete(ctx, "runs/1"); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("delete of a directory: want ErrNotFound, got %v", err)
	}
}
