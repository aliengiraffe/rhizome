package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceRestartCLI(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("AppData", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	// Missing definition proves dispatch reached the lifecycle without opening a UI.
	err := serviceCommand([]string{"restart", "--config", path})
	if err == nil || !strings.Contains(err.Error(), "service is not installed") {
		t.Fatalf("got %v", err)
	}
	for _, args := range [][]string{{"--help"}, {"-h"}} {
		old := os.Stdout
		f, err := os.CreateTemp(t.TempDir(), "help")
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout = f
		err = serviceCommand(args)
		os.Stdout = old
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "|restart|") {
			t.Fatalf("missing restart: %s", b)
		}
	}
}
