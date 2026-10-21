//go:build !windows

package channel

import (
	"errors"

	"golang.org/x/sys/unix"
)

// processAlive reports whether pid exists; EPERM means it exists as another user.
func processAlive(pid int) bool {
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}
