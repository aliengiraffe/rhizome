package release

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// NPMScope is the npm organization every Rhizome package is published under.
const NPMScope = "@aliengiraffe"

// NPMRepository is the exact repository URL npm trusted publishing compares
// with the publishing workflow's repository.
const NPMRepository = "git+https://github.com/aliengiraffe/rhizome.git"

const (
	npmHomepage = "https://github.com/aliengiraffe/rhizome#readme"
	npmLicense  = "MIT"
	npmReleases = "https://github.com/aliengiraffe/rhizome/releases"
	// maxPackageJSON bounds package.json files read back for publishing.
	maxPackageJSON = 64 << 10
)

// minNPM is the oldest npm that supports trusted publishing (OIDC).
var minNPM = [3]uint64{11, 5, 1}

//go:embed npm/rhizome.js
var npmLauncher []byte

// npmPlatform is one platform package and the archive its binary comes from.
type npmPlatform struct {
	Suffix      string   // package name after "rhizome-"
	GOOS        string   // archive operating system
	GOARCH      string   // archive architecture; darwin ships "all"
	OS, CPU     []string // npm os and cpu fields
	Description string
}

// npmPlatforms lists the platform packages in publish order. Keep in step with
// PACKAGES in npm/rhizome.js.
var npmPlatforms = []npmPlatform{
	{"darwin", "darwin", "all", []string{"darwin"}, []string{"x64", "arm64"}, "macOS universal (x64 and arm64)"},
	{"linux-x64", "linux", "amd64", []string{"linux"}, []string{"x64"}, "Linux x64"},
	{"linux-arm64", "linux", "arm64", []string{"linux"}, []string{"arm64"}, "Linux arm64"},
	{"win32-x64", "windows", "amd64", []string{"win32"}, []string{"x64"}, "Windows x64"},
	{"win32-arm64", "windows", "arm64", []string{"win32"}, []string{"arm64"}, "Windows arm64"},
}

func (p npmPlatform) name() string { return NPMScope + "/rhizome-" + p.Suffix }
func (p npmPlatform) dir() string  { return "rhizome-" + p.Suffix }

// npmMainName and npmMainDir name the launcher package.
const (
	npmMainName = NPMScope + "/rhizome"
	npmMainDir  = "rhizome"
)

// NPMVersion is the npm version for v: the tag without "v" and without SemVer
// build metadata, because npm treats versions differing only in build
// metadata as the same version. Snapshots become 0.0.0-snapshot-<commit> so
// every snapshot stays distinct.
func (v Version) NPMVersion() string {
	core, build, _ := strings.Cut(v.Version, "+")
	if v.Tag == "" && build != "" {
		return core + "-" + build
	}
	return core
}

// NPMDistTag is the dist-tag a publish of v moves: prereleases go to "next"
// and never to "latest".
func (v Version) NPMDistTag() string {
	if v.IsPrerelease() {
		return "next"
	}
	return "latest"
}

// npmManifest is the package.json written for every package. Field order is
// fixed by the struct, and there is deliberately no scripts field: npm runs no
// install lifecycle script for any Rhizome package.
type npmManifest struct {
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	Description          string            `json:"description"`
	License              string            `json:"license"`
	Homepage             string            `json:"homepage"`
	Repository           npmRepository     `json:"repository"`
	OS                   []string          `json:"os,omitempty"`
	CPU                  []string          `json:"cpu,omitempty"`
	Bin                  map[string]string `json:"bin,omitempty"`
	Engines              map[string]string `json:"engines,omitempty"`
	OptionalDependencies map[string]string `json:"optionalDependencies,omitempty"`
	PublishConfig        npmPublishConfig  `json:"publishConfig"`
	Rhizome              npmSource         `json:"rhizome"`
}

type npmRepository struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

type npmPublishConfig struct {
	Access string `json:"access"`
}

// npmSource records the release tag a package was generated from, so a
// rerun can tell its own earlier publish from a different tag that maps to
// the same npm version.
type npmSource struct {
	Tag string `json:"tag"`
}

func newNPMManifest(name, description string, v Version) npmManifest {
	return npmManifest{
		Name:          name,
		Version:       v.NPMVersion(),
		Description:   description,
		License:       npmLicense,
		Homepage:      npmHomepage,
		Repository:    npmRepository{Type: "git", URL: NPMRepository},
		PublishConfig: npmPublishConfig{Access: "public"},
		Rhizome:       npmSource{Tag: v.Tag},
	}
}

