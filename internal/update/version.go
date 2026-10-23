// Package update finds, verifies and installs newer Rhizome releases. Every
// step fails closed: nothing touches the installed binary until the release
// manifest signature, the archive digest, the platform code signature and
// the candidate's own --version output have all been checked.
package update

import (
	"strconv"
	"strings"

	"rhizome/internal/release"
)

// Compare orders two versions by SemVer 2.0.0 precedence. Build metadata is
// ignored, as the specification requires.
func Compare(a, b release.Version) int {
	for _, pair := range [][2]uint64{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if pair[0] != pair[1] {
			if pair[0] < pair[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case a.Prerelease == b.Prerelease:
		return 0
	case a.Prerelease == "":
		return 1
	case b.Prerelease == "":
		return -1
	}
	as, bs := strings.Split(a.Prerelease, "."), strings.Split(b.Prerelease, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if c := compareIdentifier(as[i], bs[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(as) < len(bs):
		return -1
	case len(as) > len(bs):
		return 1
	}
	return 0
}

func compareIdentifier(a, b string) int {
	an, aerr := strconv.ParseUint(a, 10, 64)
	bn, berr := strconv.ParseUint(b, 10, 64)
	switch {
	case aerr == nil && berr == nil:
		switch {
		case an < bn:
			return -1
		case an > bn:
			return 1
		}
		return 0
	case aerr == nil:
		return -1 // numeric identifiers precede alphanumeric ones
	case berr == nil:
		return 1
	}
	return strings.Compare(a, b)
}

// ParseRunning parses the version a binary reports. Local builds ("dev", a
// snapshot or anything that is not SemVer) are never updated.
func ParseRunning(version string) (release.Version, bool) {
	if version == "" || version == "dev" {
		return release.Version{}, false
	}
	v, err := release.ParseTag(version)
	if err != nil || strings.HasPrefix(v.Prerelease, "snapshot") {
		return release.Version{}, false
	}
	return v, true
}

// Eligible reports whether candidate may replace current: it must be strictly
// newer, and a prerelease is only offered to a build that is itself a
// prerelease. The reason is a stable code for logs and status.
func Eligible(current, candidate release.Version) (bool, string) {
	if candidate.IsPrerelease() && !current.IsPrerelease() {
		return false, "prerelease_skipped"
	}
	if Compare(candidate, current) <= 0 {
		return false, "not_newer"
	}
	return true, "newer"
}
