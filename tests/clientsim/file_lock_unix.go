//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package clientsim

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockStoreFile(file *os.File) error {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return ErrStoreBusy
	}
	if err != nil {
		return safeError(ErrStore, err)
	}
	return nil
}
