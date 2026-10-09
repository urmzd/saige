package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Dir is a Workspace on the local filesystem. Content lives in
// objects/<digest> and each name's latest Ref in names/<hash of name>.json.
// Every file is written to a temporary name in the same directory, synced,
// and renamed into place, so a reader or a crash never sees a partial file.
//
// Dir is safe for concurrent use by one or more processes: content files are
// immutable and a name update is a single rename, so the last writer of a
// name wins.
type Dir struct {
	root string
	now  func() time.Time
}

var _ Workspace = (*Dir)(nil)

// NewDir opens or creates a workspace rooted at root.
func NewDir(root string) (*Dir, error) {
	if root == "" {
		return nil, fmt.Errorf("workspace: empty root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	for _, sub := range []string{"objects", "names"} {
		if err := os.MkdirAll(filepath.Join(abs, sub), 0o700); err != nil {
			return nil, fmt.Errorf("workspace: create %s: %w", sub, err)
		}
	}
	return &Dir{root: abs, now: time.Now}, nil
}

// Root returns the workspace directory.
func (d *Dir) Root() string { return d.root }

func (d *Dir) objectPath(id string) string { return filepath.Join(d.root, "objects", id) }

func (d *Dir) namePath(name string) string {
	sum := sha256.Sum256([]byte(name))
	return filepath.Join(d.root, "names", hex.EncodeToString(sum[:])+".json")
}

// writeAtomic writes data to a temporary file next to path and renames it
// into place.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return cleanup(err)
	}
	if err := tmp.Sync(); err != nil {
		return cleanup(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// Put implements Workspace.
func (d *Dir) Put(ctx context.Context, name string, data []byte, meta map[string]string) (Ref, error) {
	if err := ctx.Err(); err != nil {
		return Ref{}, err
	}
	if err := validName(name); err != nil {
		return Ref{}, err
	}
	id := Digest(data)
	if prev, err := d.readName(name); err == nil && prev.ID == id {
		if _, err := os.Stat(d.objectPath(id)); err == nil {
			return prev, nil
		}
	}
	if _, err := os.Stat(d.objectPath(id)); errors.Is(err, fs.ErrNotExist) {
		if err := writeAtomic(d.objectPath(id), data); err != nil {
			return Ref{}, fmt.Errorf("workspace: write content: %w", err)
		}
	} else if err != nil {
		return Ref{}, err
	}
	ref := Ref{ID: id, Name: name, Size: int64(len(data)), Meta: copyMeta(meta), Created: d.now().UTC()}
	raw, err := json.Marshal(ref)
	if err != nil {
		return Ref{}, err
	}
	if err := writeAtomic(d.namePath(name), raw); err != nil {
		return Ref{}, fmt.Errorf("workspace: write name: %w", err)
	}
	return cloneRef(ref), nil
}

func (d *Dir) readName(name string) (Ref, error) {
	raw, err := os.ReadFile(d.namePath(name))
	if errors.Is(err, fs.ErrNotExist) {
		return Ref{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if err != nil {
		return Ref{}, err
	}
	var ref Ref
	if err := json.Unmarshal(raw, &ref); err != nil {
		return Ref{}, fmt.Errorf("workspace: decode name %q: %w", name, err)
	}
	if ref.Name != name || !isDigest(ref.ID) {
		return Ref{}, fmt.Errorf("workspace: corrupt name record for %q", name)
	}
	return ref, nil
}

// Stat implements Workspace.
func (d *Dir) Stat(ctx context.Context, ref Ref) (Ref, error) {
	if err := ctx.Err(); err != nil {
		return Ref{}, err
	}
	if ref.ID == "" {
		if ref.Name == "" {
			return Ref{}, ErrInvalidRef
		}
		return d.readName(ref.Name)
	}
	if !isDigest(ref.ID) {
		return Ref{}, fmt.Errorf("%w: %q", ErrInvalidRef, ref.ID)
	}
	info, err := os.Stat(d.objectPath(ref.ID))
	if errors.Is(err, fs.ErrNotExist) {
		return Ref{}, fmt.Errorf("%w: %s", ErrNotFound, ref.URI())
	}
	if err != nil {
		return Ref{}, err
	}
	if ref.Name != "" {
		if named, err := d.readName(ref.Name); err == nil && named.ID == ref.ID {
			return named, nil
		}
	}
	return Ref{ID: ref.ID, Name: ref.Name, Size: info.Size(), Created: info.ModTime().UTC()}, nil
}

// Read implements Workspace.
func (d *Dir) Read(ctx context.Context, ref Ref, offset, limit int64) ([]byte, error) {
	r, err := d.Stat(ctx, ref)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(d.objectPath(r.ID))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if offset < 0 {
		offset = 0
	}
	if offset >= r.Size {
		return []byte{}, nil
	}
	n := r.Size - offset
	if limit > 0 && limit < n {
		n = limit
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, offset); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf, nil
}

// List implements Workspace.
func (d *Dir) List(ctx context.Context) ([]Ref, error) {
	entries, err := os.ReadDir(filepath.Join(d.root, "names"))
	if err != nil {
		return nil, err
	}
	out := make([]Ref, 0, len(entries))
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".tmp-") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(d.root, "names", e.Name()))
		if err != nil {
			return nil, err
		}
		var ref Ref
		if err := json.Unmarshal(raw, &ref); err != nil || !isDigest(ref.ID) {
			continue // a corrupt record names nothing readable
		}
		out = append(out, ref)
	}
	sortRefs(out)
	return out, nil
}

// Search implements Workspace.
func (d *Dir) Search(ctx context.Context, query string, k int) ([]Hit, error) {
	refs, err := d.List(ctx)
	if err != nil {
		return nil, err
	}
	return searchRefs(ctx, refs, func(r Ref) ([]byte, error) {
		return os.ReadFile(d.objectPath(r.ID))
	}, query, k)
}

// View implements Workspace.
func (d *Dir) View(readOnly bool) Workspace { return view(d, readOnly) }
