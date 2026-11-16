package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDreamHiddenCLI(t *testing.T) {
	// These tests only use isolated configuration; no credentials or model calls.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("RHIZOME_CONFIG", "")
	t.Setenv("RHIZOME_RUNTIME_DIR", filepath.Join(home, "run"))
	if err := dreamCommand(nil); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatal(err)
	}
	if err := dreamCommand([]string{"show"}); err != nil {
		t.Fatal(err)
	}
	if err := dreamCommand([]string{"log"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "run", "memory")); !os.IsNotExist(err) {
		t.Fatal("read-only commands created memory")
	}
	for _, args := range [][]string{{"--enabled"}, {"show", "extra"}, {"rollback"}, {"rollback", "bad"}, {"--dry-run", "extra"}} {
		if err := dreamCommand(args); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	saved := os.Args
	defer func() { os.Args = saved }()
	os.Args = []string{"rhizome"}
	if err := entry(); err == nil || strings.Contains(err.Error(), "dream") {
		t.Fatal("dream exposed in usage", err)
	}
}
