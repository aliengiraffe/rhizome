package release

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeTarGz(t *testing.T, path string, members map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, body := range members {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []interface{ Close() error }{tw, gz, f} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func writeZip(t *testing.T, path string, members map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range members {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestExtractBinary(t *testing.T) {
	dir := t.TempDir()
	tgz := filepath.Join(dir, "a.tar.gz")
	writeTarGz(t, tgz, map[string]string{"LICENSE": "l", "rhizome": "bin"})
	zipped := filepath.Join(dir, "a.zip")
	writeZip(t, zipped, map[string]string{"README.md": "r", "rhizome.exe": "exe"})
	for archive, goos := range map[string]string{tgz: "linux", zipped: "windows"} {
		out := t.TempDir()
		bin, err := ExtractBinary(archive, goos, out)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(bin) != BinaryName(goos) {
			t.Fatalf("extracted %s", bin)
		}
		entries, _ := os.ReadDir(out)
		if len(entries) != 1 {
			t.Fatalf("ExtractBinary wrote %d files", len(entries))
		}
	}

	// The binary must sit at the archive root under its platform name.
	bad := filepath.Join(dir, "bad.tar.gz")
	writeTarGz(t, bad, map[string]string{"rhizome_1.2.3/rhizome": "bin", "../rhizome": "x"})
	if _, err := ExtractBinary(bad, "linux", t.TempDir()); err == nil {
		t.Fatal("ExtractBinary accepted a nested or traversing binary")
	}
	if _, err := ExtractBinary(zipped, "linux", t.TempDir()); err == nil {
		t.Fatal("ExtractBinary accepted a zip without rhizome")
	}
}

func TestSmoke(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("smoke fixtures are shell scripts")
	}
	v, err := ParseTag("v1.3.0-rc.1")
	if err != nil {
		t.Fatal(err)
	}
	goos, goarch := runtime.GOOS, runtime.GOARCH
	tests := []struct {
		name, script, commit, want string
	}{
		{"matching", "echo 'rhizome 1.3.0-rc.1 (commit abc123, built 2026-10-21T00:00:00Z)'", "abc123", ""},
		{"wrong version", "echo 'rhizome 1.3.0 (commit abc123, built x)'", "abc123", "want prefix"},
		{"wrong commit", "echo 'rhizome 1.3.0-rc.1 (commit def456, built x)'", "abc123", "want prefix"},
		{"failing binary", "echo killed; exit 137", "abc123", "exit status 137"},
		{"extra output", "echo 'rhizome 1.3.0-rc.1 (commit abc123, built x)'; echo more", "abc123", "want prefix"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeAssets(t, v.AssetVersion())
			arch := goarch
			if goos == "darwin" {
				arch = "all"
			}
			writeTarGz(t, filepath.Join(dir, ArchiveName(v.AssetVersion(), goos, arch)), map[string]string{"rhizome": "#!/bin/sh\n" + tt.script + "\n"})
			if err := WriteManifest(dir, v.AssetVersion()); err != nil {
				t.Fatal(err)
			}
			line, err := Smoke(context.Background(), dir, v, tt.commit, goos, arch)
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if !strings.HasPrefix(line, "rhizome 1.3.0-rc.1") {
					t.Fatalf("Smoke = %q", line)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Smoke = %q, %v; want error containing %q", line, err, tt.want)
			}
		})
	}
}
