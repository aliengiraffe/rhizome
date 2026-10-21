//go:build windows

package channel

import "os"

// processAlive reports whether pid exists: on Windows, FindProcess opens a
// handle to the process and fails when it does not.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
