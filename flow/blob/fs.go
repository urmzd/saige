package blob

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
)

// FS is a Store on a local directory.
//
// # Layout
//
//	<root>/blobs/<sha256 of tenant>/<first two digest digits>/<digest>
//	<root>/refs/<sha256 of tenant>/<first two digits>/<sha256 of reference key>
//	<root>/tmp/<random>.part
//
// No path component is built from untrusted input. A reference key is hashed
// for the same reason the tenant is. The tenant is hashed, so a
// tenant named "../other" or one containing a path separator or a NUL is an
// ordinary scope with an ordinary directory. The digest is validated as 64
// lowercase hex digits before it is used. The temporary name is random bytes
// this process generated. There is nothing for a traversal to be spelled in.
//
// # Confinement
//
// Every file operation goes through an os.Root opened on the store directory,
// so the operating system, rather than string checks in this package, refuses
// any path that would resolve outside it, including through a symbolic link
// planted inside the directory. On top of that, a blob is only ever served
// from a regular file: an entry that is a symbolic link, a directory or a
// device is reported as ErrCorrupt, and the opened file is checked to be the
// same file that was inspected, so a link swapped in between the two steps is
// caught as well.
//
// # Atomicity
//
// Put writes to a temporary file, syncs it, and renames it into place. A
// reader therefore sees either no blob or the whole blob, never a prefix, and
// a crash mid-upload leaves at worst an orphan under tmp/ that no digest
// points to. Orphans are not swept automatically: another process may share
// the directory, and this one cannot tell that process's in-flight upload
// from debris.
//
// # Integrity
//
// The reader Get returns hashes what it serves and reports ErrCorrupt in
// place of io.EOF if the content no longer matches its digest. Disk
// corruption, or a file edited behind the store's back, is caught on read.
type FS struct {
	root *os.Root
}

const (
	blobsDir = "blobs"
	refsDir  = "refs"
	tmpDir   = "tmp"
	dirPerm  = 0o700
	filePerm = 0o600
)

var _ Store = (*FS)(nil)

// NewFS opens a filesystem store rooted at dir, creating it if needed. The
// directory is trusted configuration: it is the operator's choice of where
// blobs live, not something derived from a request.
func NewFS(dir string) (*FS, error) {
	if dir == "" {
		return nil, fmt.Errorf("%w: empty store directory", ErrInvalid)
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("blob: create store directory: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("blob: open store directory: %w", err)
	}
	for _, sub := range []string{blobsDir, refsDir, tmpDir} {
		if err := root.MkdirAll(sub, dirPerm); err != nil {
			_ = root.Close()
			return nil, fmt.Errorf("blob: create %s directory: %w", sub, err)
		}
	}
	return &FS{root: root}, nil
}

// Close releases the store's handle on its directory.
func (f *FS) Close() error {
	if err := f.root.Close(); err != nil {
		return fmt.Errorf("blob: close store: %w", err)
	}
	return nil
}

// tenantDir is the tenant's directory name: a hash, so the tenant string
// itself never reaches the filesystem.
func tenantDir(tenant string) string { return blobsDir + "/" + hashName(tenant) }

func hashName(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// refLocation returns the directory and full path of a reference.
func refLocation(tenant, key string) (dir, path string) {
	name := hashName(key)
	dir = refsDir + "/" + hashName(tenant) + "/" + name[:2]
	return dir, dir + "/" + name
}

// location returns the directory and full path of a blob. d must already be
// validated.
func location(tenant string, d Digest) (dir, path string) {
	dir = tenantDir(tenant) + "/" + string(d[:2])
	return dir, dir + "/" + string(d)
}

// Put implements Store.
func (f *FS) Put(ctx context.Context, tenant string, r io.Reader, opts PutOptions) (Info, error) {
	if err := checkTenant(ctx, tenant); err != nil {
		return Info{}, err
	}
	tmp, file, err := f.temp()
	if err != nil {
		return Info{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = f.root.Remove(tmp)
		}
	}()

	info, err := ingest(ctx, file, r, opts)
	if err == nil {
		if serr := file.Sync(); serr != nil {
			err = fmt.Errorf("blob: sync temporary file: %w", serr)
		}
	}
	if cerr := file.Close(); cerr != nil && err == nil {
		err = fmt.Errorf("blob: close temporary file: %w", cerr)
	}
	if err != nil {
		return Info{}, err
	}

	dir, path := location(tenant, info.Digest)
	if err := f.root.MkdirAll(dir, dirPerm); err != nil {
		return Info{}, fmt.Errorf("blob: create blob directory: %w", err)
	}
	// Rename unconditionally, even when the blob is already present. The new
	// file is known good, the replacement is atomic, and it heals an entry
	// that had been corrupted or replaced by a link, which a "skip if it
	// exists" check would preserve.
	if err := f.root.Rename(tmp, path); err != nil {
		return Info{}, fmt.Errorf("blob: commit blob: %w", err)
	}
	committed = true
	f.syncDir(dir)
	return info, nil
}

// temp creates a uniquely named temporary file.
func (f *FS) temp() (string, *os.File, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", nil, fmt.Errorf("blob: temporary name: %w", err)
	}
	tmp := tmpDir + "/" + hex.EncodeToString(nonce[:]) + ".part"
	// O_EXCL: the name is ours alone, and a pre-planted entry with the same
	// name is refused rather than written through.
	file, err := f.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return "", nil, fmt.Errorf("blob: create temporary file: %w", err)
	}
	return tmp, file, nil
}

