package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"rhizome/internal/release"
)

const (
	// maxArchive bounds a downloaded release archive; the largest published
	// asset (the universal macOS archive) is well below it.
	maxArchive = 128 << 20
	// maxBinary bounds the extracted executable.
	maxBinary = 256 << 20
	// maxDoc bounds the documentation files shipped next to the binary.
	maxDoc = 1 << 20
	// maxMembers bounds archive members; release archives hold three.
	maxMembers = 8
)

// extractBinary extracts the executable from a release archive into dir and
// returns its path. The archive may contain only the executable, LICENSE and
// README.md as regular files at its root; any other member, a symlink, a
// directory, a traversal path or an oversize member rejects the archive.
func extractBinary(archive, goos, dir string) (string, error) {
	name := release.BinaryName(goos)
	allowed := map[string]int64{name: maxBinary, "LICENSE": maxDoc, "README.md": maxDoc}
	seen := map[string]bool{}
	found := false
	member := func(path string, mode os.FileMode, regular bool, r io.Reader) error {
		if len(seen) >= maxMembers {
			return errors.New("too many members")
		}
		limit, ok := allowed[path]
		if !ok || path != filepath.Base(path) || strings.ContainsAny(path, `/\`) {
			return fmt.Errorf("unexpected member %q", path)
		}
		if seen[path] {
			return fmt.Errorf("member %q repeated", path)
		}
		seen[path] = true
		if !regular || mode&os.ModeType != 0 {
			return fmt.Errorf("member %q is not a regular file", path)
		}
		if path != name {
			n, err := io.Copy(io.Discard, io.LimitReader(r, limit+1))
			if err != nil {
				return err
			}
			if n > limit {
				return fmt.Errorf("member %q exceeds %d bytes", path, limit)
			}
			return nil
		}
		out, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
		if err != nil {
			return err
		}
		n, err := io.Copy(out, io.LimitReader(r, limit+1))
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err == nil && n > limit {
			err = fmt.Errorf("%s exceeds %d bytes", name, limit)
		}
		if err != nil {
			os.Remove(out.Name())
			return err
		}
		found = true
		return nil
	}
	var err error
	if strings.HasSuffix(archive, ".zip") {
		err = walkZip(archive, member)
	} else {
		err = walkTarGz(archive, member)
	}
	if err != nil {
		os.Remove(filepath.Join(dir, name))
		return "", coded("archive_invalid", err)
	}
	if !found {
		return "", coded("archive_invalid", fmt.Errorf("archive does not contain %s", name))
	}
	return filepath.Join(dir, name), nil
}

type memberFunc func(path string, mode os.FileMode, regular bool, r io.Reader) error

func walkTarGz(archive string, fn memberFunc) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for i := 0; ; i++ {
		if i > maxMembers {
			return errors.New("too many members")
		}
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(h.Name, h.FileInfo().Mode(), h.Typeflag == tar.TypeReg, tr); err != nil {
			return err
		}
	}
}

func walkZip(archive string, fn memberFunc) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	if len(zr.File) > maxMembers {
		return errors.New("too many members")
	}
	for _, zf := range zr.File {
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		// Sizes in the central directory are untrusted; the member callback
		// bounds what it reads.
		err = fn(zf.Name, zf.Mode(), zf.Mode().IsRegular(), rc)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
