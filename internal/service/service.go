// Package service manages a per-user native background observer.
package service

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"rhizome/internal/channel"
	"rhizome/internal/localstate"
)

// Config captures absolute paths at installation; no shell environment is needed.
type Config struct {
	Binary   string
	Observer channel.ObserverOptions
}
type command struct {
	name string
	args []string
}
type manager struct {
	platform, home, configPath, definition string
	run                                    func(context.Context, string, ...string) error
}

// Defaults resolves the current user's data directories without changing them.
func Defaults() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, err
	}
	binary, err := os.Executable()
	if err != nil {
		return Config{}, err
	}
	agentRoot := func(env, fallback string) string {
		if v := os.Getenv(env); v != "" {
			return v
		}
		return filepath.Join(home, fallback)
	}
	rt, err := localstate.Runtime(os.Getenv("RHIZOME_RUNTIME_DIR"))
	if err != nil {
		return Config{}, err
	}
	return Config{Binary: binary, Observer: channel.ObserverOptions{Runtime: rt, ClaudeRoot: filepath.Join(agentRoot("CLAUDE_CONFIG_DIR", ".claude"), "projects"), CodexRoot: filepath.Join(agentRoot("CODEX_HOME", ".codex"), "sessions")}}, nil
}
func newManager() (*manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(configDir, "rhizome")
	m := &manager{platform: runtime.GOOS, home: home, configPath: filepath.Join(dir, "service.json")}
	switch m.platform {
	case "darwin":
		m.definition = filepath.Join(home, "Library", "LaunchAgents", "com.rhizome.observer.plist")
	case "linux":
		m.definition = filepath.Join(configDir, "systemd", "user", "rhizome-observer.service")
	case "windows":
		if currentUserID() == "" {
			return nil, errors.New("cannot resolve current Windows account SID")
		}
		m.definition = filepath.Join(dir, "observer-task.xml")
	default:
		return nil, fmt.Errorf("service management unsupported on %s", m.platform)
	}
	m.run = func(ctx context.Context, name string, args ...string) error {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	}
	return m, nil
}
func (m *manager) commands(action string) []command {
	switch m.platform {
	case "linux":
		switch action {
		case "install":
			return []command{{"systemctl", []string{"--user", "daemon-reload"}}, {"systemctl", []string{"--user", "enable", "--now", "rhizome-observer.service"}}}
		case "uninstall":
			return []command{{"systemctl", []string{"--user", "disable", "--now", "rhizome-observer.service"}}}
		default:
			return []command{{"systemctl", []string{"--user", action, "rhizome-observer.service"}}}
		}
	case "darwin":
		domain := fmt.Sprintf("gui/%d", os.Getuid())
		target := domain + "/com.rhizome.observer"
		switch action {
		case "install":
			return []command{{"launchctl", []string{"bootstrap", domain, m.definition}}}
		case "start":
			return []command{{"launchctl", []string{"kickstart", target}}}
		case "stop":
			return []command{{"launchctl", []string{"bootout", target}}}
		case "status":
			return []command{{"launchctl", []string{"print", target}}}
		case "uninstall":
			return []command{{"launchctl", []string{"bootout", target}}}
		}
	case "windows":
		switch action {
		case "install":
			return []command{{"schtasks", []string{"/Create", "/TN", taskName(), "/XML", m.definition, "/F"}}, {"schtasks", []string{"/Run", "/TN", taskName()}}}
		case "start":
			return []command{{"schtasks", []string{"/Run", "/TN", taskName()}}}
		case "stop":
			return []command{{"schtasks", []string{"/End", "/TN", taskName()}}}
		case "status":
			return []command{{"schtasks", []string{"/Query", "/TN", taskName(), "/V", "/FO", "LIST"}}}
		case "uninstall":
			return []command{{"schtasks", []string{"/Delete", "/TN", taskName(), "/F"}}}
		}
	}
	return nil
}
func escaped(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
func unitArg(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(s) + `"`
}
func (m *manager) render(c Config) string {
	switch m.platform {
	case "linux":
		return "[Unit]\nDescription=Rhizome session observer\n[Service]\nExecStart=" + unitArg(c.Binary) + " service run --config " + unitArg(m.configPath) + "\nRestart=on-failure\nRestartSec=5\nUMask=0077\n[Install]\nWantedBy=default.target\n"
	case "darwin":
		args := ""
		for _, a := range []string{c.Binary, "service", "run", "--config", m.configPath} {
			args += "<string>" + escaped(a) + "</string>"
		}
		return `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>Label</key><string>com.rhizome.observer</string><key>ProgramArguments</key><array>` + args + `</array><key>RunAtLoad</key><true/><key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict><key>ThrottleInterval</key><integer>5</integer><key>StandardErrorPath</key><string>` + escaped(filepath.Join(c.Observer.Runtime, "service-errors.log")) + `</string></dict></plist>`
	case "windows":
		// InteractiveToken is intentionally per-user: no password or SYSTEM account.
		return `<?xml version="1.0" encoding="UTF-8"?><Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task"><Triggers><LogonTrigger><UserId>` + escaped(currentUserID()) + `</UserId><Enabled>true</Enabled></LogonTrigger></Triggers><Principals><Principal id="Author"><UserId>` + escaped(currentUserID()) + `</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals><Settings><MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy><DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries><StopIfGoingOnBatteries>false</StopIfGoingOnBatteries><ExecutionTimeLimit>PT0S</ExecutionTimeLimit><RestartOnFailure><Interval>PT1M</Interval><Count>3</Count></RestartOnFailure></Settings><Actions Context="Author"><Exec><Command>` + escaped(c.Binary) + `</Command><Arguments>service run --config ` + escaped(windowsQuote(m.configPath)) + `</Arguments></Exec></Actions></Task>`
	}
	return ""
}
func validate(c *Config) error {
	paths := []*string{&c.Binary, &c.Observer.Runtime, &c.Observer.ClaudeRoot, &c.Observer.CodexRoot}
	for _, p := range paths {
		if *p == "" {
			return errors.New("binary, runtime and log roots must be configured")
		}
		a, err := filepath.Abs(*p)
		if err != nil {
			return err
		}
		*p = filepath.Clean(a)
	}
	if c.Observer.ClaudeRoot == c.Observer.CodexRoot {
		return errors.New("Claude and Codex log roots must differ")
	}
	return nil
}

// Load reads a bounded service configuration file.
func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	var c Config
	err = json.NewDecoder(io.LimitReader(f, 64*1024)).Decode(&c)
	if err != nil {
		return c, err
	}
	return c, validate(&c)
}

