//go:build !unix

package fsutil

import "errors"

// ErrLocked reports a lock another holder has.
var ErrLocked = errors.New("fsutil: locked")

// Lock is unsupported off Unix: the file adapters need advisory locks.
func Lock(string) (func(), error) { return nil, errors.New("fsutil: file locks require Unix") }

// TryLock is unsupported off Unix.
func TryLock(string) (func(), error) { return nil, errors.New("fsutil: file locks require Unix") }
