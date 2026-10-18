//go:build !windows

package modelstudio

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func tryAutoLoginLock(file *os.File) error {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return errAutoLoginInProgress
	}
	return err
}

func unlockAutoLoginFile(file *os.File) error { return unix.Flock(int(file.Fd()), unix.LOCK_UN) }
