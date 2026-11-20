//go:build unix

package fsutil

import (
	"errors"
	"math"
	"os"

	"golang.org/x/sys/unix"
)

// ErrLocked reports a lock another holder has.
var ErrLocked = errors.New("fsutil: locked")

// Lock takes an exclusive advisory lock on path, creating it, and blocks
// until it has it. The returned function releases it. A lock belongs to the
// open file: a crashed process releases its locks.
func Lock(path string) (func(), error) { return lock(path, 0) }

// TryLock is Lock without waiting: it returns ErrLocked when another holder
// has the lock.
func TryLock(path string) (func(), error) { return lock(path, unix.LOCK_NB) }

func lock(path string, flags int) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- a lock file in a directory the adapter owns.
	if err != nil {
		return nil, err
	}
	fd := f.Fd()
	if fd > uintptr(math.MaxInt) {
		_ = f.Close()
		return nil, errors.New("fsutil: file descriptor exceeds platform integer range")
	}
	descriptor := int(fd)
	for {
		err = unix.Flock(descriptor, unix.LOCK_EX|flags)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return func() { _ = unix.Flock(descriptor, unix.LOCK_UN); _ = f.Close() }, nil
}
