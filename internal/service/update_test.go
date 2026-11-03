package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rhizome/internal/channel"
	"rhizome/internal/localstate"
	"rhizome/internal/update"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	run := filepath.Join(home, "run")
	if err := localstate.SecureDir(run); err != nil {
		t.Fatal(err)
	}
	return Config{ConfigPath: filepath.Join(home, "config.toml"), Observer: channel.ObserverOptions{Runtime: run}}
}

func writeHealth(t *testing.T, c Config, version string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"version": version, "updated_at": time.Now().UTC()})
	if err := os.WriteFile(filepath.Join(c.Observer.Runtime, "observer-health.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func hold(t *testing.T, c Config) *os.File {
	t.Helper()
	lock, err := localstate.Lock(filepath.Join(c.Observer.Runtime, "observer.lock"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	return lock
}

func TestServiceRestarterSkipsWhenNotSupervised(t *testing.T) {
	c := testConfig(t)
	var actions []string
	r := &serviceRestarter{c: c, manage: func(_ context.Context, action string, _ Config) error { actions = append(actions, action); return nil }}
	// Not installed: nothing to restart, the installed binary is ready for the next start.
	if err := r.Restart(context.Background(), "1.1.0"); err != nil || len(actions) != 0 {
		t.Fatal(err, actions)
	}
	m, err := newManager(c.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(m.definition), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.definition, []byte("definition"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Installed but stopped: still nothing to restart.
	if err := r.Restart(context.Background(), "1.1.0"); err != nil || len(actions) != 0 {
		t.Fatal(err, actions)
	}
}

func TestServiceRestarterStopsStartsAndWaitsForVersion(t *testing.T) {
	c := testConfig(t)
	m, _ := newManager(c.ConfigPath)
	os.MkdirAll(filepath.Dir(m.definition), 0o700)
	os.WriteFile(m.definition, []byte("definition"), 0o600)
	lock := hold(t, c)
	writeHealth(t, c, "1.0.0")
	var actions []string
	r := &serviceRestarter{c: c, manage: func(_ context.Context, action string, _ Config) error {
		actions = append(actions, action)
		if action == "stop" {
			lock.Close()
		}
		if action == "start" {
			hold(t, c)
			writeHealth(t, c, "1.1.0")
		}
		return nil
	}}
	if err := r.Restart(context.Background(), "1.1.0"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(actions, ",") != "stop,start" {
		t.Fatalf("actions %v", actions)
	}
	failing := &serviceRestarter{c: c, manage: func(_ context.Context, action string, _ Config) error {
		if action == "start" {
			return errors.New("launchctl: boom")
		}
		return nil
	}}
	if err := failing.Restart(context.Background(), "1.2.0"); err == nil || !strings.Contains(err.Error(), "start: launchctl") {
		t.Fatalf("got %v", err)
	}
}

func TestWaitHealthyRequiresVersion(t *testing.T) {
	c := testConfig(t)
	hold(t, c)
	writeHealth(t, c, "1.0.0")
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	err := waitHealthy(ctx, c, "1.1.0")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stale version accepted or wrong error: %v", err)
	}
	writeHealth(t, c, "1.1.0")
	if err := waitHealthy(context.Background(), c, "1.1.0"); err != nil {
		t.Fatal(err)
	}
}

func TestSelfRestarter(t *testing.T) {
	c := testConfig(t)
	// Without a native definition the process must exit for its supervisor.
	r := &selfRestarter{c: c, spawn: func(context.Context, Config, string, []string) error { t.Fatal("spawned"); return nil }}
	if err := r.Restart(context.Background(), "1.1.0"); !errors.Is(err, update.ErrRestartRequired) {
		t.Fatalf("got %v", err)
	}
	m, _ := newManager(c.ConfigPath)
	os.MkdirAll(filepath.Dir(m.definition), 0o700)
	os.WriteFile(m.definition, []byte("definition"), 0o600)
	// Installed but no pending record: refuse rather than guess.
	if err := r.Restart(context.Background(), "1.1.0"); err == nil || errors.Is(err, update.ErrDeferred) {
		t.Fatalf("got %v", err)
	}
	target := filepath.Join(t.TempDir(), "rhizome")
	state, _ := json.Marshal(update.State{Pending: &update.Pending{From: "1.0.0", To: "1.1.0", Target: target, Backup: target + ".previous"}})
	if err := os.WriteFile(filepath.Join(c.Observer.Runtime, update.StateFile), state, 0o600); err != nil {
		t.Fatal(err)
	}
	var spawned []string
	r.spawn = func(_ context.Context, got Config, binary string, args []string) error {
		spawned = append([]string{binary}, args...)
		if got.ConfigPath != c.ConfigPath {
			t.Errorf("config %+v", got)
		}
		return nil
	}
	if err := r.Restart(context.Background(), "1.1.0"); !errors.Is(err, update.ErrDeferred) {
		t.Fatalf("got %v", err)
	}
	if strings.Join(spawned, " ") != target+" update --finish --config "+c.ConfigPath {
		t.Fatalf("spawned %v", spawned)
	}
	r.spawn = func(context.Context, Config, string, []string) error { return errors.New("fork failed") }
	if err := r.Restart(context.Background(), "1.1.0"); err == nil || errors.Is(err, update.ErrDeferred) {
		t.Fatalf("spawn failure hidden: %v", err)
	}
}

func TestNewUpdaterEnvironmentHooks(t *testing.T) {
	c := testConfig(t)
	t.Setenv("RHIZOME_UPDATE_SOURCE_URL", "")
	t.Setenv("RHIZOME_UPDATE_TRUSTED_KEYS", "")
	if _, err := newUpdater(c, nil, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RHIZOME_UPDATE_TRUSTED_KEYS", strings.Repeat("ab", 32))
	if _, err := newUpdater(c, nil, nil); err == nil || !strings.Contains(err.Error(), "require") {
		t.Fatalf("key override without a loopback source accepted: %v", err)
	}
	t.Setenv("RHIZOME_UPDATE_SOURCE_URL", "https://mirror.example")
	if _, err := newUpdater(c, nil, nil); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("non-loopback source accepted: %v", err)
	}
	t.Setenv("RHIZOME_UPDATE_SOURCE_URL", "http://127.0.0.1:9/")
	if _, err := newUpdater(c, nil, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RHIZOME_UPDATE_TRUSTED_KEYS", "zz")
	if _, err := newUpdater(c, nil, nil); err == nil {
		t.Fatal("malformed key accepted")
	}
	t.Setenv("RHIZOME_UPDATE_TRUSTED_KEYS", "")
	t.Setenv("RHIZOME_UPDATE_INITIAL_DELAY", "0s")
	if _, err := newUpdater(c, nil, nil); err == nil {
		t.Fatal("zero initial delay accepted")
	}
	t.Setenv("RHIZOME_UPDATE_INITIAL_DELAY", "2s")
	if _, err := newUpdater(c, nil, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RHIZOME_UPDATE_SOURCE_URL", "")
	if _, err := newUpdater(c, nil, nil); err == nil {
		t.Fatal("initial delay without a loopback source accepted")
	}
}

func TestInspectReportsUpdateState(t *testing.T) {
	c := testConfig(t)
	s, err := Inspect(context.Background(), c)
	if err != nil || s.Update != nil {
		t.Fatal(s, err)
	}
	if err := os.WriteFile(filepath.Join(c.Observer.Runtime, update.StateFile), []byte(`{"version":"1.0.0","last_outcome":"up_to_date"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err = Inspect(context.Background(), c)
	if err != nil || !strings.Contains(string(s.Update), "up_to_date") {
		t.Fatal(s, err)
	}
	if err := os.WriteFile(filepath.Join(c.Observer.Runtime, update.StateFile), []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(context.Background(), c); err == nil {
		t.Fatal("malformed state accepted")
	}
}

func TestRunForegroundDisabledUpdatesMakeNoRequests(t *testing.T) {
	c := testConfig(t)
	c.Observer.ClaudeRoot, c.Observer.CodexRoot = filepath.Join(c.Observer.Runtime, "claude"), filepath.Join(c.Observer.Runtime, "codex")
	// A loopback source nobody listens on: any request would fail loudly in
	// the state file, and none is expected while updates are disabled.
	t.Setenv("RHIZOME_UPDATE_SOURCE_URL", "http://127.0.0.1:9/")
	SetBuild("1.0.0", "")
	t.Cleanup(func() { SetBuild("", "") })
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if err := RunForeground(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.Observer.Runtime, update.StateFile)); !os.IsNotExist(err) {
		t.Fatal("updater ran while disabled", err)
	}
	// Enabled: the loop schedules its first check (one minute away) and records that.
	c.AutoUpdate.Enabled, c.AutoUpdate.CheckInterval = true, time.Hour
	ctx, cancel = context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if err := RunForeground(ctx, c); err != nil {
		t.Fatal(err)
	}
	s, err := update.ReadState(c.Observer.Runtime)
	if err != nil || s.NextCheckAt.IsZero() || s.LastOutcome != "" {
		t.Fatalf("%+v %v", s, err)
	}
	health, _ := os.ReadFile(filepath.Join(c.Observer.Runtime, "observer-health.json"))
	_ = health // removed at shutdown; the version field is covered by waitHealthy tests
}
