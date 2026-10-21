package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests drive scripts/release with stubbed Apple tools. They cover the
// scripts' gating logic only; they do not prove real signing or notarization.

func scriptsDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("release scripts run on macOS and Linux runners")
	}
	for _, tool := range []string{"bash", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	dir, err := filepath.Abs(filepath.Join("..", "..", "scripts", "release"))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// stubs writes executable shell stubs and returns a PATH that finds them first.
// Each stub also records its invocation in calls.log.
func stubs(t *testing.T, tools map[string]string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	for name, body := range tools {
		script := "#!/bin/sh\necho \"" + name + " $*\" >>'" + log + "'\n" + body + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir + string(os.PathListSeparator) + os.Getenv("PATH"), log
}

func runScript(t *testing.T, path string, env []string, script string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"-c", script, "test"}, args...)...)
	cmd.Env = append([]string{"PATH=" + path, "HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir()}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func notaryEnv(t *testing.T) []string {
	t.Helper()
	p8 := filepath.Join(t.TempDir(), "key.p8")
	if err := os.WriteFile(p8, []byte("not a real key"), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"APPLE_NOTARY_KEY_P8=" + p8, "APPLE_NOTARY_KEY_ID=KEYID", "APPLE_NOTARY_ISSUER_ID=issuer"}
}

func TestNotarizeSubmissionRequiresAccepted(t *testing.T) {
	dir := scriptsDir(t)
	const id = "2efe2717-52ef-43a5-96dc-0797e4ca1041"
	tests := []struct {
		name, output string
		exit         int
		ok           bool
	}{
		{"accepted", `{"id":"` + id + `","status":"Accepted","message":"Processing complete"}`, 0, true},
		{"invalid with exit zero", `{"id":"` + id + `","status":"Invalid","message":"Processing complete"}`, 0, false},
		{"rejected", `{"id":"` + id + `","status":"Rejected"}`, 0, false},
		{"still in progress", `{"id":"` + id + `","status":"In Progress"}`, 0, false},
		{"accepted but nonzero exit", `{"id":"` + id + `","status":"Accepted"}`, 1, false},
		{"accepted without id", `{"status":"Accepted"}`, 0, false},
		{"malformed", `Current status: Accepted`, 0, false},
		{"not an object", `["Accepted"]`, 0, false},
		{"status wrong type", `{"id":"` + id + `","status":true}`, 0, false},
		{"empty", ``, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, log := stubs(t, map[string]string{
				"xcrun": `[ "$2" = submit ] || exit 0; printf '%s' '` + tt.output + `'; exit ` + string(rune('0'+tt.exit)),
			})
			scratch := t.TempDir()
			out, err := runScript(t, path, notaryEnv(t), `set -euo pipefail; . "$1/lib.sh"; notarize_submission "$2" "$3"`,
				dir, filepath.Join(scratch, "rhizome.zip"), scratch)
			if (err == nil) != tt.ok {
				t.Fatalf("notarize_submission ok=%t, want %t:\n%s", err == nil, tt.ok, out)
			}
			calls, _ := os.ReadFile(log)
			if !strings.Contains(string(calls), "notarytool submit") || !strings.Contains(string(calls), "--output-format json") {
				t.Fatalf("notarytool not invoked as expected:\n%s", calls)
			}
			if !tt.ok && !strings.Contains(out, "expected Accepted") {
				t.Fatalf("failure does not explain the verdict:\n%s", out)
			}
		})
	}
}

func TestNotarizeSubmissionMissingCredentials(t *testing.T) {
	dir := scriptsDir(t)
	path, log := stubs(t, map[string]string{"xcrun": "exit 0"})
	scratch := t.TempDir()
	out, err := runScript(t, path, []string{"APPLE_NOTARY_KEY_ID=KEYID"},
		`set -euo pipefail; . "$1/lib.sh"; notarize_submission "$2" "$2"`, dir, scratch)
	if err == nil || !strings.Contains(out, "APPLE_NOTARY_KEY_P8 APPLE_NOTARY_ISSUER_ID") {
		t.Fatalf("missing credentials accepted:\n%s", out)
	}
	if _, err := os.Stat(log); err == nil {
		t.Fatal("xcrun ran without credentials")
	}
}

func TestSigningPolicyRejectsUnknownMode(t *testing.T) {
	dir := scriptsDir(t)
	out, err := runScript(t, os.Getenv("PATH"), []string{"RHIZOME_SIGNING=maybe"}, `. "$1/lib.sh"`, dir)
	if err == nil || !strings.Contains(out, "RHIZOME_SIGNING") {
		t.Fatalf("unknown signing mode accepted:\n%s", out)
	}
}

