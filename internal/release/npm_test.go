package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestNPMVersion(t *testing.T) {
	tests := []struct {
		tag, version, distTag string
	}{
		{"v1.2.3", "1.2.3", "latest"},
		{"1.2.3", "1.2.3", "latest"},
		{"v1.3.0-rc.1", "1.3.0-rc.1", "next"},
		{"v1.2.3+build.5", "1.2.3", "latest"},
		{"v1.3.0-rc.1+build.5", "1.3.0-rc.1", "next"},
	}
	for _, tt := range tests {
		v, err := ParseTag(tt.tag)
		if err != nil {
			t.Fatal(err)
		}
		if v.NPMVersion() != tt.version || v.NPMDistTag() != tt.distTag {
			t.Errorf("%s: npm %s@%s, want %s@%s", tt.tag, v.NPMVersion(), v.NPMDistTag(), tt.version, tt.distTag)
		}
	}
	snap, err := Snapshot("0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if snap.NPMVersion() != "0.0.0-snapshot-0123456789ab" || snap.NPMDistTag() != "next" {
		t.Errorf("snapshot: npm %s@%s", snap.NPMVersion(), snap.NPMDistTag())
	}
	// The snapshot version must itself be valid SemVer, even for an
	// all-digit commit prefix.
	if digits, _ := Snapshot("0123456789012"); !semver.MatchString(digits.NPMVersion()) {
		t.Errorf("snapshot npm version %q is not SemVer", digits.NPMVersion())
	}
}

