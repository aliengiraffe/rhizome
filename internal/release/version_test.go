package release

import (
	"strings"
	"testing"
)

func TestParseTag(t *testing.T) {
	tests := []struct {
		tag, version, pkg, asset string
		prerelease               bool
	}{
		{"v1.2.3", "1.2.3", "1.2.3", "1.2.3", false},
		{"1.2.3", "1.2.3", "1.2.3", "1.2.3", false},
		{"v0.0.1", "0.0.1", "0.0.1", "0.0.1", false},
		{"v1.3.0-rc.1", "1.3.0-rc.1", "1.3.0", "1.3.0-rc.1", true},
		{"v1.3.0-0.alpha-1", "1.3.0-0.alpha-1", "1.3.0", "1.3.0-0.alpha-1", true},
		{"v1.3.0-rc.1+build.5", "1.3.0-rc.1+build.5", "1.3.0", "1.3.0-rc.1.build.5", true},
		// Build metadata alone is not a prerelease.
		{"v2.0.0+20261021", "2.0.0+20261021", "2.0.0", "2.0.0.20261021", false},
		{"v2147483647.0.0", "2147483647.0.0", "2147483647.0.0", "2147483647.0.0", false},
	}
	for _, tt := range tests {
		v, err := ParseTag(tt.tag)
		if err != nil {
			t.Fatalf("ParseTag(%q): %v", tt.tag, err)
		}
		if v.Tag != tt.tag || v.Version != tt.version || v.PackageVersion() != tt.pkg ||
			v.AssetVersion() != tt.asset || v.IsPrerelease() != tt.prerelease {
			t.Fatalf("ParseTag(%q) = %+v pkg=%s asset=%s prerelease=%t", tt.tag, v, v.PackageVersion(), v.AssetVersion(), v.IsPrerelease())
		}
	}
}

func TestParseTagRejects(t *testing.T) {
	for _, tag := range []string{
		"", "v", "1.2", "v1.2", "v1", "1.2.3.4", "V1.2.3", "vv1.2.3", "release-1.2.3",
		"v01.2.3", "v1.02.3", "v1.2.03", "v1.2.3-", "v1.2.3-01", "v1.2.3-rc..1",
		"v1.2.3+", "v1.2.3+build..1", "v1.2.3-rc_1", "v1.2.3 ", " v1.2.3", "v1.2.3\n",
		"v1.2.3-rc.1/../x", "v-1.2.3", "v2147483648.0.0", "v1.99999999999999999999.0",
		"v1.2.3-" + strings.Repeat("a", 130),
	} {
		if v, err := ParseTag(tag); err == nil {
			t.Errorf("ParseTag(%q) accepted %+v", tag, v)
		}
	}
}

func TestSnapshot(t *testing.T) {
	v, err := Snapshot("0123456789abcdef0123")
	if err != nil {
		t.Fatal(err)
	}
	if v.Tag != "" || v.Version != "0.0.0-snapshot+0123456789ab" || !v.IsPrerelease() || v.AssetVersion() != "0.0.0-snapshot.0123456789ab" {
		t.Fatalf("Snapshot = %+v", v)
	}
	for _, bad := range []string{"", "abc", "XYZ1234", "0123456-dirty"} {
		if _, err := Snapshot(bad); err == nil {
			t.Errorf("Snapshot(%q) accepted", bad)
		}
	}
}
