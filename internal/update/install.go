package update

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ErrNotWritable reports an install directory this user cannot write, such
// as /usr/local/bin for a package install. The updater never escalates.
var ErrNotWritable = errors.New("install directory is not writable by this user")

// BackupSuffix names the single previous binary kept until the new one is
// confirmed running.
const BackupSuffix = ".previous"

// Target resolves the running executable through symlinks to the file an
// update must replace.
func Target() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", real)
	}
	return real, nil
}

// checkWritable probes whether files can be created in the target's
// directory, which atomic replacement requires.
func checkWritable(target string) error {
	f, err := os.CreateTemp(filepath.Dir(target), ".rhizome-update-probe-*")
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return ErrNotWritable
		}
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// stageCandidate copies the verified candidate into the target's directory as
// a private temporary executable so the final step is a same-directory rename.
func stageCandidate(candidate, target string) (string, error) {
	src, err := os.Open(candidate)
	if err != nil {
		return "", err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(filepath.Dir(target), ".rhizome-update-*")
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return "", ErrNotWritable
		}
		return "", err
	}
	if _, err := io.Copy(tmp, io.LimitReader(src, maxBinary+1)); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// installBinary moves the staged file over target, keeping the previous
// binary at target+BackupSuffix. A failure after the backup rename restores
// the original. The running process keeps its already-opened image.
func installBinary(staged, target string) (backup string, err error) {
	backup = target + BackupSuffix
	if err := os.Remove(backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.Rename(target, backup); err != nil {
		os.Remove(staged)
		return "", err
	}
	if err := os.Rename(staged, target); err != nil {
		if rerr := os.Rename(backup, target); rerr != nil {
			return backup, fmt.Errorf("install failed (%v) and restoring the previous binary failed: %w", err, rerr)
		}
		os.Remove(staged)
		return "", err
	}
	return backup, nil
}

// restoreBinary puts the backup back in place of target.
func restoreBinary(backup, target string) error {
	if _, err := os.Stat(backup); err != nil {
		return err
	}
	aside := target + ".failed"
	os.Remove(aside)
	if err := os.Rename(target, aside); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(backup, target); err != nil {
		return err
	}
	os.Remove(aside)
	return nil
}
