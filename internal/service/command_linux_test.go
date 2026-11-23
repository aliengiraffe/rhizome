package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSystemdUserEnv(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	shared := t.TempDir()
	if err := os.Chmod(shared, 0755); err != nil {
		t.Fatal(err)
	}
	uid := os.Geteuid()
	for _, tc := range []struct {
		name, dir, runtime, bus string
		uid                     int
		infer                   bool
	}{
		{"missing login environment", dir, "", "", uid, true},
		{"explicit runtime", dir, "/custom/runtime", "", uid, false},
		{"explicit bus", dir, "", "unix:path=/custom/bus", uid, false},
		{"missing directory", filepath.Join(dir, "missing"), "", "", uid, false},
		{"another owner", dir, "", "", uid + 1, false},
		{"shared directory", shared, "", "", uid, false},
		{"symlink", link, "", "", uid, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_RUNTIME_DIR", tc.runtime)
			t.Setenv("DBUS_SESSION_BUS_ADDRESS", tc.bus)
			env := systemdUserEnv(tc.dir, tc.uid)
			if !tc.infer {
				if env != nil {
					t.Fatal("expected the child to inherit the caller's environment")
				}
				return
			}
			cmd := exec.Command("sh", "-c", `printf %s "$XDG_RUNTIME_DIR"`)
			cmd.Env = env
			out, err := cmd.Output()
			if err != nil || string(out) != dir || os.Getenv("XDG_RUNTIME_DIR") != "" {
				t.Fatalf("child runtime = %q, parent runtime = %q, error = %v", out, os.Getenv("XDG_RUNTIME_DIR"), err)
			}
		})
	}
}