// WriteNPMPackages verifies the checksum manifest in dir, then writes one
// package directory per platform plus the launcher package under out, ready
// for npm pack or npm publish. It returns the directories in publish order:
// platform packages first, the launcher package last.
func WriteNPMPackages(dir, out string, v Version) ([]string, error) {
	if err := VerifyManifest(dir, v.AssetVersion(), false); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, err
	}
	var dirs []string
	optional := map[string]string{}
	for _, p := range npmPlatforms {
		pkgDir := filepath.Join(out, p.dir())
		archive := filepath.Join(dir, ArchiveName(v.AssetVersion(), p.GOOS, p.GOARCH))
		if err := writePlatformPackage(pkgDir, archive, p, v); err != nil {
			return nil, fmt.Errorf("%s: %w", p.name(), err)
		}
		optional[p.name()] = v.NPMVersion()
		dirs = append(dirs, pkgDir)
	}
	mainDir := filepath.Join(out, npmMainDir)
	m := newNPMManifest(npmMainName, "Rhizome connects local coding agent sessions (Claude Code, Codex, OpenCode) over MCP.", v)
	m.Bin = map[string]string{"rhizome": "bin/rhizome.js"}
	m.Engines = map[string]string{"node": ">=18"}
	m.OptionalDependencies = optional
	readme := fmt.Sprintf(`# %s

Rhizome connects local coding agent sessions (Claude Code, Codex and OpenCode)
over MCP. This package installs the prebuilt `+"`rhizome`"+` binary for your
platform through an optional dependency; it runs no install scripts.

`+"```sh"+`
npm i -g %s
rhizome setup
`+"```"+`

Or run it once with `+"`npx %s --version`"+`.

If the platform package was skipped (for example with `+"`--omit=optional`"+`)
or your platform has no prebuilt binary, download a build from
%s. Windows binaries are unsigned, so SmartScreen may warn.

Documentation: %s
`, npmMainName, npmMainName, npmMainName, npmReleases, npmHomepage)
	license := filepath.Join(out, npmPlatforms[0].dir(), "LICENSE")
	if err := writePackage(mainDir, m, readme, license); err != nil {
		return nil, fmt.Errorf("%s: %w", npmMainName, err)
	}
	if err := os.MkdirAll(filepath.Join(mainDir, "bin"), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(mainDir, "bin", "rhizome.js"), npmLauncher, 0o755); err != nil {
		return nil, err
	}
	return append(dirs, mainDir), nil
}

func writePlatformPackage(pkgDir, archive string, p npmPlatform, v Version) error {
	m := newNPMManifest(p.name(), "The "+p.Description+" binary for "+npmMainName+".", v)
	m.OS, m.CPU = p.OS, p.CPU
	readme := fmt.Sprintf(`# %s

The prebuilt %s `+"`rhizome`"+` binary. Do not install this package directly;
install %s, which selects the right platform package:

`+"```sh"+`
npm i -g %s
`+"```"+`

Documentation: %s
`, p.name(), p.Description, npmMainName, npmMainName, npmHomepage)
	if strings.HasPrefix(p.Suffix, "win32") {
		readme += "\nWindows binaries are unsigned, so SmartScreen may warn.\n"
	}
	if err := writePackage(pkgDir, m, readme, ""); err != nil {
		return err
	}
	if _, err := extractMember(archive, "LICENSE", pkgDir, 0o644); err != nil {
		return err
	}
	binDir := filepath.Join(pkgDir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		return err
	}
	bin, err := ExtractBinary(archive, p.GOOS, binDir)
	if err != nil {
		return err
	}
	// The umask may have narrowed the mode; npm pack keeps the execute bits.
	return os.Chmod(bin, 0o755)
}

// writePackage creates pkgDir, which must not exist yet, with package.json,
// README.md and, when license is set, a copy of that LICENSE file.
func writePackage(pkgDir string, m npmManifest, readme, license string) error {
	if err := os.Mkdir(pkgDir, 0o755); err != nil {
		return err
	}
	var data bytes.Buffer
	enc := json.NewEncoder(&data)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), data.Bytes(), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "README.md"), []byte(readme), 0o644); err != nil {
		return err
	}
	if license == "" {
		return nil
	}
	text, err := readBounded(license, maxManifest)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(pkgDir, "LICENSE"), text, 0o644)
}

// NPMRunner runs npm with args and returns its standard output. Errors carry
// npm's standard error text.
type NPMRunner func(ctx context.Context, args ...string) ([]byte, error)