const developerIDDisplay = `Executable=/tmp/rhizome
Identifier=com.aliengiraffe.rhizome
Format=Mach-O universal (x86_64 arm64)
CodeDirectory v=20500 size=1 flags=0x10000(runtime) hashes=1+2 location=embedded
Signature size=9000
Authority=Developer ID Application: Alien Giraffe (ABCDE12345)
Authority=Developer ID Certification Authority
Authority=Apple Root CA
Timestamp=Oct 21, 2026 at 10:00:00 AM
TeamIdentifier=ABCDE12345`

func TestCheckReleaseSignature(t *testing.T) {
	dir := scriptsDir(t)
	tests := []struct {
		name, display, want string
	}{
		{"developer id", developerIDDisplay, ""},
		{"ad-hoc", strings.NewReplacer("Signature size=9000", "Signature=adhoc",
			"Authority=Developer ID Application: Alien Giraffe (ABCDE12345)\n", "",
			"TeamIdentifier=ABCDE12345", "TeamIdentifier=not set").Replace(developerIDDisplay), "ad-hoc signature"},
		{"no hardened runtime", strings.Replace(developerIDDisplay, "flags=0x10000(runtime)", "flags=0x0(none)", 1), "hardened runtime"},
		{"no timestamp", strings.Replace(developerIDDisplay, "Timestamp=Oct 21, 2026 at 10:00:00 AM\n", "", 1), "secure timestamp"},
		{"apple development cert", strings.Replace(developerIDDisplay, "Developer ID Application:", "Apple Development:", 1), "Developer ID Application"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, _ := stubs(t, map[string]string{"codesign": "cat >&2 <<'OUT'\n" + tt.display + "\nOUT"})
			out, err := runScript(t, path, nil, `set -euo pipefail; . "$1/lib.sh"; check_release_signature /tmp/rhizome`, dir)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("valid signature rejected:\n%s", out)
				}
				return
			}
			if err == nil || !strings.Contains(out, tt.want) {
				t.Fatalf("got err=%v, want failure mentioning %q:\n%s", err, tt.want, out)
			}
		})
	}
	path, _ := stubs(t, map[string]string{"codesign": "echo 'code object is not signed at all' >&2; exit 1"})
	if out, err := runScript(t, path, nil, `set -euo pipefail; . "$1/lib.sh"; check_release_signature /tmp/rhizome`, dir); err == nil {
		t.Fatalf("unsigned binary accepted:\n%s", out)
	}
}

func TestRunGateChecksVersion(t *testing.T) {
	dir := scriptsDir(t)
	bin := filepath.Join(t.TempDir(), "rhizome")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'rhizome 1.2.3 (commit abc, built x)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gate := `set -euo pipefail; . "$1/lib.sh"; run_gate test "$2"`
	if out, err := runScript(t, os.Getenv("PATH"), []string{"RHIZOME_EXPECT_VERSION=1.2.3"}, gate, dir, bin); err != nil {
		t.Fatalf("matching version rejected:\n%s", out)
	}
	if out, err := runScript(t, os.Getenv("PATH"), []string{"RHIZOME_EXPECT_VERSION=1.2.4"}, gate, dir, bin); err == nil {
		t.Fatalf("mismatched version accepted:\n%s", out)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nkill -9 $$\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := runScript(t, os.Getenv("PATH"), nil, gate, dir, bin); err == nil || !strings.Contains(out, "137") {
		t.Fatalf("killed binary accepted:\n%s", out)
	}
}

func TestSignScriptProductionPrerequisites(t *testing.T) {
	dir := scriptsDir(t)
	bin := filepath.Join(t.TempDir(), "rhizome")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	darwin := map[string]string{
		"uname":    `[ "$1" = -m ] && echo arm64 || echo Darwin`,
		"lipo":     "echo x86_64 arm64",
		"security": "exit 0",
		"codesign": "exit 0",
		"xcrun":    "exit 0",
	}
	tests := []struct {
		name  string
		tools map[string]string
		env   []string
		want  string
		ok    bool
	}{
		{"missing notary credentials", darwin, []string{"RHIZOME_SIGNING=required"}, "missing: APPLE_NOTARY_KEY_P8 APPLE_NOTARY_KEY_ID APPLE_NOTARY_ISSUER_ID", false},
		{"missing identity", darwin, append(notaryEnv(t), "RHIZOME_SIGNING=required", "RHIZOME_SIGN_KEYCHAIN=/tmp/none"), "no Developer ID Application identity", false},
		{"production off macOS", map[string]string{"uname": "echo Linux"}, append(notaryEnv(t), "RHIZOME_SIGNING=required"), "needs a macOS runner", false},
		{"thin binary", map[string]string{"uname": "echo Darwin", "lipo": "echo arm64"}, nil, "lacks an x86_64 slice", false},
		{"snapshot off macOS", map[string]string{"uname": "echo Linux"}, nil, "skipping the gate", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, log := stubs(t, tt.tools)
			out, err := runScript(t, path, tt.env, `bash "$1/sign-macos-binary.sh" "$2"`, dir, bin)
			if (err == nil) != tt.ok || !strings.Contains(out, tt.want) {
				t.Fatalf("err=%v, want ok=%t and output containing %q:\n%s", err, tt.ok, tt.want, out)
			}
			calls, _ := os.ReadFile(log)
			if strings.Contains("\n"+string(calls), "\ncodesign ") || strings.Contains("\n"+string(calls), "\nxcrun ") {
				t.Fatalf("signing tools ran despite failed prerequisites:\n%s", calls)
			}
		})
	}
}

