package release

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// maxManifest bounds the checksum manifest read from disk.
const maxManifest = 64 << 10

// Platforms lists every published archive as goos/goarch; darwin ships one
// universal binary named "all".
var Platforms = []string{"darwin/all", "linux/amd64", "linux/arm64", "windows/amd64", "windows/arm64"}

// ArchiveName returns the archive published for goos/goarch.
func ArchiveName(assetVersion, goos, goarch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("rhizome_%s_%s_%s%s", assetVersion, goos, goarch, ext)
}

// PackageName returns the macOS installer package name.
func PackageName(assetVersion string) string {
	return fmt.Sprintf("rhizome_%s_macos.pkg", assetVersion)
}

// ManifestName returns the SHA-256 manifest name.
func ManifestName(assetVersion string) string {
	return fmt.Sprintf("rhizome_%s_checksums.txt", assetVersion)
}

// Assets lists every downloadable artifact the manifest must cover, sorted.
// The manifest itself is published alongside them.
func Assets(assetVersion string) []string {
	out := []string{PackageName(assetVersion)}
	for _, p := range Platforms {
		goos, goarch, _ := strings.Cut(p, "/")
		out = append(out, ArchiveName(assetVersion, goos, goarch))
	}
	slices.Sort(out)
	return out
}

// WriteManifest writes "<sha256>  <name>" lines for every expected asset in
// dir to the manifest file. A missing asset is an error.
func WriteManifest(dir, assetVersion string) error {
	var b bytes.Buffer
	for _, name := range Assets(assetVersion) {
		sum, err := fileSHA256(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("checksum %s: %w", name, err)
		}
		fmt.Fprintf(&b, "%s  %s\n", sum, name)
	}
	return os.WriteFile(filepath.Join(dir, ManifestName(assetVersion)), b.Bytes(), 0o644)
}

// VerifyManifest checks that the manifest in dir lists exactly the expected
// assets once each and that every file matches its digest. When exact is set,
// dir must contain nothing besides those assets and the manifest, which is how
// downloaded release assets are checked for strays.
func VerifyManifest(dir, assetVersion string, exact bool) error {
	_, err := verifiedSums(dir, assetVersion, exact)
	return err
}

// verifiedSums verifies dir like VerifyManifest and returns the manifest's
// digests by asset name.
func verifiedSums(dir, assetVersion string, exact bool) (map[string]string, error) {
	f, err := os.Open(filepath.Join(dir, ManifestName(assetVersion)))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxManifest+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxManifest {
		return nil, errors.New("checksum manifest too large")
	}
	want := Assets(assetVersion)
	seen := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		sum, name, ok := strings.Cut(sc.Text(), "  ")
		if !ok || len(sum) != sha256.Size*2 || strings.ToLower(sum) != sum {
			return nil, fmt.Errorf("malformed manifest line %q", sc.Text())
		}
		if _, err := hex.DecodeString(sum); err != nil {
			return nil, fmt.Errorf("malformed digest for %s", name)
		}
		if !slices.Contains(want, name) {
			return nil, fmt.Errorf("manifest lists unexpected asset %q", name)
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("manifest lists %s twice", name)
		}
		seen[name] = sum
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	for _, name := range want {
		sum, ok := seen[name]
		if !ok {
			return nil, fmt.Errorf("manifest is missing %s", name)
		}
		got, err := fileSHA256(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("checksum %s: %w", name, err)
		}
		if got != sum {
			return nil, fmt.Errorf("%s: digest %s does not match manifest %s", name, got, sum)
		}
	}
	if exact {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.Name() != ManifestName(assetVersion) && !slices.Contains(want, e.Name()) {
				return nil, fmt.Errorf("unexpected file %s alongside release assets", e.Name())
			}
		}
	}
	return seen, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
