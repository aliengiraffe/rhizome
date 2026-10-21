package release

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestAssetsCoverMatrix(t *testing.T) {
	got := Assets("1.2.3")
	want := []string{
		"rhizome_1.2.3_darwin_all.tar.gz",
		"rhizome_1.2.3_linux_amd64.tar.gz",
		"rhizome_1.2.3_linux_arm64.tar.gz",
		"rhizome_1.2.3_macos.pkg",
		"rhizome_1.2.3_windows_amd64.zip",
		"rhizome_1.2.3_windows_arm64.zip",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Assets = %v, want %v", got, want)
	}
}

func writeAssets(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range Assets(version) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("content of "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestManifestRoundTrip(t *testing.T) {
	dir := writeAssets(t, "1.2.3")
	if err := WriteManifest(dir, "1.2.3"); err != nil {
		t.Fatal(err)
	}
	if err := VerifyManifest(dir, "1.2.3", true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ManifestName("1.2.3")))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(data), "\n"); lines != len(Assets("1.2.3")) {
		t.Fatalf("manifest has %d lines:\n%s", lines, data)
	}
}

func TestWriteManifestRequiresEveryAsset(t *testing.T) {
	dir := writeAssets(t, "1.2.3")
	if err := os.Remove(filepath.Join(dir, PackageName("1.2.3"))); err != nil {
		t.Fatal(err)
	}
	if err := WriteManifest(dir, "1.2.3"); err == nil || !strings.Contains(err.Error(), "macos.pkg") {
		t.Fatalf("WriteManifest without the package: %v", err)
	}
}

func TestVerifyManifestRejects(t *testing.T) {
	const v = "1.2.3"
	pkg := PackageName(v)
	tests := []struct {
		name   string
		mutate func(t *testing.T, dir, manifest string)
		exact  bool
		want   string
	}{
		{"tampered asset", func(t *testing.T, dir, _ string) {
			write(t, filepath.Join(dir, pkg), "re-signed")
		}, false, "does not match"},
		{"missing asset file", func(t *testing.T, dir, _ string) {
			os.Remove(filepath.Join(dir, pkg))
		}, false, pkg},
		{"asset missing from manifest", func(t *testing.T, _, manifest string) {
			edit(t, manifest, func(lines []string) []string {
				return slices.DeleteFunc(lines, func(l string) bool { return strings.HasSuffix(l, pkg) })
			})
		}, false, "missing " + pkg},
		{"duplicate line", func(t *testing.T, _, manifest string) {
			edit(t, manifest, func(lines []string) []string { return append(lines, lines[0]) })
		}, false, "twice"},
		{"unexpected line", func(t *testing.T, _, manifest string) {
			edit(t, manifest, func(lines []string) []string {
				return append(lines, strings.Repeat("0", 64)+"  extra.tar.gz")
			})
		}, false, "unexpected asset"},
		{"malformed line", func(t *testing.T, _, manifest string) {
			edit(t, manifest, func(lines []string) []string { return append(lines, "garbage") })
		}, false, "malformed"},
		{"stray file", func(t *testing.T, dir, _ string) {
			write(t, filepath.Join(dir, "checksums.txt"), "")
		}, true, "unexpected file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeAssets(t, v)
			if err := WriteManifest(dir, v); err != nil {
				t.Fatal(err)
			}
			tt.mutate(t, dir, filepath.Join(dir, ManifestName(v)))
			err := VerifyManifest(dir, v, tt.exact)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("VerifyManifest = %v, want error containing %q", err, tt.want)
			}
		})
	}
	if err := VerifyManifest(t.TempDir(), v, false); err == nil {
		t.Fatal("VerifyManifest without a manifest succeeded")
	}
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func edit(t *testing.T, path string, f func([]string) []string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := f(strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"))
	write(t, path, strings.Join(lines, "\n")+"\n")
}