// syncDir flushes a directory entry so a committed blob survives a power
// loss. It is best effort: not every platform can sync a directory, and the
// blob is already complete and visible.
func (f *FS) syncDir(dir string) {
	d, err := f.root.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// Get implements Store.
func (f *FS) Get(ctx context.Context, tenant string, d Digest) (io.ReadCloser, Info, error) {
	if err := check(ctx, tenant, d); err != nil {
		return nil, Info{}, err
	}
	_, path := location(tenant, d)
	file, opened, err := f.openRegular(path, func() error { return notFound(d) }, d.String())
	if err != nil {
		return nil, Info{}, err
	}
	return &verifyingReader{file: file, hash: sha256.New(), want: d}, Info{Digest: d, Size: opened.Size()}, nil
}

// Stat implements Store.
func (f *FS) Stat(ctx context.Context, tenant string, d Digest) (Info, error) {
	if err := check(ctx, tenant, d); err != nil {
		return Info{}, err
	}
	_, path := location(tenant, d)
	fi, err := f.regular(path, d)
	if err != nil {
		return Info{}, err
	}
	return Info{Digest: d, Size: fi.Size()}, nil
}

// Delete implements Store. Remove unlinks the entry itself and never follows
// a link, so it cannot be steered at another tenant's file.
func (f *FS) Delete(ctx context.Context, tenant string, d Digest) error {
	if err := check(ctx, tenant, d); err != nil {
		return err
	}
	_, path := location(tenant, d)
	if err := f.root.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("blob: delete blob: %w", err)
	}
	return nil
}

// PutRef implements Store.
//
// The reference is written to a temporary file and hard-linked into place.
// Link fails if the name exists, which is what makes "first writer wins"
// atomic: of two racing writers exactly one creates the name, and the other
// reads what the winner wrote.
func (f *FS) PutRef(ctx context.Context, tenant, key string, d Digest) (Digest, error) {
	if err := checkRef(ctx, tenant, key); err != nil {
		return "", err
	}
	if _, err := f.Stat(ctx, tenant, d); err != nil {
		return "", err
	}
	tmp, file, err := f.temp()
	if err != nil {
		return "", err
	}
	defer func() { _ = f.root.Remove(tmp) }()
	_, err = file.WriteString(string(d))
	if err == nil {
		err = file.Sync()
	}
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("blob: write reference: %w", err)
	}
	dir, path := refLocation(tenant, key)
	if err := f.root.MkdirAll(dir, dirPerm); err != nil {
		return "", fmt.Errorf("blob: create reference directory: %w", err)
	}
	err = f.root.Link(tmp, path)
	if err == nil {
		f.syncDir(dir)
		return d, nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return "", fmt.Errorf("blob: commit reference: %w", err)
	}
	// Someone got there first. Keep their answer if it is still usable.
	if cur, rerr := f.readRef(path, key); rerr == nil {
		if _, serr := f.Stat(ctx, tenant, cur); serr == nil {
			return cur, nil
		}
	}
	// The existing reference is dangling or unreadable: replace it.
	if err := f.root.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("blob: replace reference: %w", err)
	}
	f.syncDir(dir)
	return d, nil
}

