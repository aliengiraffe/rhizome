// Package localstate provides private runtime directories and process locks.
package localstate

import (
	"errors"
	"os"
	"path/filepath"
)

// Runtime returns the configured absolute runtime directory.
func Runtime(path string) (string, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, ".rhizome", "run")
	}
	return filepath.Abs(path)
}

// Lock opens a process lock. Close releases it, including after process death.
// A nonblocking lock returns ErrBusy if another process owns it.
func Lock(path string, wait bool) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = lock(f, wait); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// ErrBusy indicates an existing lock owner.
var ErrBusy = errors.New("already running or session collection owned by another process")
