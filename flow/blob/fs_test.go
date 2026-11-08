package blob_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urmzd/saige/flow/blob"
	"github.com/urmzd/saige/flow/blob/blobtest"
)

func newFS(t *testing.T) (*blob.FS, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "store")
	s, err := blob.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

func TestFSSatisfiesTheStoreContract(t *testing.T) {
	blobtest.Run(t, func(t *testing.T) blob.Store {
		s, _ := newFS(t)
		return s
	})
}

// blobPath finds the one file stored under blobs/.
func blobPath(t *testing.T, dir string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(filepath.Join(dir, "blobs"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			found = p
		}
		return err
	})
	if err != nil || found == "" {
		t.Fatalf("no blob file found: %v", err)
	}
	return found
}

func TestFSNeverWritesOutsideItsRootWhateverTheTenantIsCalled(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "store")
	s, err := blob.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	for _, tenant := range []string{"../escape", "../../escape", "/abs/escape", "..", "a/../../b", `..\escape`} {
		info, err := s.Put(ctx, tenant, strings.NewReader("payload"), blob.PutOptions{})
		if err != nil {
			t.Fatalf("Put(%q): %v", tenant, err)
		}
		if _, err := s.PutRef(ctx, tenant, "../../ref", info.Digest); err != nil {
			t.Fatalf("PutRef(%q): %v", tenant, err)
		}
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "store" {
		t.Fatalf("store wrote beside its root: %v", entries)
	}
	// Every name on disk is one this package generated: hex, or a fixed
	// directory. No tenant or key text reaches the filesystem.
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		name := d.Name()
		if name == "blobs" || name == "refs" || name == "tmp" {
			return nil
		}
		if strings.Trim(name, "0123456789abcdef") != "" {
			t.Errorf("unexpected name on disk: %q", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFSRefusesToServeThroughASymlinkPlantedAtABlobPath(t *testing.T) {
	s, dir := newFS(t)
	ctx := context.Background()

	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("outside the store"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The planted link's target has exactly the content its name claims, so
	// only the "regular file" rule, not the digest check, can refuse it.
	info, err := s.Put(ctx, "acme", strings.NewReader("outside the store"), blob.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	path := blobPath(t, dir)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, path); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}

	if rc, _, err := s.Get(ctx, "acme", info.Digest); err == nil {
		_ = rc.Close()
		t.Fatal("Get served a blob through a symlink leaving the store")
	} else if !errors.Is(err, blob.ErrCorrupt) {
		t.Fatalf("Get = %v, want ErrCorrupt", err)
	}
	if _, err := s.Stat(ctx, "acme", info.Digest); !errors.Is(err, blob.ErrCorrupt) {
		t.Fatalf("Stat = %v, want ErrCorrupt", err)
	}

	// Delete removes the link, not what it points at.
	if err := s.Delete(ctx, "acme", info.Digest); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(secret); err != nil || string(b) != "outside the store" {
		t.Fatalf("Delete followed the symlink: %q, %v", b, err)
	}
}

func TestFSPutReplacesAPlantedSymlinkInsteadOfWritingThroughIt(t *testing.T) {
	s, dir := newFS(t)
	ctx := context.Background()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := s.Put(ctx, "acme", strings.NewReader("payload"), blob.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	path := blobPath(t, dir)
	_ = os.Remove(path)
	if err := os.Symlink(victim, path); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	if _, err := s.Put(ctx, "acme", strings.NewReader("payload"), blob.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "untouched" {
		t.Fatalf("Put wrote through the symlink: %q", b)
	}
	rc, _, err := s.Get(ctx, "acme", info.Digest)
	if err != nil {
		t.Fatalf("Get after healing Put: %v", err)
	}
	defer func() { _ = rc.Close() }()
	if b, _ := io.ReadAll(rc); string(b) != "payload" {
		t.Fatalf("healed blob = %q", b)
	}
}

func TestFSRefusesASymlinkedTenantDirectoryThatLeavesTheRoot(t *testing.T) {
	s, dir := newFS(t)
	ctx := context.Background()
	outside := t.TempDir()
	if _, err := s.Put(ctx, "acme", strings.NewReader("x"), blob.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	tenantDir := filepath.Dir(filepath.Dir(blobPath(t, dir)))
	if err := os.RemoveAll(tenantDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, tenantDir); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	if _, err := s.Put(ctx, "acme", strings.NewReader("x"), blob.PutOptions{}); err == nil {
		t.Fatal("Put followed a tenant directory symlink out of the store")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("store wrote outside its root: %v", entries)
	}
}

func TestFSDetectsContentThatNoLongerMatchesItsDigest(t *testing.T) {
	s, dir := newFS(t)
	ctx := context.Background()
	info, err := s.Put(ctx, "acme", strings.NewReader("the original bytes"), blob.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blobPath(t, dir), []byte("the tampered bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	rc, _, err := s.Get(ctx, "acme", info.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if _, err := io.ReadAll(rc); !errors.Is(err, blob.ErrCorrupt) {
		t.Fatalf("reading a tampered blob = %v, want ErrCorrupt", err)
	}
}

func TestFSFailedUploadsLeaveNoTemporaryFiles(t *testing.T) {
	s, dir := newFS(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "acme", strings.NewReader("too long"), blob.PutOptions{MaxBytes: 3}); !errors.Is(err, blob.ErrTooLarge) {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "acme", strings.NewReader("x"), blob.PutOptions{Expect: blob.Sum([]byte("y"))}); !errors.Is(err, blob.ErrDigestMismatch) {
		t.Fatal(err)
	}
	info, err := s.Put(ctx, "acme", strings.NewReader("ok"), blob.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutRef(ctx, "acme", "k", info.Digest); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestFSContentSurvivesReopening(t *testing.T) {
	s, dir := newFS(t)
	ctx := context.Background()
	info, err := s.Put(ctx, "acme", strings.NewReader("durable"), blob.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutRef(ctx, "acme", "k", info.Digest); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	again, err := blob.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Close() }()
	if st, err := again.Stat(ctx, "acme", info.Digest); err != nil || st != info {
		t.Fatalf("Stat after reopen = %+v, %v", st, err)
	}
	if d, err := again.Ref(ctx, "acme", "k"); err != nil || d != info.Digest {
		t.Fatalf("Ref after reopen = %s, %v", d, err)
	}
}