// ExecNPM runs the npm on PATH.
func ExecNPM(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "npm", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("npm %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// PublishNPM publishes the packages WriteNPMPackages wrote under out, platform
// packages first and the launcher last, under v's dist-tag. A package whose
// version is already published from the same tag is skipped, so a run that
// failed halfway can be repeated; one published from a different tag (two
// tags mapping to one npm version) fails.
func PublishNPM(ctx context.Context, out string, v Version, run NPMRunner, log io.Writer) error {
	if v.Tag == "" {
		return errors.New("snapshots are never published to npm")
	}
	if err := requireNPM(ctx, run); err != nil {
		return err
	}
	dirs := make([]string, 0, len(npmPlatforms)+1)
	for _, p := range npmPlatforms {
		dirs = append(dirs, filepath.Join(out, p.dir()))
	}
	dirs = append(dirs, filepath.Join(out, npmMainDir))
	for _, dir := range dirs {
		m, err := readNPMManifest(dir)
		if err != nil {
			return err
		}
		if m.Version != v.NPMVersion() || m.Rhizome.Tag != v.Tag {
			return fmt.Errorf("%s: package is %s from tag %q, want %s from %q", dir, m.Version, m.Rhizome.Tag, v.NPMVersion(), v.Tag)
		}
		spec := m.Name + "@" + m.Version
		tag, published, err := publishedTag(ctx, run, spec)
		if err != nil {
			return err
		}
		if published {
			if tag != v.Tag {
				return fmt.Errorf("%s is already published from tag %q, not %q; npm versions ignore build metadata, so cut a new version", spec, tag, v.Tag)
			}
			fmt.Fprintf(log, "%s already published from %s; skipping\n", spec, v.Tag)
			continue
		}
		if _, err := run(ctx, "publish", dir, "--tag", v.NPMDistTag()); err != nil {
			return err
		}
		fmt.Fprintf(log, "published %s with dist-tag %s\n", spec, v.NPMDistTag())
	}
	return nil
}

// requireNPM fails unless npm is at least minNPM.
func requireNPM(ctx context.Context, run NPMRunner) error {
	out, err := run(ctx, "--version")
	if err != nil {
		return err
	}
	got := strings.TrimSpace(string(out))
	v, err := ParseTag(got)
	if err != nil {
		return fmt.Errorf("npm --version printed %q: %w", got, err)
	}
	if slices.Compare([]uint64{v.Major, v.Minor, v.Patch}, minNPM[:]) >= 0 {
		return nil
	}
	return fmt.Errorf("npm %s is older than %d.%d.%d, which trusted publishing requires", got, minNPM[0], minNPM[1], minNPM[2])
}

func readNPMManifest(dir string) (npmManifest, error) {
	var m npmManifest
	data, err := readBounded(filepath.Join(dir, "package.json"), maxPackageJSON)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("%s/package.json: %w", dir, err)
	}
	return m, nil
}

// publishedTag reports whether spec (name@version) exists on the registry
// and, if so, the release tag recorded in its package.json.
func publishedTag(ctx context.Context, run NPMRunner, spec string) (string, bool, error) {
	out, err := run(ctx, "view", "--json", spec, "version", "rhizome")
	if err != nil {
		// An unknown package is E404; an unknown version of a known package
		// succeeds with empty output.
		if strings.Contains(err.Error(), "E404") || bytes.Contains(out, []byte("E404")) {
			return "", false, nil
		}
		return "", false, err
	}
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return "", false, nil
	}
	var view struct {
		Version string    `json:"version"`
		Rhizome npmSource `json:"rhizome"`
	}
	if err := json.Unmarshal(out, &view); err != nil {
		// With only one requested field present npm prints its bare value:
		// a published version that records no release tag.
		var version string
		if json.Unmarshal(out, &version) == nil {
			return "", true, nil
		}
		return "", false, fmt.Errorf("npm view %s: unexpected output %q", spec, out)
	}
	return view.Rhizome.Tag, true, nil
}

// NPMSmoke generates the npm packages from the release assets in dir, packs
// every one, installs the launcher and the goos/goarch platform package into
// a temporary global prefix without contacting the registry, and runs the
// installed rhizome --version. goarch is the archive architecture ("all" for
// darwin).
func NPMSmoke(ctx context.Context, dir string, v Version, commit, goos, goarch string, run NPMRunner) (string, error) {
	tmp, err := os.MkdirTemp("", "rhizome-npm-smoke-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	dirs, err := WriteNPMPackages(dir, filepath.Join(tmp, "packages"), v)
	if err != nil {
		return "", err
	}
	native := ""
	for _, p := range npmPlatforms {
		if p.GOOS == goos && p.GOARCH == goarch {
			native = p.dir()
		}
	}
	if native == "" {
		return "", fmt.Errorf("no npm platform package for %s/%s", goos, goarch)
	}
	tarballs := filepath.Join(tmp, "tarballs")
	if err := os.Mkdir(tarballs, 0o755); err != nil {
		return "", err
	}
	var install []string
	for _, d := range dirs {
		out, err := run(ctx, "pack", d, "--pack-destination", tarballs, "--json")
		if err != nil {
			return "", err
		}
		var packed []struct {
			Filename string `json:"filename"`
		}
		if err := json.Unmarshal(out, &packed); err != nil || len(packed) != 1 || packed[0].Filename == "" {
			return "", fmt.Errorf("npm pack %s: unexpected output %q", d, out)
		}
		if base := filepath.Base(d); base == npmMainDir || base == native {
			install = append(install, filepath.Join(tarballs, filepath.Base(packed[0].Filename)))
		}
	}
	prefix := filepath.Join(tmp, "prefix")
	args := append([]string{"install", "--global", "--prefix", prefix, "--offline", "--no-audit", "--no-fund"}, install...)
	if _, err := run(ctx, args...); err != nil {
		return "", err
	}
	bin := filepath.Join(prefix, "bin", "rhizome")
	if goos == "windows" {
		bin = filepath.Join(prefix, "rhizome.cmd")
	}
	return CheckVersion(ctx, bin, v.Version, commit)
}
