//go:build linux || darwin

package renew

import (
	"errors"
	"os"
	"syscall"
)

// tryFlock attempts an advisory exclusive lock without blocking. A lock held
// by a crashed process is released by the kernel when that process dies.
func tryFlock(f *os.File) (bool, error) {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(err, syscall.EINTR) {
			continue // interrupted: retry
		}
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return false, nil
		}
		return err == nil, err
	}
}
