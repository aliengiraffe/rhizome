//go:build !windows

package oai

import "os"

// Sync the directory entry as well as the temporary file before a rotating
// refresh token is sent, so the uncertainty fence survives a machine restart.
func syncStateDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
