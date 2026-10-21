// Package release validates release tags and the artifacts a tagged release
// publishes. It backs the release workflow; nothing in the rhizome binary
// imports it.
package release

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// maxTagLen bounds tag input; real tags are far shorter.
const maxTagLen = 128

// maxComponent keeps numeric cores representable by installer version fields.
const maxComponent = 1<<31 - 1

// semver is the SemVer 2.0.0 grammar from semver.org, anchored.
var semver = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)` +
	`(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?` +
	`(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)

// Version is a validated release identity.
type Version struct {
	// Tag is the exact git tag, including any "v" prefix.
	Tag string
	// Version is the full SemVer without the "v" prefix; binaries report it.
	Version string
	// Major, Minor and Patch form the numeric core.
	Major, Minor, Patch uint64
	// Prerelease and Build are the optional SemVer components.
	Prerelease, Build string
}

// ParseTag validates tag as SemVer 2.0.0 with an optional "v" prefix.
func ParseTag(tag string) (Version, error) {
	if tag == "" {
		return Version{}, errors.New("empty tag")
	}
	if len(tag) > maxTagLen {
		return Version{}, fmt.Errorf("tag longer than %d bytes", maxTagLen)
	}
	v := strings.TrimPrefix(tag, "v")
	m := semver.FindStringSubmatch(v)
	if m == nil {
		return Version{}, fmt.Errorf("tag %q is not SemVer 2.0.0 (want [v]MAJOR.MINOR.PATCH[-PRERELEASE][+BUILD])", tag)
	}
	out := Version{Tag: tag, Version: v, Prerelease: m[4], Build: m[5]}
	for i, dst := range []*uint64{&out.Major, &out.Minor, &out.Patch} {
		n, err := strconv.ParseUint(m[i+1], 10, 64)
		if err != nil || n > maxComponent {
			return Version{}, fmt.Errorf("tag %q: version component %s exceeds %d", tag, m[i+1], maxComponent)
		}
		*dst = n
	}
	return out, nil
}

// Snapshot returns the identity of an untagged build of commit.
func Snapshot(commit string) (Version, error) {
	if !regexp.MustCompile(`^[0-9a-f]{7,64}$`).MatchString(commit) {
		return Version{}, fmt.Errorf("snapshot commit %q is not a hex object name", commit)
	}
	if len(commit) > 12 {
		commit = commit[:12]
	}
	v, err := ParseTag("0.0.0-snapshot+" + commit)
	v.Tag = ""
	return v, err
}

// IsPrerelease reports whether the version has a prerelease component. Build
// metadata alone does not make a prerelease.
func (v Version) IsPrerelease() bool { return v.Prerelease != "" }

// PackageVersion is the numeric MAJOR.MINOR.PATCH used for the macOS
// installer receipt, which cannot carry SemVer prerelease or build text.
func (v Version) PackageVersion() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// AssetVersion is the version used in asset file names. GitHub rewrites "+"
// in asset names, so build metadata is joined with "." instead.
func (v Version) AssetVersion() string {
	return strings.ReplaceAll(v.Version, "+", ".")
}
