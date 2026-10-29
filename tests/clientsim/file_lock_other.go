//go:build !windows && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd

package clientsim

import "os"

func lockStoreFile(*os.File) error { return ErrStoreUnsupported }
