// Package fsutil holds the file operations the file adapters share: publish
// a file once (link), replace one atomically (rename), and lock a directory
// across processes (flock). Every write is synced before it is published,
// and the directory after.
package fsutil

import (
	"errors"
	"os"
	"path/filepath"
)

// Publish writes raw to dir/name only if dir/name does not exist. The file
// is written to a temporary name, synced and linked into place, so readers
// never see a partial file and of concurrent publishers exactly one wins.
// It returns an error matching os.ErrExist when the file already exists.
func Publish(dir, name string, raw []byte) error {
	tmp, err := writeTemp(dir, raw)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	if err := os.Link(tmp, filepath.Join(dir, name)); err != nil {
		if errors.Is(err, os.ErrExist) {
			return os.ErrExist
		}
		return err
	}
	return syncDir(dir)
}

// Replace writes raw to dir/name, replacing it atomically: a reader sees the
// old file or the new one, never a mix.
func Replace(dir, name string, raw []byte) error {
	tmp, err := writeTemp(dir, raw)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(dir)
}

func writeTemp(dir string, raw []byte) (string, error) {
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir) // #nosec G304 -- a directory the adapter owns.
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
