//go:build !windows

package oauth

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func tryRefreshLock(file *os.File) error {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return errRefreshLockBusy
	}
	return err
}

func unlockRefreshFile(file *os.File) error { return unix.Flock(int(file.Fd()), unix.LOCK_UN) }
