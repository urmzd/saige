package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// tmpPrefix marks the temporary files Write renames into place. List never
// reports them.
const tmpPrefix = ".tmp-"

// Local is a Workspace backed by a directory on the local filesystem. It is
// the default backend for single-machine deployments and for tests.
//
// Every file access goes through an os.Root opened on the directory, so no
// key reaches outside it: a symbolic link that points out of the directory,
// whether it was there before a call or swapped in during one, fails the
// operation instead of being followed. Keys name regular files only; List
// does not report symbolic links, and Write replaces a link at its key
// rather than writing through it.
type Local struct {
	root *os.Root
}

// NewLocal creates (if needed) and opens a workspace rooted at dir. The
// workspace holds the directory open until Close.
func NewLocal(dir string) (*Local, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("workspace: resolve root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil { //nolint:gosec // nodes and isolated instances read the workspace as other users
		return nil, fmt.Errorf("workspace: create root: %w", err)
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace: open root: %w", err)
	}
	return &Local{root: root}, nil
}

// Close releases the open directory. The workspace is unusable afterwards.
func (l *Local) Close() error { return l.root.Close() }

// name validates key and returns its path relative to the root.
func name(key string) (string, error) {
	if !ValidKey(key) {
		return "", fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	return filepath.FromSlash(key), nil
}

// Read implements Workspace.
func (l *Local) Read(_ context.Context, key string) (io.ReadCloser, error) {
	n, err := name(key)
	if err != nil {
		return nil, err
	}
	f, err := l.root.Open(n)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, key)
	}
	if err != nil {
		return nil, fmt.Errorf("workspace: read %q: %w", key, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("workspace: read %q: %w", key, err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %q", ErrNotFound, key)
	}
	return f, nil
}

// Write implements Workspace. The blob lands atomically: it is written to a
// temporary file beside the key and renamed into place.
func (l *Local) Write(_ context.Context, key string, r io.Reader) error {
	n, err := name(key)
	if err != nil {
		return err
	}
	if err := l.root.MkdirAll(filepath.Dir(n), 0o755); err != nil {
		return fmt.Errorf("workspace: write %q: %w", key, err)
	}
	tmp, f, err := l.createTemp(filepath.Dir(n))
	if err != nil {
		return fmt.Errorf("workspace: write %q: %w", key, err)
	}
	defer l.root.Remove(tmp) //nolint:errcheck // gone after a successful rename
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return fmt.Errorf("workspace: write %q: %w", key, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("workspace: write %q: %w", key, err)
	}
	if err := l.root.Rename(tmp, n); err != nil {
		return fmt.Errorf("workspace: write %q: %w", key, err)
	}
	return nil
}

// createTemp creates a new temporary file in dir (relative to the root) and
// returns its name and handle.
func (l *Local) createTemp(dir string) (string, *os.File, error) {
	for range 10 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", nil, err
		}
		tmp := filepath.Join(dir, tmpPrefix+hex.EncodeToString(b[:]))
		f, err := l.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return tmp, f, err
	}
	return "", nil, errors.New("could not create a temporary file")
}

// List implements Workspace.
func (l *Local) List(_ context.Context, prefix string) ([]string, error) {
	if prefix != "" && !ValidKey(prefix) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidKey, prefix)
	}
	var keys []string
	err := fs.WalkDir(l.root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p != "." && errors.Is(err, fs.ErrNotExist) {
				return nil // removed while listing
			}
			return err
		}
		if d.IsDir() {
			if p != "." && prefix != "" && !related(p, prefix) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasPrefix(path.Base(p), tmpPrefix) {
			return nil
		}
		if prefix == "" || p == prefix || strings.HasPrefix(p, prefix+"/") {
			keys = append(keys, p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("workspace: list %q: %w", prefix, err)
	}
	sort.Strings(keys)
	return keys, nil
}

// related reports whether directory dir can hold keys under prefix: it is
// the prefix, an ancestor of it, or inside it.
func related(dir, prefix string) bool {
	return dir == prefix || strings.HasPrefix(prefix, dir+"/") || strings.HasPrefix(dir, prefix+"/")
}

// Delete implements Workspace.
func (l *Local) Delete(_ context.Context, key string) error {
	n, err := name(key)
	if err != nil {
		return err
	}
	info, err := l.root.Lstat(n)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !info.Mode().IsRegular()) {
		return fmt.Errorf("%w: %q", ErrNotFound, key)
	}
	if err != nil {
		return fmt.Errorf("workspace: delete %q: %w", key, err)
	}
	if err := l.root.Remove(n); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %q", ErrNotFound, key)
	} else if err != nil {
		return fmt.Errorf("workspace: delete %q: %w", key, err)
	}
	return nil
}
