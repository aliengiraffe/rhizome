package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"rhizome/internal/release"
)

func v(t *testing.T, tag string) release.Version {
	t.Helper()
	p, err := release.ParseTag(tag)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCompare(t *testing.T) {
	ordered := []string{"0.9.0", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.1.0", "2.0.0"}
	for i := range ordered {
		for j := range ordered {
			got := Compare(v(t, ordered[i]), v(t, ordered[j]))
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
	if Compare(v(t, "1.0.0+build.1"), v(t, "1.0.0+build.2")) != 0 {
		t.Error("build metadata affected precedence")
	}
}

func TestEligible(t *testing.T) {
	cases := []struct {
		current, candidate string
		want               bool
		reason             string
	}{
		{"1.0.0", "1.0.1", true, "newer"},
		{"1.0.0", "2.0.0", true, "newer"},
		{"1.0.0", "1.0.0", false, "not_newer"},
		{"1.0.1", "1.0.0", false, "not_newer"},
		{"1.0.0", "1.1.0-rc.1", false, "prerelease_skipped"},
		{"1.1.0-rc.1", "1.1.0-rc.2", true, "newer"},
		{"1.1.0-rc.1", "1.1.0", true, "newer"},
		{"1.1.0-rc.2", "1.1.0-rc.1", false, "not_newer"},
		{"1.1.0", "1.1.0-rc.3", false, "prerelease_skipped"},
		{"1.0.0+build.5", "1.0.0+build.6", false, "not_newer"},
	}
	for _, tc := range cases {
		got, reason := Eligible(v(t, tc.current), v(t, tc.candidate))
		if got != tc.want || reason != tc.reason {
			t.Errorf("Eligible(%s, %s) = %v %s, want %v %s", tc.current, tc.candidate, got, reason, tc.want, tc.reason)
		}
	}
}

func TestParseRunning(t *testing.T) {
	for _, bad := range []string{"", "dev", "0.0.0-snapshot+abc123", "v1", "1.2", "garbage"} {
		if _, ok := ParseRunning(bad); ok {
			t.Errorf("%q treated as a release build", bad)
		}
	}
	for _, good := range []string{"1.2.3", "v1.2.3", "1.2.3-rc.1", "1.2.3+build.9"} {
		if _, ok := ParseRunning(good); !ok {
			t.Errorf("%q rejected", good)
		}
	}
}

type member struct {
	name    string
	body    []byte
	symlink string
	dir     bool
}

func writeTarGz(t *testing.T, path string, members []member) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		h := &tar.Header{Name: m.name, Mode: 0o755, Size: int64(len(m.body)), Typeflag: tar.TypeReg}
		switch {
		case m.symlink != "":
			h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, m.symlink, 0
		case m.dir:
			h.Typeflag, h.Size = tar.TypeDir, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(m.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeZip(t *testing.T, path string, members []member) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, m := range members {
		h := &zip.FileHeader{Name: m.name, Method: zip.Deflate}
		h.SetMode(0o755)
		if m.symlink != "" {
			h.SetMode(os.ModeSymlink | 0o755)
		}
		if m.dir {
			h.Name += "/"
			h.SetMode(os.ModeDir | 0o755)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		body := m.body
		if m.symlink != "" {
			body = []byte(m.symlink)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExtractBinary(t *testing.T) {
	good := []member{{name: "rhizome", body: []byte("#!/bin/sh\necho hi\n")}, {name: "LICENSE", body: []byte("l")}, {name: "README.md", body: []byte("r")}}
	cases := []struct {
		name    string
		members []member
		ok      bool
	}{
		{"complete", good, true},
		{"binary only", good[:1], true},
		{"missing binary", good[1:], false},
		{"extra file", append(append([]member{}, good...), member{name: "extra", body: []byte("x")}), false},
		{"nested binary", []member{{name: "bin/rhizome", body: []byte("x")}}, false},
		{"traversal", []member{{name: "../rhizome", body: []byte("x")}}, false},
		{"absolute", []member{{name: "/rhizome", body: []byte("x")}}, false},
		{"symlink", []member{{name: "rhizome", symlink: "/bin/sh"}}, false},
		{"symlink doc", append(append([]member{}, good[:1]...), member{name: "LICENSE", symlink: "/etc/passwd"}), false},
		{"directory", append(append([]member{}, good...), member{name: "docs", dir: true}), false},
		{"duplicate", append(append([]member{}, good...), good[0]), false},
		{"oversize doc", []member{good[0], {name: "LICENSE", body: bytes.Repeat([]byte("x"), maxDoc+1)}}, false},
		{"windows name on unix", []member{{name: "rhizome.exe", body: []byte("x")}}, false},
	}
	for _, format := range []string{"tar.gz", "zip"} {
		for _, tc := range cases {
			t.Run(format+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				archive := filepath.Join(dir, "a."+format)
				if format == "zip" {
					writeZip(t, archive, tc.members)
				} else {
					writeTarGz(t, archive, tc.members)
				}
				out := filepath.Join(dir, "out")
				if err := os.Mkdir(out, 0o700); err != nil {
					t.Fatal(err)
				}
				bin, err := extractBinary(archive, "linux", out)
				if tc.ok != (err == nil) {
					t.Fatalf("ok=%v err=%v", tc.ok, err)
				}
				entries, _ := os.ReadDir(out)
				if !tc.ok {
					if Code(err) != "archive_invalid" {
						t.Fatalf("code %s", Code(err))
					}
					if len(entries) != 0 {
						t.Fatalf("rejected archive left %v", entries)
					}
					return
				}
				if len(entries) != 1 || entries[0].Name() != "rhizome" || bin != filepath.Join(out, "rhizome") {
					t.Fatalf("extracted %v (%s)", entries, bin)
				}
				st, _ := os.Stat(bin)
				if runtime.GOOS != "windows" && st.Mode().Perm()&0o100 == 0 {
					t.Fatal("binary not executable")
				}
			})
		}
	}
	dir := t.TempDir()
	writeZip(t, filepath.Join(dir, "w.zip"), []member{{name: "rhizome.exe", body: []byte("x")}})
	if _, err := extractBinary(filepath.Join(dir, "w.zip"), "windows", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := extractBinary(filepath.Join(dir, "missing.tar.gz"), "linux", dir); err == nil {
		t.Fatal("missing archive accepted")
	}
}

const goodCodesign = `Executable=/tmp/rhizome
Identifier=com.aliengiraffe.rhizome
Format=Mach-O universal (x86_64 arm64)
CodeDirectory v=20500 size=1234 flags=0x10000(runtime) hashes=10+2 location=embedded
Signature size=9000
Authority=Developer ID Application: Alien Giraffe Inc (6BY6T468D8)
Authority=Developer ID Certification Authority
Authority=Apple Root CA
Timestamp=Oct 20, 2026 at 10:00:00
TeamIdentifier=6BY6T468D8
Runtime Version=14.0.0
Sealed Resources=none
Internal requirements count=1 size=180
`

func TestCheckCodesignDisplay(t *testing.T) {
	cases := []struct{ name, info, want string }{
		{"release", goodCodesign, ""},
		{"adhoc", strings.Replace(goodCodesign, "Signature size=9000", "Signature=adhoc", 1), "ad-hoc"},
		{"other team", strings.ReplaceAll(goodCodesign, "6BY6T468D8", "ZZZZZZZZZZ"), "team identifier"},
		{"no developer id", strings.Replace(goodCodesign, "Authority=Developer ID Application: Alien Giraffe Inc (6BY6T468D8)\n", "Authority=Apple Development: someone\n", 1), "Developer ID"},
		{"no hardened runtime", strings.Replace(goodCodesign, "flags=0x10000(runtime)", "flags=0x0(none)", 1), "hardened runtime"},
		{"no timestamp", strings.Replace(goodCodesign, "Timestamp=Oct 20, 2026 at 10:00:00\n", "", 1), "timestamp"},
		{"other identifier", strings.Replace(goodCodesign, "Identifier=com.aliengiraffe.rhizome", "Identifier=com.example.rhizome", 1), "identifier"},
		{"unsigned", "code object is not signed at all", "Developer ID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkCodesignDisplay(tc.info)
			if tc.want == "" && err != nil {
				t.Fatal(err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCodesignVerifier(t *testing.T) {
	calls := 0
	fake := func(verifyErr error, info string) *codesignVerifier {
		return &codesignVerifier{run: func(_ context.Context, args ...string) (string, error) {
			calls++
			if args[0] == "--verify" {
				return "", verifyErr
			}
			return info, nil
		}}
	}
	if err := fake(nil, goodCodesign).Verify(context.Background(), "/x"); err != nil {
		t.Fatal(err)
	}
	if err := fake(errors.New("invalid signature"), goodCodesign).Verify(context.Background(), "/x"); Code(err) != "codesign_failed" {
		t.Fatalf("strict verify failure: %v", err)
	}
	if err := fake(nil, strings.Replace(goodCodesign, "Signature size=9000", "Signature=adhoc", 1)).Verify(context.Background(), "/x"); Code(err) != "codesign_failed" {
		t.Fatalf("ad-hoc: %v", err)
	}
	if PlatformVerifier("linux") != nil || PlatformVerifier("windows") != nil || PlatformVerifier("darwin") == nil {
		t.Fatal("platform verifier selection")
	}
}

func TestInstallAndRestore(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "rhizome")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(t.TempDir(), "candidate")
	if err := os.WriteFile(candidate, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkWritable(target); err != nil {
		t.Fatal(err)
	}
	staged, err := stageCandidate(candidate, target)
	if err != nil || filepath.Dir(staged) != dir {
		t.Fatal(staged, err)
	}
	backup, err := installBinary(staged, target)
	if err != nil || backup != target+BackupSuffix {
		t.Fatal(backup, err)
	}
	if b, _ := os.ReadFile(target); string(b) != "new" {
		t.Fatalf("target %q", b)
	}
	if b, _ := os.ReadFile(backup); string(b) != "old" {
		t.Fatalf("backup %q", b)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("stray files: %v", entries)
	}
	if err := restoreBinary(backup, target); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "old" {
		t.Fatalf("restored %q", b)
	}
	entries, _ = os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("restore left %v", entries)
	}
	if err := restoreBinary(backup, target); err == nil {
		t.Fatal("restore without backup succeeded")
	}
	// A second install replaces the single backup rather than accumulating.
	for i := 0; i < 2; i++ {
		staged, err = stageCandidate(candidate, target)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = installBinary(staged, target); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ = os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("backups accumulated: %v", entries)
	}
}

func TestNotWritable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("directory permissions are not enforced for this user")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "rhizome")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if err := checkWritable(target); !errors.Is(err, ErrNotWritable) {
		t.Fatalf("got %v", err)
	}
	if _, err := stageCandidate(target, target); !errors.Is(err, ErrNotWritable) {
		t.Fatalf("stage: %v", err)
	}
}

func TestStateRoundTrip(t *testing.T) {
	run := filepath.Join(t.TempDir(), "run")
	s, err := ReadState(run)
	if err != nil || s.Version != "" {
		t.Fatal(s, err)
	}
	s.Version, s.LastOutcome, s.Pending = "1.0.0", OutcomeUpdated, &Pending{From: "0.9.0", To: "1.0.0"}
	if err := writeState(run, s); err != nil {
		t.Fatal(err)
	}
	got, err := ReadState(run)
	if err != nil || got.Version != "1.0.0" || got.Pending == nil || got.Pending.To != "1.0.0" {
		t.Fatal(got, err)
	}
	st, _ := os.Stat(filepath.Join(run, StateFile))
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("state file mode %v", st.Mode())
	}
	if err := os.WriteFile(filepath.Join(run, StateFile), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadState(run); err == nil {
		t.Fatal("malformed state accepted")
	}
}
