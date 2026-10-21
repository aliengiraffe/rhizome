package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"rhizome/internal/localstate"
	"rhizome/internal/service"
	"runtime"
	"strings"
	"testing"
)

// serviceFixture is an isolated HOME with a configuration file whose runtime
// directory is private to the test. Nothing here touches a real service manager.
type serviceFixture struct {
	path, runtime, definition string
}

func newServiceFixture(t *testing.T) serviceFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("RHIZOME_CONFIG", "")
	t.Setenv("RHIZOME_RUNTIME_DIR", "")
	f := serviceFixture{path: filepath.Join(home, "custom", "config.toml"), runtime: filepath.Join(home, "run")}
	switch runtime.GOOS {
	case "darwin":
		f.definition = filepath.Join(home, "Library", "LaunchAgents", "com.rhizome.observer.plist")
	case "linux":
		f.definition = filepath.Join(home, "config", "systemd", "user", "rhizome-observer.service")
	default:
		t.Skip("native definition path is only modelled for darwin and linux")
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path, []byte(fmt.Sprintf("runtime_dir = %q\n", f.runtime)), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// hold simulates a running observer by taking its runtime lock.
func (f serviceFixture) hold(t *testing.T) {
	t.Helper()
	if err := localstate.SecureDir(f.runtime); err != nil {
		t.Fatal(err)
	}
	lock, err := localstate.Lock(filepath.Join(f.runtime, "observer.lock"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
}

func (f serviceFixture) writeDefinition(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.definition), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.definition, []byte("definition"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestInstallService(t *testing.T) {
	startup := errors.New("service manager accepted startup but observer did not acquire its runtime lock")
	cases := []struct {
		name    string
		prepare func(t *testing.T, f serviceFixture) // state before the step
		manage  func(t *testing.T, f serviceFixture) error
		cancel  bool
		want    serviceOutcome
		wantErr error  // wrapped cause, when any
		wantMsg string // substring of the error
	}{
		{"missing service installs and starts", nil, func(t *testing.T, f serviceFixture) error { f.writeDefinition(t); f.hold(t); return nil }, false, serviceOutcome{Installed: true, Running: true}, nil, ""},
		{"running service is success without restart", func(t *testing.T, f serviceFixture) { f.writeDefinition(t); f.hold(t) }, func(*testing.T, serviceFixture) error { return nil }, false, serviceOutcome{Installed: true, Running: true}, nil, ""},
		{"install error leaves nothing installed", nil, func(*testing.T, serviceFixture) error { return errors.New("service already configured differently") }, false, serviceOutcome{}, nil, "service not installed: service already configured differently"},
		{"startup timeout keeps the definition", nil, func(t *testing.T, f serviceFixture) error { f.writeDefinition(t); return startup }, false, serviceOutcome{Installed: true}, startup, "definition installed but the observer is not running"},
		{"success claim without a running observer", nil, func(t *testing.T, f serviceFixture) error { f.writeDefinition(t); return nil }, false, serviceOutcome{Installed: true}, nil, "observer is not running"},
		{"cancellation", nil, func(*testing.T, serviceFixture) error { return context.Canceled }, true, serviceOutcome{}, context.Canceled, "service not installed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newServiceFixture(t)
			if tc.prepare != nil {
				tc.prepare(t, f)
			}
			var actions []string
			manage := func(ctx context.Context, action string, c service.Config) error {
				actions = append(actions, action)
				if c.ConfigPath != f.path || c.Observer.Runtime != f.runtime {
					t.Errorf("service config %+v", c)
				}
				if tc.cancel {
					if ctx.Err() == nil {
						t.Error("cancellation not propagated")
					}
					return ctx.Err()
				}
				return tc.manage(t, f)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			got, err := installService(ctx, f.path, manage)
			if len(actions) != 1 || actions[0] != "install" {
				t.Fatalf("actions %v", actions)
			}
			if got.Runtime != f.runtime || got.Installed != tc.want.Installed || got.Running != tc.want.Running {
				t.Fatalf("outcome %+v", got)
			}
			if tc.wantMsg == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantMsg != "" && (err == nil || !strings.Contains(err.Error(), tc.wantMsg)) {
				t.Fatalf("got %v, want %q", err, tc.wantMsg)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("cause not wrapped: %v", err)
			}
			if _, statErr := os.Stat(f.definition); tc.want.Installed && statErr != nil {
				t.Fatal("definition removed after failure", statErr)
			}
			if b, _ := os.ReadFile(f.path); !strings.Contains(string(b), f.runtime) {
				t.Fatalf("configuration changed: %s", b)
			}
		})
	}
}

func TestInstallServiceRejectsInvalidConfiguration(t *testing.T) {
	f := newServiceFixture(t)
	if err := os.WriteFile(f.path, []byte("runtime_dir = relative\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err := installService(context.Background(), f.path, func(context.Context, string, service.Config) error { called = true; return nil })
	if err == nil || called {
		t.Fatalf("invalid configuration reached the service manager: %v %v", err, called)
	}
	_, err = installService(context.Background(), filepath.Join(filepath.Dir(f.path), "missing.toml"), func(context.Context, string, service.Config) error { called = true; return nil })
	if err == nil || called {
		t.Fatalf("missing configuration reached the service manager: %v %v", err, called)
	}
}
