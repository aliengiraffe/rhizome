package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"rhizome/internal/channel"
	"rhizome/internal/localstate"
)

func TestRestart(t *testing.T) {
	for _, platform := range []string{"darwin", "linux", "windows"} {
		for _, scenario := range []string{"running", "stopped", "missing", "stop failure", "stale lock", "start failure", "start timeout", "cancel shutdown", "cancel startup", "delayed shutdown", "runtime mismatch"} {
			t.Run(platform+"/"+scenario, func(t *testing.T) {
				dir := t.TempDir()
				c := Config{Observer: channel.ObserverOptions{Runtime: filepath.Join(dir, "runtime")}}
				if err := localstate.SecureDir(c.Observer.Runtime); err != nil {
					t.Fatal(err)
				}
				m := manager{platform: platform, definition: filepath.Join(dir, "definition"), configPath: filepath.Join(dir, "config.toml"), binary: "/bin/rhizome", waitTimeout: 250 * time.Millisecond}
				definition := m.render(c)
				if scenario != "missing" {
					if err := m.writeDefinition(definition); err != nil {
						t.Fatal(err)
					}
				}
				var lock *os.File
				acquire := func() {
					t.Helper()
					var err error
					lock, err = localstate.Lock(filepath.Join(c.Observer.Runtime, "observer.lock"), false)
					if err != nil {
						t.Fatal(err)
					}
				}
				release := func() {
					if lock != nil {
						lock.Close()
						lock = nil
					}
				}
				defer release()
				if scenario != "stopped" && scenario != "missing" {
					acquire()
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				loaded := scenario != "stopped"
				var commands []string
				m.probe = func(context.Context, string, ...string) error {
					if loaded {
						return nil
					}
					return errors.New("unloaded")
				}
				var stopped chan struct{}
				m.run = func(_ context.Context, name string, args ...string) error {
					command := name + " " + strings.Join(args, " ")
					commands = append(commands, command)
					stopping := strings.Contains(command, "bootout") || strings.Contains(command, "--user stop") || strings.Contains(command, "/End")
					if stopping {
						if scenario == "stop failure" {
							return errors.New("stop rejected")
						}
						loaded = false
						if scenario == "cancel shutdown" {
							cancel()
							return nil
						}
						if scenario == "stale lock" {
							return nil
						}
						if scenario == "delayed shutdown" {
							// Keep the old lock until the wait has begun; a start before release fails.
							stopped = make(chan struct{})
							old := lock
							lock = nil
							go func() {
								defer close(stopped)
								timer := time.NewTimer(50 * time.Millisecond)
								defer timer.Stop()
								select {
								case <-timer.C:
								case <-ctx.Done():
								}
								old.Close()
							}()
							return nil
						}
						release()
						return nil
					}
					if running(c.Observer.Runtime) {
						t.Error("start raced predecessor lock")
					}
					if scenario == "start failure" {
						return errors.New("start rejected")
					}
					if scenario == "start timeout" {
						return nil
					}
					if scenario == "cancel startup" {
						cancel()
						return nil
					}
					acquire()
					return nil
				}
				if scenario == "runtime mismatch" {
					c.Observer.Runtime = filepath.Join(dir, "other")
				}
				err := m.manage(ctx, "restart", c)
				if stopped != nil {
					<-stopped
				}
				wantError := ""
				switch scenario {
				case "missing":
					wantError = "not installed"
				case "stop failure", "stale lock", "cancel shutdown":
					wantError = "restart shutdown"
				case "start failure", "start timeout", "cancel startup":
					wantError = "restart startup"
				case "runtime mismatch":
					wantError = "runtime differs"
				}
				if wantError == "" {
					if err != nil {
						t.Fatal(err)
					}
				} else if err == nil || !strings.Contains(err.Error(), wantError) {
					t.Fatalf("want %q, got %v", wantError, err)
				}
				if strings.HasPrefix(scenario, "cancel") && !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
				if scenario == "stale lock" || scenario == "start timeout" {
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("timeout lost: %v", err)
					}
				}
				var want []string
				if scenario != "missing" && scenario != "runtime mismatch" {
					if scenario != "stopped" {
						switch platform {
						case "darwin":
							want = append(want, fmt.Sprintf("launchctl bootout gui/%d/com.rhizome.observer", os.Getuid()))
						case "linux":
							want = append(want, "systemctl --user stop rhizome-observer.service")
						case "windows":
							want = append(want, "schtasks /End /TN "+taskName())
						}
					}
					if scenario != "stop failure" && scenario != "stale lock" && scenario != "cancel shutdown" {
						switch platform {
						case "darwin":
							want = append(want, fmt.Sprintf("launchctl bootstrap gui/%d %s", os.Getuid(), m.definition))
						case "linux":
							want = append(want, "systemctl --user start rhizome-observer.service")
						case "windows":
							want = append(want, "schtasks /Run /TN "+taskName())
						}
					}
				}
				if !reflect.DeepEqual(commands, want) {
					t.Fatalf("commands %v; want %v", commands, want)
				}
				if scenario == "missing" {
					if _, err := os.Stat(m.definition); !os.IsNotExist(err) {
						t.Fatal("definition created")
					}
				} else if got, err := m.readDefinition(); err != nil || got != definition {
					t.Fatal("definition changed", err)
				}
			})
		}
	}
}

func TestRestartProbeCancellation(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			dir := t.TempDir()
			c := Config{Observer: channel.ObserverOptions{Runtime: dir}}
			m := manager{platform: "darwin", definition: filepath.Join(dir, "definition")}
			if err := m.writeDefinition(m.render(c)); err != nil {
				t.Fatal(err)
			}
			m.probe = func(context.Context, string, ...string) error { return cause }
			m.run = func(context.Context, string, ...string) error {
				t.Fatal("native command after failed shutdown probe")
				return nil
			}
			if err := m.manage(context.Background(), "restart", c); !errors.Is(err, cause) || !strings.Contains(err.Error(), "restart shutdown") {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestStartLaunchdRecovery(t *testing.T) {
	for _, loaded := range []bool{true, false} {
		t.Run(fmt.Sprint(loaded), func(t *testing.T) {
			dir := t.TempDir()
			c := Config{Observer: channel.ObserverOptions{Runtime: dir}}
			m := manager{platform: "darwin", definition: filepath.Join(dir, "definition")}
			m.probe = func(context.Context, string, ...string) error {
				if loaded {
					return nil
				}
				return errors.New("unloaded")
			}
			var lock *os.File
			defer func() {
				if lock != nil {
					lock.Close()
				}
			}()
			m.run = func(_ context.Context, name string, args ...string) error {
				want := "bootstrap"
				if loaded {
					want = "kickstart"
				}
				if name != "launchctl" || args[0] != want {
					t.Fatalf("%s %v", name, args)
				}
				var err error
				lock, err = localstate.Lock(filepath.Join(dir, "observer.lock"), false)
				return err
			}
			if err := m.manage(context.Background(), "start", c); err != nil {
				t.Fatal(err)
			}
		})
	}
}
