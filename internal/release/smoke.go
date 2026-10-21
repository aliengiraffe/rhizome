package release

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// maxBinary bounds the extracted executable.
	maxBinary = 256 << 20
	// maxEntries bounds archive members scanned for the executable.
	maxEntries = 64
	// versionTimeout bounds one --version run.
	versionTimeout = 30 * time.Second
)

// BinaryName returns the executable name inside archives for goos.
func BinaryName(goos string) string {
	if goos == "windows" {
		return "rhizome.exe"
	}
	return "rhizome"
}

// ExtractBinary copies the executable at the root of archive into dstDir and
// returns its path. Only that one member is written.
func ExtractBinary(archive, goos, dstDir string) (string, error) {
	name := BinaryName(goos)
	dst := filepath.Join(dstDir, name)
	var src io.Reader
	if strings.HasSuffix(archive, ".zip") {
		zr, err := zip.OpenReader(archive)
		if err != nil {
			return "", err
		}
		defer zr.Close()
		if len(zr.File) > maxEntries {
			return "", fmt.Errorf("%s has more than %d members", archive, maxEntries)
		}
		for _, f := range zr.File {
			if f.Name == name && f.Mode().IsRegular() {
				rc, err := f.Open()
				if err != nil {
					return "", err
				}
				defer rc.Close()
				src = rc
				break
			}
		}
	} else {
		f, err := os.Open(archive)
		if err != nil {
			return "", err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return "", err
		}
		tr := tar.NewReader(gz)
		for i := 0; src == nil; i++ {
			if i == maxEntries {
				return "", fmt.Errorf("%s has more than %d members", archive, maxEntries)
			}
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", err
			}
			if h.Name == name && h.Typeflag == tar.TypeReg {
				src = tr
			}
		}
	}
	if src == nil {
		return "", fmt.Errorf("%s does not contain %s at its root", archive, name)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return "", err
	}
	n, err := io.Copy(out, io.LimitReader(src, maxBinary+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxBinary {
		err = fmt.Errorf("%s exceeds %d bytes", name, maxBinary)
	}
	if err != nil {
		os.Remove(dst)
		return "", err
	}
	return dst, nil
}

// CheckVersion runs bin --version with an empty environment apart from PATH
// and requires the output to name version and, when non-empty, commit.
func CheckVersion(ctx context.Context, bin, version, commit string, prefix ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	args := append(append([]string{}, prefix...), bin, "--version")
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	if root := os.Getenv("SYSTEMROOT"); root != "" {
		cmd.Env = append(cmd.Env, "SYSTEMROOT="+root)
	}
	out, err := cmd.CombinedOutput()
	line := strings.TrimSpace(string(out))
	if err != nil {
		return line, fmt.Errorf("%s --version: %w (output %q)", bin, err, line)
	}
	want := "rhizome " + version + " (commit "
	if commit != "" {
		want += commit + ", built "
	}
	if !strings.HasPrefix(line, want) || strings.Contains(line, "\n") {
		return line, fmt.Errorf("%s --version printed %q, want prefix %q", bin, line, want)
	}
	return line, nil
}

// Smoke verifies the manifest in dir, extracts the archive for goos/goarch
// into a temporary directory and runs the binary's version check.
func Smoke(ctx context.Context, dir string, v Version, commit, goos, goarch string) (string, error) {
	if err := VerifyManifest(dir, v.AssetVersion(), false); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp("", "rhizome-smoke-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	bin, err := ExtractBinary(filepath.Join(dir, ArchiveName(v.AssetVersion(), goos, goarch)), goos, tmp)
	if err != nil {
		return "", err
	}
	if goos != "windows" {
		if err := os.Chmod(bin, 0o755); err != nil {
			return "", err
		}
	}
	return CheckVersion(ctx, bin, v.Version, commit)
}