// Manage installs or controls this user's service. Installation never changes agent data.
func Manage(ctx context.Context, action string, c Config) error {
	m, err := newManager()
	if err != nil {
		return err
	}
	switch action {
	case "install", "start", "stop", "status", "uninstall":
	default:
		return fmt.Errorf("unknown service action %q", action)
	}
	if action == "install" {
		if err := validate(&c); err != nil {
			return err
		}
		if err := localstate.SecureDir(filepath.Dir(m.configPath)); err != nil {
			return err
		}
		if err := localstate.SecureDir(c.Observer.Runtime); err != nil {
			return err
		}
		// Reinstallation with identical paths is idempotent. Require stop/uninstall
		// before changing a running service's configuration.
		if old, err := Load(m.configPath); err == nil {
			if old != c {
				return errors.New("service already configured differently; uninstall before reinstalling")
			}
			if running(c.Observer.Runtime) {
				return nil
			}
		}
		b, _ := json.MarshalIndent(c, "", "  ")
		if err := os.WriteFile(m.configPath, b, 0600); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(m.definition), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(m.definition, []byte(m.render(c)), 0600); err != nil {
			return err
		}
	} else {
		c, err = Load(m.configPath)
		if err != nil {
			if action == "uninstall" && os.IsNotExist(err) {
				return nil
			}
			return err
		}
	}
	if action == "status" {
		b, readErr := os.ReadFile(filepath.Join(c.Observer.Runtime, "observer-health.json"))
		if readErr == nil {
			fmt.Println(string(b))
		}
		if !running(c.Observer.Runtime) {
			return errors.New("observer is not running")
		}
	}
	if m.platform == "darwin" && (action == "start" || action == "install") {
		target := fmt.Sprintf("gui/%d/com.rhizome.observer", os.Getuid())
		probe := exec.CommandContext(ctx, "launchctl", "print", target)
		if probe.Run() == nil {
			action = "start"
		} else {
			action = "install"
		}
	}
	if action == "stop" && m.platform == "darwin" {
		probe := exec.CommandContext(ctx, "launchctl", "print", fmt.Sprintf("gui/%d/com.rhizome.observer", os.Getuid()))
		if probe.Run() != nil {
			return nil
		}
	}
	if action == "stop" && m.platform != "darwin" && !running(c.Observer.Runtime) {
		return nil
	}
	if action == "uninstall" && m.platform == "windows" && running(c.Observer.Runtime) {
		if err := m.run(ctx, "schtasks", "/End", "/TN", taskName()); err != nil {
			return err
		}
	}
	for _, cmd := range m.commands(action) {
		if action == "uninstall" && m.platform == "darwin" {
			probe := exec.CommandContext(ctx, "launchctl", "print", fmt.Sprintf("gui/%d/com.rhizome.observer", os.Getuid()))
			if probe.Run() != nil {
				continue
			}
		}
		if err := m.run(ctx, cmd.name, cmd.args...); err != nil {
			return err
		}
	}
	if action == "install" || action == "start" {
		deadline := time.NewTimer(15 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for !running(c.Observer.Runtime) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-deadline.C:
				return errors.New("service manager accepted startup but observer did not acquire its runtime lock; inspect service logs")
			case <-tick.C:
			}
		}
	}
	if action == "uninstall" {
		for _, path := range []string{m.definition, m.configPath} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if m.platform == "linux" {
			return m.run(ctx, "systemctl", "--user", "daemon-reload")
		}
	}
	return nil
}
func running(dir string) bool {
	f, err := localstate.Lock(filepath.Join(dir, "observer.lock"), false)
	if f != nil {
		f.Close()
	}
	return errors.Is(err, localstate.ErrBusy)
}
