//go:build !windows

package channel

import "syscall"

// sourceOpenFlags refuses a symlink swapped in after Lstat and never blocks
// on a FIFO.
const sourceOpenFlags = syscall.O_NONBLOCK | syscall.O_NOFOLLOW
