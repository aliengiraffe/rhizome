package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOAIHiddenDispatchAndReadOnlyStatus(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("RHIZOME_CONFIG", filepath.Join(home, "missing-config"))
	for _, args := range [][]string{{"rhizome"}, {"rhizome", "--help"}} {
		out, e := runEntry(t, args...)
		if e != nil {
			out += e.Error()
		}
		for _, hidden := range []string{" auth", " test", "oai"} {
			if strings.Contains(out, hidden) {
				t.Fatalf("hidden command exposed: %s", out)
			}
		}
	}
	out, e := runEntry(t, "rhizome", "auth", "oai", "status")
	if e != nil || !strings.Contains(out, "Not signed in") {
		t.Fatalf("status %q %v", out, e)
	}
	files, e := os.ReadDir(home)
	if e != nil || len(files) != 0 {
		t.Fatalf("status created state: %v %v", files, e)
	}
	for _, args := range [][]string{{"rhizome", "auth"}, {"rhizome", "auth", "other"}, {"rhizome", "auth", "oai", "unknown"}, {"rhizome", "test", "oai", "extra"}} {
		if _, e := runEntry(t, args...); e == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if _, e := runEntry(t, "rhizome", "test", "oai", "--model", "synthetic", "--prompt", "hello"); e == nil || !strings.Contains(e.Error(), "not signed in") {
		t.Fatalf("test without auth: %v", e)
	}
}
