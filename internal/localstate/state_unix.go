//go:build !windows

package localstate

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// SecureDir creates or validates a private directory owned by this user.
func SecureDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Lstat(dir, &stat); err != nil {
		return err
	}
	if stat.Uid != uint32(os.Getuid()) || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return errors.New("runtime directory must be owned by current user, private (0700), and not a symlink")
	}
	return nil
}
func lock(f *os.File, wait bool) error {
	flags := unix.LOCK_EX
	if !wait {
		flags |= unix.LOCK_NB
	}
	err := unix.Flock(int(f.Fd()), flags)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return ErrBusy
	}
	return err
}

// PrivateFile validates a regular, private runtime file.
func PrivateFile(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return errors.New("runtime file must be private and regular")
	}
	return nil
}
