package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"rhizome/internal/localstate"
	"rhizome/internal/release"
	"rhizome/internal/update"
)

// build is the running binary's identity, set once by main through SetBuild.
var build struct{ version, commit string }

// SetBuild records the version and commit the running binary reports. The
// observer publishes the version in its health file and the updater uses it
// for eligibility; an empty version behaves like a development build.
func SetBuild(version, commit string) { build.version, build.commit = version, commit }

// Build returns the recorded version and commit.
func Build() (version, commit string) { return build.version, build.commit }

// healthDeadline bounds how long a restarted observer may take to report the
// expected version before the previous binary is restored.
const healthDeadline = 60 * time.Second

// Test-only environment hooks. RHIZOME_UPDATE_SOURCE_URL points both the API
// and download endpoints at a loopback fake; RHIZOME_UPDATE_TRUSTED_KEYS
// replaces the compiled trusted keys and RHIZOME_UPDATE_INITIAL_DELAY shortens
// the delay before the first background check. Both are honored only together
// with a loopback source, so no environment can redirect a real installation
// to a differently keyed mirror.
const (
	envSource  = "RHIZOME_UPDATE_SOURCE_URL"
	envKeys    = "RHIZOME_UPDATE_TRUSTED_KEYS"
	envInitial = "RHIZOME_UPDATE_INITIAL_DELAY"
)

// newUpdater builds the updater for the resolved configuration.
func newUpdater(c Config, restarter update.Restarter, log *slog.Logger) (*update.Updater, error) {
	if log == nil {
		log = slog.Default()
	}
	api, downloads := update.DefaultAPI, update.DefaultDownloads
	keys, err := release.TrustedKeys()
	if err != nil {
		return nil, err
	}
	var initial time.Duration
	if override := os.Getenv(envSource); override != "" {
		u, err := url.Parse(override)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("%s must be a loopback URL", envSource)
		}
		if ip := net.ParseIP(u.Hostname()); u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, fmt.Errorf("%s must be a loopback URL", envSource)
		}
		api, downloads = override, override
		if texts := os.Getenv(envKeys); texts != "" {
			if keys, err = release.ParsePublicKeys(strings.Split(texts, ",")); err != nil {
				return nil, fmt.Errorf("%s: %w", envKeys, err)
			}
		}
		if v := os.Getenv(envInitial); v != "" {
			if initial, err = time.ParseDuration(v); err != nil || initial <= 0 {
				return nil, fmt.Errorf("%s must be a positive duration", envInitial)
			}
		}
		log.Warn("update_source_override", "source", override, "trusted_keys", len(keys), "initial_delay", initial)
	} else if os.Getenv(envKeys) != "" || os.Getenv(envInitial) != "" {
		return nil, fmt.Errorf("%s and %s require %s", envKeys, envInitial, envSource)
	}
	src, err := update.NewGitHub(api, downloads, "rhizome/"+defaultString(build.version, "dev"), nil)
	if err != nil {
		return nil, err
	}
	return update.New(update.Options{Runtime: c.Observer.Runtime, Version: build.version, Commit: build.commit, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Source: src, Keys: keys, Verifier: update.PlatformVerifier(runtime.GOOS), Restarter: restarter, Interval: c.AutoUpdate.CheckInterval, Initial: initial, Logger: log})
}

func defaultString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// NewUpdater is the explicit `rhizome update` entry point: the same code path
// as the background loop, independent of [update].enabled, restarting an
// installed and running service from the calling process.
func NewUpdater(c Config, log *slog.Logger) (*update.Updater, error) {
	return newUpdater(c, &serviceRestarter{c: c, manage: ManageQuiet}, log)
}

// serviceRestarter restarts an installed, running observer through the
// native lifecycle from outside it and waits for the new version's health.
type serviceRestarter struct {
	c      Config
	manage func(context.Context, string, Config) error
}

func (r *serviceRestarter) Restart(ctx context.Context, version string) error {
	status, err := Inspect(ctx, r.c)
	if err != nil {
		return err
	}
	if !status.Installed || !status.Running {
		// Nothing is supervised; the binary is in place for the next start.
		return nil
	}
	if err := r.manage(ctx, "stop", r.c); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	if err := r.manage(ctx, "start", r.c); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	return waitHealthy(ctx, r.c, version)
}

// waitHealthy waits until the observer holds its lock and reports version.
func waitHealthy(ctx context.Context, c Config, version string) error {
	deadline := time.NewTimer(healthDeadline)
	defer deadline.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		if running(c.Observer.Runtime) {
			if got, ok := healthVersion(c.Observer.Runtime); ok && got == version {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			got, _ := healthVersion(c.Observer.Runtime)
			return fmt.Errorf("observer did not report version %s within %s (running %t, health version %q)", version, healthDeadline, running(c.Observer.Runtime), got)
		case <-tick.C:
		}
	}
}

func healthVersion(runtimeDir string) (string, bool) {
	b, err := readJSON(filepath.Join(runtimeDir, "observer-health.json"), "observer health")
	if err != nil || b == nil {
		return "", false
	}
	var h struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(b, &h) != nil {
		return "", false
	}
	return h.Version, true
}

// selfRestarter is used by the observer updating itself. A running process
// cannot supervise its own restart, so when a native definition is installed
// it starts a detached finisher (`rhizome update --finish`) from the new
// binary, which restarts the service through the lifecycle, checks health and
// restores the backup on failure. Without a definition the process exits so a
// custom supervisor, or the user, restarts it.
type selfRestarter struct {
	c     Config
	spawn func(ctx context.Context, c Config, binary string, args []string) error
}

func (r *selfRestarter) Restart(ctx context.Context, version string) error {
	m, err := newManager(r.c.ConfigPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(m.definition); err != nil {
		return update.ErrRestartRequired
	}
	s, err := update.ReadState(r.c.Observer.Runtime)
	if err != nil || s.Pending == nil {
		return fmt.Errorf("pending update state unavailable: %v", err)
	}
	spawn := r.spawn
	if spawn == nil {
		spawn = startFinisher
	}
	if err := spawn(ctx, r.c, s.Pending.Target, []string{"update", "--finish", "--config", r.c.ConfigPath}); err != nil {
		return fmt.Errorf("start update finisher: %w", err)
	}
	return update.ErrDeferred
}

// Finish runs the detached finisher: it is invoked by the new binary with the
// installed configuration and completes the pending restart.
func Finish(ctx context.Context, c Config) error {
	if err := localstate.SecureDir(c.Observer.Runtime); err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(&rotatingLog{path: filepath.Join(c.Observer.Runtime, "update.log")}, nil))
	// The observer that spawned this process exits during "stop"; releasing
	// its update lock is what lets Finish proceed.
	return update.Finish(ctx, c.Observer.Runtime, &serviceRestarter{c: c, manage: ManageQuiet}, log)
}

// finisherOutput is where the detached finisher's native command output goes.
func finisherOutput(c Config) (*os.File, error) {
	return os.OpenFile(filepath.Join(c.Observer.Runtime, "update-finish.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}