func TestPackageScriptProductionPrerequisites(t *testing.T) {
	dir := scriptsDir(t)
	bin := filepath.Join(t.TempDir(), "rhizome")
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	path, log := stubs(t, map[string]string{"uname": "echo Darwin", "security": "exit 0", "pkgbuild": "exit 0", "productbuild": "exit 0"})
	tests := []struct {
		name string
		env  []string
		want string
	}{
		{"missing installer identity", append(notaryEnv(t), "RHIZOME_SIGNING=required", "RHIZOME_SIGN_KEYCHAIN=/tmp/none"), "no Developer ID Installer identity"},
		{"missing notary credentials", []string{"RHIZOME_SIGNING=required", "APPLE_INSTALLER_IDENTITY=Developer ID Installer: X (ABCDE12345)"}, "missing: APPLE_NOTARY_KEY_P8"},
		{"installer-unsafe version", []string{"RHIZOME_PKG_VERSION=1.2.3-rc.1"}, "must be numeric"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := append([]string{"RHIZOME_BINARY=" + bin, "RHIZOME_PKG_VERSION=1.2.3", "RHIZOME_PKG_OUT=" + filepath.Join(t.TempDir(), "r.pkg")}, tt.env...)
			out, err := runScript(t, path, env, `bash "$1/build-macos-pkg.sh"`, dir)
			if err == nil || !strings.Contains(out, tt.want) {
				t.Fatalf("err=%v, want failure containing %q:\n%s", err, tt.want, out)
			}
			calls, _ := os.ReadFile(log)
			if strings.Contains(string(calls), "pkgbuild") {
				t.Fatalf("package built despite failed prerequisites:\n%s", calls)
			}
		})
	}
}

// TestPackagePayload builds a real unsigned package around an ad-hoc signed
// copy of a system binary and checks the payload verifier. macOS only;
// nothing is installed.
func TestPackagePayload(t *testing.T) {
	dir := scriptsDir(t)
	if runtime.GOOS != "darwin" {
		t.Skip("pkgbuild and pkgutil are macOS tools")
	}
	if _, err := exec.LookPath("pkgbuild"); err != nil {
		t.Skip("pkgbuild not available")
	}
	tmp := t.TempDir()
	// A copied platform binary is killed outside system paths; re-sign it
	// ad-hoc so the execution gate can run it.
	copyFile := func(src, dst string) {
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("codesign", "--force", "--sign", "-", dst).CombinedOutput(); err != nil {
			t.Fatalf("codesign: %v\n%s", err, out)
		}
	}
	bin, other := filepath.Join(tmp, "rhizome"), filepath.Join(tmp, "other")
	copyFile("/bin/echo", bin)
	copyFile("/bin/cat", other)
	pkg := filepath.Join(tmp, "rhizome.pkg")
	env := []string{"RHIZOME_BINARY=" + bin, "RHIZOME_PKG_VERSION=1.2.3", "RHIZOME_PKG_OUT=" + pkg}
	if out, err := runScript(t, os.Getenv("PATH"), env, `bash "$1/build-macos-pkg.sh"`, dir); err != nil {
		t.Fatalf("build-macos-pkg.sh: %v\n%s", err, out)
	}
	if out, err := runScript(t, os.Getenv("PATH"), nil, `bash "$1/verify-pkg-payload.sh" "$2" "$3"`, dir, pkg, bin); err != nil {
		t.Fatalf("matching payload rejected: %v\n%s", err, out)
	}
	if out, err := runScript(t, os.Getenv("PATH"), nil, `bash "$1/verify-pkg-payload.sh" "$2" "$3"`, dir, pkg, other); err == nil || !strings.Contains(out, "does not match") {
		t.Fatalf("mismatched payload accepted: %v\n%s", err, out)
	}
	// An ad-hoc payload can never pass the production policy.
	if out, err := runScript(t, os.Getenv("PATH"), []string{"RHIZOME_SIGNING=required"}, `bash "$1/verify-pkg-payload.sh" "$2" "$3"`, dir, pkg, bin); err == nil {
		t.Fatalf("ad-hoc payload passed the Developer ID policy:\n%s", out)
	}
}