// Ref implements Store.
func (f *FS) Ref(ctx context.Context, tenant, key string) (Digest, error) {
	if err := checkRef(ctx, tenant, key); err != nil {
		return "", err
	}
	_, path := refLocation(tenant, key)
	return f.readRef(path, key)
}

// DeleteRef implements Store.
func (f *FS) DeleteRef(ctx context.Context, tenant, key string) error {
	if err := checkRef(ctx, tenant, key); err != nil {
		return err
	}
	_, path := refLocation(tenant, key)
	if err := f.root.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("blob: delete reference: %w", err)
	}
	return nil
}

// readRef reads a reference file, holding it to the same rule as a blob: a
// regular file, and the same file that was inspected.
func (f *FS) readRef(path, key string) (Digest, error) {
	file, _, err := f.openRegular(path, func() error { return fmt.Errorf("%w: reference %q", ErrNotFound, key) }, "reference")
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, sha256.Size*2+1))
	if err != nil {
		return "", fmt.Errorf("blob: read reference: %w", err)
	}
	d, err := ParseDigest(string(raw))
	if err != nil {
		return "", fmt.Errorf("%w: reference %q does not hold a digest", ErrCorrupt, key)
	}
	return d, nil
}

// openAttempts bounds how often openRegular re-inspects an entry that changed
// under it. A concurrent Put of the same content atomically replaces the
// entry, which is benign and settles at once; an entry that keeps changing is
// reported as corrupt rather than chased.
const openAttempts = 8

// openRegular opens path only if it is a regular file, and only if the file
// it opened is the file it inspected. Lstat does not follow a final link and
// Open does, so if the entry is swapped for a link between the two calls the
// opened file is a different one and is refused.
func (f *FS) openRegular(path string, missing func() error, what string) (*os.File, fs.FileInfo, error) {
	for range openAttempts {
		seen, err := f.root.Lstat(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, nil, missing()
			}
			return nil, nil, fmt.Errorf("blob: stat %s: %w", what, err)
		}
		if !seen.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("%w: %s is stored as %s, not a regular file", ErrCorrupt, what, seen.Mode().Type())
		}
		file, err := f.root.Open(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // replaced or deleted in between: look again
			}
			return nil, nil, fmt.Errorf("blob: open %s: %w", what, err)
		}
		opened, err := file.Stat()
		if err == nil && os.SameFile(seen, opened) {
			return file, opened, nil
		}
		_ = file.Close()
		if err != nil {
			return nil, nil, fmt.Errorf("blob: stat open %s: %w", what, err)
		}
	}
	return nil, nil, fmt.Errorf("%w: %s kept changing while being opened", ErrCorrupt, what)
}

// regular inspects a blob path without following a final link and requires a
// regular file.
func (f *FS) regular(path string, d Digest) (fs.FileInfo, error) {
	fi, err := f.root.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, notFound(d)
		}
		return nil, fmt.Errorf("blob: stat blob: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is stored as %s, not a regular file", ErrCorrupt, d, fi.Mode().Type())
	}
	return fi, nil
}

// verifyingReader hashes a blob as it is read and turns the final io.EOF
// into ErrCorrupt when the content does not match its digest.
type verifyingReader struct {
	file *os.File
	hash hash.Hash
	want Digest
	done bool
}

func (v *verifyingReader) Read(p []byte) (int, error) {
	n, err := v.file.Read(p)
	if n > 0 {
		v.hash.Write(p[:n])
	}
	if err == io.EOF && !v.done {
		v.done = true
		if got := digestOf(v.hash); got != v.want {
			return n, fmt.Errorf("%w: stored content of %s hashes to %s", ErrCorrupt, v.want, got)
		}
	}
	return n, err
}

func (v *verifyingReader) Close() error { return v.file.Close() }