// writeReleaseAssets writes real archives for every platform whose binary is
// script (a shell script printing a --version line for v and commit), a
// placeholder installer package and the checksum manifest.
func writeReleaseAssets(t *testing.T, v Version, commit string) string {
	t.Helper()
	dir := writeAssets(t, v.AssetVersion())
	script := "#!/bin/sh\necho 'rhizome " + v.Version + " (commit " + commit + ", built 2026-11-02T00:00:00Z)'\n"
	for _, p := range Platforms {
		goos, goarch, _ := strings.Cut(p, "/")
		members := map[string]string{BinaryName(goos): script, "LICENSE": "MIT License\n", "README.md": "readme\n"}
		path := filepath.Join(dir, ArchiveName(v.AssetVersion(), goos, goarch))
		if goos == "windows" {
			writeZip(t, path, members)
		} else {
			writeTarGz(t, path, members)
		}
	}
	if err := WriteManifest(dir, v.AssetVersion()); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestWriteNPMPackages(t *testing.T) {
	v, err := ParseTag("v1.3.0-rc.1+build.5")
	if err != nil {
		t.Fatal(err)
	}
	assets := writeReleaseAssets(t, v, "abc123")
	out := filepath.Join(t.TempDir(), "npm")
	dirs, err := WriteNPMPackages(assets, out, v)
	if err != nil {
		t.Fatal(err)
	}
	wantDirs := []string{"rhizome-darwin", "rhizome-linux-x64", "rhizome-linux-arm64", "rhizome-win32-x64", "rhizome-win32-arm64", "rhizome"}
	var gotDirs []string
	for _, d := range dirs {
		gotDirs = append(gotDirs, filepath.Base(d))
	}
	if !slices.Equal(gotDirs, wantDirs) {
		t.Fatalf("package directories %v, want %v (platforms first, launcher last)", gotDirs, wantDirs)
	}

	platforms := map[string]struct {
		os, cpu []string
		bin     string
	}{
		"rhizome-darwin":      {[]string{"darwin"}, []string{"x64", "arm64"}, "rhizome"},
		"rhizome-linux-x64":   {[]string{"linux"}, []string{"x64"}, "rhizome"},
		"rhizome-linux-arm64": {[]string{"linux"}, []string{"arm64"}, "rhizome"},
		"rhizome-win32-x64":   {[]string{"win32"}, []string{"x64"}, "rhizome.exe"},
		"rhizome-win32-arm64": {[]string{"win32"}, []string{"arm64"}, "rhizome.exe"},
	}
	for _, d := range dirs {
		base := filepath.Base(d)
		raw, err := os.ReadFile(filepath.Join(d, "package.json"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(raw, []byte(`"node": ">=18"`)) && base == "rhizome" {
			t.Errorf("launcher package.json escapes engines: %s", raw)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		// No lifecycle scripts, and no libc constraint on static binaries.
		for _, forbidden := range []string{"scripts", "libc", "preinstall", "install", "postinstall"} {
			if _, ok := fields[forbidden]; ok {
				t.Errorf("%s/package.json has %q", base, forbidden)
			}
		}
		var m struct {
			npmManifest
			LibC []string `json:"libc"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		if m.Version != "1.3.0-rc.1" || m.Rhizome.Tag != v.Tag {
			t.Errorf("%s: version %s tag %s", base, m.Version, m.Rhizome.Tag)
		}
		if m.Repository != (npmRepository{"git", "git+https://github.com/aliengiraffe/rhizome.git"}) {
			t.Errorf("%s: repository %+v", base, m.Repository)
		}
		if m.PublishConfig.Access != "public" || m.License != "MIT" || m.Homepage == "" || m.Description == "" {
			t.Errorf("%s: incomplete metadata %+v", base, m.npmManifest)
		}
		for _, f := range []string{"README.md", "LICENSE"} {
			if _, err := os.Stat(filepath.Join(d, f)); err != nil {
				t.Errorf("%s: %v", base, err)
			}
		}
		if base == "rhizome" {
			if m.Name != "@aliengiraffe/rhizome" || len(m.OS) != 0 || len(m.CPU) != 0 {
				t.Errorf("launcher package %+v", m.npmManifest)
			}
			if m.Bin["rhizome"] != "bin/rhizome.js" || m.Engines["node"] != ">=18" {
				t.Errorf("launcher bin %v engines %v", m.Bin, m.Engines)
			}
			want := map[string]string{}
			for name := range platforms {
				want["@aliengiraffe/"+name] = "1.3.0-rc.1"
			}
			if !mapsEqual(m.OptionalDependencies, want) {
				t.Errorf("optionalDependencies %v, want %v", m.OptionalDependencies, want)
			}
			launcher, err := os.ReadFile(filepath.Join(d, "bin", "rhizome.js"))
			if err != nil || !bytes.Equal(launcher, npmLauncher) || !bytes.HasPrefix(launcher, []byte("#!/usr/bin/env node\n")) {
				t.Errorf("launcher bin/rhizome.js: %v", err)
			}
			continue
		}
		want := platforms[base]
		if m.Name != "@aliengiraffe/"+base || !slices.Equal(m.OS, want.os) || !slices.Equal(m.CPU, want.cpu) {
			t.Errorf("%s: name %s os %v cpu %v", base, m.Name, m.OS, m.CPU)
		}
		if m.Bin != nil || m.OptionalDependencies != nil || m.LibC != nil {
			t.Errorf("%s: platform package declares bin, dependencies or libc", base)
		}
		st, err := os.Stat(filepath.Join(d, "bin", want.bin))
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && st.Mode().Perm() != 0o755 {
			t.Errorf("%s: binary mode %v, want 0755", base, st.Mode().Perm())
		}
		entries, _ := os.ReadDir(filepath.Join(d, "bin"))
		if len(entries) != 1 {
			t.Errorf("%s: bin holds %d files", base, len(entries))
		}
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestWriteNPMPackagesRejects(t *testing.T) {
	v, err := ParseTag("v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	linux := ArchiveName(v.AssetVersion(), "linux", "arm64")
	tests := []struct {
		name   string
		mutate func(t *testing.T, assets, out string)
		want   string
	}{
		{"missing asset", func(t *testing.T, assets, _ string) {
			os.Remove(filepath.Join(assets, linux))
		}, linux},
		{"checksum mismatch", func(t *testing.T, assets, _ string) {
			writeTarGz(t, filepath.Join(assets, linux), map[string]string{"rhizome": "tampered", "LICENSE": "l"})
		}, "does not match"},
		{"missing manifest", func(t *testing.T, assets, _ string) {
			os.Remove(filepath.Join(assets, ManifestName(v.AssetVersion())))
		}, "checksums.txt"},
		{"stale output", func(t *testing.T, _, out string) {
			if err := os.MkdirAll(filepath.Join(out, "rhizome-darwin"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "exists"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assets := writeReleaseAssets(t, v, "abc123")
			out := filepath.Join(t.TempDir(), "npm")
			tt.mutate(t, assets, out)
			if _, err := WriteNPMPackages(assets, out, v); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("WriteNPMPackages = %v, want error containing %q", err, tt.want)
			}
		})
	}
}

// fakeRegistry is an NPMRunner recording publishes and answering npm view
// from a map of name@version to the published package's release tag.
type fakeRegistry struct {
	npmVersion string            // "" = 11.6.0
	published  map[string]string // spec → tag ("" = no tag recorded)
	viewErr    error
	failOn     string // directory base name whose publish fails
	calls      []string
}

func (r *fakeRegistry) run(_ context.Context, args ...string) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(args, " "))
	switch args[0] {
	case "--version":
		if r.npmVersion == "" {
			return []byte("11.6.0\n"), nil
		}
		return []byte(r.npmVersion + "\n"), nil
	case "view":
		if r.viewErr != nil {
			return nil, r.viewErr
		}
		tag, ok := r.published[args[2]]
		switch {
		case !ok && strings.HasSuffix(args[2], "@9.9.9"):
			return []byte(`{"error":{"code":"E404"}}`), errors.New("npm view: exit status 1: npm error code E404")
		case !ok:
			return nil, nil
		case tag == "":
			return []byte(`"` + strings.SplitN(args[2][1:], "@", 2)[1] + `"`), nil
		}
		return json.Marshal(map[string]any{"version": "x", "rhizome": map[string]string{"tag": tag}})
	case "publish":
		if r.failOn != "" && filepath.Base(args[1]) == r.failOn {
			return nil, errors.New("npm publish: exit status 1: E403")
		}
		m, err := readNPMManifest(args[1])
		if err != nil {
			return nil, err
		}
		r.published[m.Name+"@"+m.Version] = m.Rhizome.Tag
		return nil, nil
	}
	return nil, errors.New("unexpected npm " + args[0])
}

func (r *fakeRegistry) publishes() []string {
	var out []string
	for _, c := range r.calls {
		if strings.HasPrefix(c, "publish ") {
			f := strings.Fields(c)
			out = append(out, filepath.Base(f[1])+" "+strings.Join(f[2:], " "))
		}
	}
	return out
}

func TestPublishNPM(t *testing.T) {
	order := []string{"rhizome-darwin", "rhizome-linux-x64", "rhizome-linux-arm64", "rhizome-win32-x64", "rhizome-win32-arm64", "rhizome"}
	withTag := func(dist string, names ...string) []string {
		var out []string
		for _, n := range names {
			out = append(out, n+" --tag "+dist)
		}
		return out
	}
	tests := []struct {
		name      string
		tag       string
		published map[string]string
		viewErr   error
		failOn    string
		want      []string
		wantErr   string
	}{
		{name: "stable", tag: "v1.2.3", want: withTag("latest", order...)},
		{name: "build metadata", tag: "v1.2.3+build.5", want: withTag("latest", order...)},
		{name: "prerelease never moves latest", tag: "v1.3.0-rc.1", want: withTag("next", order...)},
		{name: "unknown package is a 404", tag: "v9.9.9", want: withTag("latest", order...)},
		{
			name: "rerun skips packages published by an earlier attempt", tag: "v1.2.3",
			published: map[string]string{"@aliengiraffe/rhizome-darwin@1.2.3": "v1.2.3", "@aliengiraffe/rhizome-linux-x64@1.2.3": "v1.2.3"},
			want:      withTag("latest", order[2:]...),
		},
		{
			name: "everything already published", tag: "v1.2.3",
			published: map[string]string{
				"@aliengiraffe/rhizome-darwin@1.2.3": "v1.2.3", "@aliengiraffe/rhizome-linux-x64@1.2.3": "v1.2.3",
				"@aliengiraffe/rhizome-linux-arm64@1.2.3": "v1.2.3", "@aliengiraffe/rhizome-win32-x64@1.2.3": "v1.2.3",
				"@aliengiraffe/rhizome-win32-arm64@1.2.3": "v1.2.3", "@aliengiraffe/rhizome@1.2.3": "v1.2.3",
			},
		},
		{
			name: "same npm version from another tag", tag: "v1.2.3+build.5",
			published: map[string]string{"@aliengiraffe/rhizome-darwin@1.2.3": "v1.2.3"},
			wantErr:   `already published from tag "v1.2.3"`,
		},
		{
			name: "published version without a recorded tag", tag: "v1.2.3",
			published: map[string]string{"@aliengiraffe/rhizome-linux-arm64@1.2.3": ""},
			want:      withTag("latest", order[:2]...),
			wantErr:   "already published from tag \"\"",
		},
		{name: "registry error", tag: "v1.2.3", viewErr: errors.New("npm view: ETIMEDOUT"), wantErr: "ETIMEDOUT"},
		{
			name: "launcher is never published after a failed platform package", tag: "v1.2.3", failOn: "rhizome-linux-arm64",
			want: withTag("latest", order[:3]...), wantErr: "E403",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := ParseTag(tt.tag)
			if err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "npm")
			if _, err := WriteNPMPackages(writeReleaseAssets(t, v, "abc123"), out, v); err != nil {
				t.Fatal(err)
			}
			if tt.published == nil {
				tt.published = map[string]string{}
			}
			reg := &fakeRegistry{published: tt.published, viewErr: tt.viewErr, failOn: tt.failOn}
			var log bytes.Buffer
			err = PublishNPM(context.Background(), out, v, reg.run, &log)
			if tt.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("PublishNPM = %v, want error containing %q", err, tt.wantErr)
			}
			if got := reg.publishes(); !slices.Equal(got, tt.want) {
				t.Fatalf("publishes %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPublishNPMRequiresTrustedPublishingNPM(t *testing.T) {
	v, _ := ParseTag("v1.2.3")
	out := filepath.Join(t.TempDir(), "npm")
	if _, err := WriteNPMPackages(writeReleaseAssets(t, v, "abc123"), out, v); err != nil {
		t.Fatal(err)
	}
	for npm, ok := range map[string]bool{"11.5.1": true, "11.10.0": true, "12.0.0": true, "11.5.0": false, "10.9.2": false, "11.4.9": false, "garbage": false} {
		reg := &fakeRegistry{npmVersion: npm, published: map[string]string{}}
		err := PublishNPM(context.Background(), out, v, reg.run, &bytes.Buffer{})
		if ok != (err == nil) || (!ok && len(reg.publishes()) != 0) {
			t.Errorf("npm %s: PublishNPM = %v, publishes %v", npm, err, reg.publishes())
		}
	}
}

func TestPublishNPMRefuses(t *testing.T) {
	snap, err := Snapshot("0123456789ab")
	if err != nil {
		t.Fatal(err)
	}
	reg := &fakeRegistry{published: map[string]string{}}
	if err := PublishNPM(context.Background(), t.TempDir(), snap, reg.run, &bytes.Buffer{}); err == nil || len(reg.calls) != 0 {
		t.Fatalf("PublishNPM published a snapshot: %v %v", err, reg.calls)
	}

	// Packages generated for another tag are never published under this one.
	v1, _ := ParseTag("v1.2.3")
	v2, _ := ParseTag("v1.2.3+build.5")
	out := filepath.Join(t.TempDir(), "npm")
	if _, err := WriteNPMPackages(writeReleaseAssets(t, v1, "abc123"), out, v1); err != nil {
		t.Fatal(err)
	}
	if err := PublishNPM(context.Background(), out, v2, reg.run, &bytes.Buffer{}); err == nil || len(reg.publishes()) != 0 {
		t.Fatalf("PublishNPM published packages of another tag: %v %v", err, reg.calls)
	}
}

// TestNPMSmoke packs and installs real tarballs with the npm on PATH; the
// release workflow runs the same check against real binaries.
func TestNPMSmoke(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("smoke fixtures are shell scripts")
	}
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not available")
	}
	t.Setenv("npm_config_cache", t.TempDir())
	t.Setenv("npm_config_userconfig", filepath.Join(t.TempDir(), "npmrc"))
	v, err := Snapshot("0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	goarch := runtime.GOARCH
	if runtime.GOOS == "darwin" {
		goarch = "all"
	}
	assets := writeReleaseAssets(t, v, "abc123")
	line, err := NPMSmoke(context.Background(), assets, v, "abc123", runtime.GOOS, goarch, ExecNPM)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "rhizome 0.0.0-snapshot+0123456789ab (commit abc123,") {
		t.Fatalf("NPMSmoke = %q", line)
	}
	if _, err := NPMSmoke(context.Background(), assets, v, "def456", runtime.GOOS, goarch, ExecNPM); err == nil || !strings.Contains(err.Error(), "want prefix") {
		t.Fatalf("NPMSmoke with the wrong commit = %v", err)
	}
}
