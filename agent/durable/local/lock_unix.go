//go:build unix

package local

import (
	"errors"
	"math"
	"os"

	"golang.org/x/sys/unix"
)

func lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600) // #nosec G304 G703 -- derived private run directory; no user-controlled path component.
	if err != nil {
		return nil, err
	}
	fd := f.Fd()
	if fd > uintptr(math.MaxInt) {
		_ = f.Close()
		return nil, errors.New("file descriptor exceeds platform integer range")
	}
	descriptor := int(fd)
	if err = unix.Flock(descriptor, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return func() { _ = unix.Flock(descriptor, unix.LOCK_UN); _ = f.Close() }, nil
}
