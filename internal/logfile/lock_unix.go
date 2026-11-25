//go:build !windows

package logfile

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes an exclusive, non-blocking flock on f. The kernel drops it
// when the process exits, so it is never stale.
func lockFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errWouldBlock
	}
	return err
}
