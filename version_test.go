package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionLine(t *testing.T) {
	tests := []struct {
		name, version, commit, date string
		want                        []string
	}{
		{"release", "1.3.0-rc.1+build.5", "0123abc", "2026-10-21T00:00:00Z", []string{"rhizome 1.3.0-rc.1+build.5 (commit 0123abc, built 2026-10-21T00:00:00Z)"}},
		{"local", "", "", "", []string{"rhizome dev (commit ", ", built "}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func(v, c, d string) { version, commit, date = v, c, d }(version, commit, date)
			version, commit, date = tt.version, tt.commit, tt.date
			got := versionLine()
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Fatalf("versionLine() = %q, want it to contain %q", got, w)
				}
			}
		})
	}
}

// TestVersionIgnoresConfiguration proves --version needs no configuration and
// writes nothing: an invalid config file and a missing RHIZOME_CONFIG would
// fail every other command.
func TestVersionIgnoresConfiguration(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".rhizome"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".rhizome", "config.toml"), []byte("not = [valid"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("RHIZOME_CONFIG", filepath.Join(home, "missing.toml"))
	t.Setenv("RHIZOME_RUNTIME_DIR", filepath.Join(home, "runtime"))

	for _, arg := range []string{"--version", "-version"} {
		out, err := runEntry(t, "rhizome", arg)
		if err != nil {
			t.Fatalf("%s: %v", arg, err)
		}
		if !strings.HasPrefix(out, "rhizome ") || strings.Count(out, "\n") != 1 {
			t.Fatalf("%s printed %q, want one version line", arg, out)
		}
	}
	if _, err := runEntry(t, "rhizome", "--version", "extra"); err == nil {
		t.Fatal("--version with extra arguments should fail")
	}
	var files []string
	filepath.WalkDir(home, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if len(files) != 1 {
		t.Fatalf("--version wrote files: %v", files)
	}
}

func runEntry(t *testing.T, args ...string) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldArgs, oldStdout := os.Args, os.Stdout
	os.Args, os.Stdout = args, w
	defer func() { os.Args, os.Stdout = oldArgs, oldStdout }()
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	runErr := entry()
	w.Close()
	return <-done, runErr
}
