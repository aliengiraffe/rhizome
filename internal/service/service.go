// Package service manages a per-user native background observer.
package service

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf16"

	"rhizome/internal/channel"
	"rhizome/internal/config"
	"rhizome/internal/localstate"
)

// Config contains the resolved observer settings and selected TOML path.
type Config struct {
	ConfigPath   string
	Gateway      config.Gateway
	PeerHistory  config.HistoryPolicy // Served to paired hosts by the gateway; read at service start.
	RecentWindow time.Duration        // [sessions].recent_window the gateway advertises; read at service start.
	Debug        bool                 // debug-level observer diagnostics; read at service start.
	Observer     channel.ObserverOptions
}

// FromConfig adapts resolved configuration for the observer.
func FromConfig(c config.Config) Config {
	return Config{ConfigPath: c.Path, Gateway: c.Gateway, PeerHistory: c.PeerHistory, RecentWindow: c.RecentWindow, Debug: c.Debug, Observer: channel.ObserverOptions{Hostname: c.Hostname, Runtime: c.RuntimeDir, ClaudeRoot: c.Claude.ProjectsDir, CodexRoot: c.Codex.SessionsDir, OpenCodeRoot: c.OpenCode.DataDir}}
}

type command struct {
	name string
	args []string
}
type manager struct {
	platform, home, configPath, definition, binary string
	run                                            func(context.Context, string, ...string) error
}

func newManager(configPath string) (*manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(configDir, "rhizome")
	binary, err := os.Executable()
	if err != nil {
		return nil, err
	}
	m := &manager{platform: runtime.GOOS, home: home, configPath: configPath, binary: binary}
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
	definition := m.renderDefinition(c)
	marker := "rhizome-runtime: " + base64.StdEncoding.EncodeToString([]byte(c.Observer.Runtime))
	if m.platform == "linux" {
		return definition + "# " + marker + "\n"
	}
	return definition + "\n<!-- " + marker + " -->\n"
}

func installedRuntime(definition string) (string, error) {
	const marker = "rhizome-runtime: "
	_, tail, ok := strings.Cut(definition, marker)
	if !ok {
		return "", errors.New("installed definition lacks runtime metadata; stop and uninstall it before reinstalling")
	}
	fields := strings.Fields(tail)
	if len(fields) == 0 {
		return "", errors.New("invalid installed runtime metadata")
	}
	b, err := base64.StdEncoding.DecodeString(fields[0])
	if err != nil || !filepath.IsAbs(string(b)) {
		return "", errors.New("invalid installed runtime metadata")
	}
	return string(b), nil
}

func (m *manager) renderDefinition(c Config) string {
	switch m.platform {
	case "linux":
		return "[Unit]\nDescription=Rhizome session observer\n[Service]\nExecStart=" + unitArg(m.binary) + " service run --config " + unitArg(m.configPath) + "\nRestart=on-failure\nRestartSec=5\nUMask=0077\n[Install]\nWantedBy=default.target\n"
	case "darwin":
		args := ""
		for _, a := range []string{m.binary, "service", "run", "--config", m.configPath} {
			args += "<string>" + escaped(a) + "</string>"
		}
		return `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>Label</key><string>com.rhizome.observer</string><key>ProgramArguments</key><array>` + args + `</array><key>RunAtLoad</key><true/><key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict><key>ThrottleInterval</key><integer>5</integer><key>StandardErrorPath</key><string>` + escaped(filepath.Join(c.Observer.Runtime, "service-errors.log")) + `</string></dict></plist>`
	case "windows":
		// InteractiveToken is intentionally per-user: no password or SYSTEM account.
		// schtasks /XML rejects UTF-8 task files; writeDefinition stores this as UTF-16LE.
		return `<?xml version="1.0" encoding="UTF-16"?><Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task"><Triggers><LogonTrigger><UserId>` + escaped(currentUserID()) + `</UserId><Enabled>true</Enabled></LogonTrigger></Triggers><Principals><Principal id="Author"><UserId>` + escaped(currentUserID()) + `</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals><Settings><MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy><DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries><StopIfGoingOnBatteries>false</StopIfGoingOnBatteries><ExecutionTimeLimit>PT0S</ExecutionTimeLimit><RestartOnFailure><Interval>PT1M</Interval><Count>3</Count></RestartOnFailure></Settings><Actions Context="Author"><Exec><Command>` + escaped(m.binary) + `</Command><Arguments>service run --config ` + escaped(windowsQuote(m.configPath)) + `</Arguments></Exec></Actions></Task>`
	}
	return ""
}
func validate(c *Config) error {
	paths := []*string{&c.Observer.Runtime, &c.Observer.ClaudeRoot, &c.Observer.CodexRoot}
	for _, p := range paths {
		if *p == "" {
			return errors.New("runtime and log roots must be configured")
		}
		if !filepath.IsAbs(*p) {
			return errors.New("runtime and log roots must be absolute")
		}
		*p = filepath.Clean(*p)
	}
	if c.Observer.ClaudeRoot == c.Observer.CodexRoot {
		return errors.New("Claude and Codex log roots must differ")
	}
	// The OpenCode data directory is optional here so older callers keep working.
	if p := &c.Observer.OpenCodeRoot; *p != "" {
		if !filepath.IsAbs(*p) {
			return errors.New("runtime and log roots must be absolute")
		}
		*p = filepath.Clean(*p)
		if *p == c.Observer.ClaudeRoot || *p == c.Observer.CodexRoot {
			return errors.New("OpenCode data directory must differ from the Claude and Codex log roots")
		}
	}
	return nil
}

// Manage installs or controls this user's service. Installation never changes agent data.
func Manage(ctx context.Context, action string, c Config) error {
	m, err := newManager(c.ConfigPath)
	if err != nil {
		return err
	}
	return m.manage(ctx, action, c)
}

func (m *manager) manage(ctx context.Context, action string, c Config) error {
	switch action {
	case "install", "start", "stop", "status", "uninstall":
	default:
		return fmt.Errorf("unknown service action %q", action)
	}
	if action == "install" {
		if err := validate(&c); err != nil {
			return err
		}
		// Definitions refer only to the live file, never a snapshot of this shell.
		disk, err := config.Resolve(map[string]string{"config": c.ConfigPath}, func(string) string { return "" })
		if err != nil {
			return fmt.Errorf("service install requires an existing TOML configuration: %w", err)
		}
		if FromConfig(disk) != c {
			return errors.New("service install overrides differ from the file; put persistent paths in config.toml")
		}
		if err := localstate.SecureDir(c.Observer.Runtime); err != nil {
			return err
		}
		old, err := m.readDefinition()
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			previous, err := installedRuntime(string(old))
			if err != nil {
				return err
			}
			if running(previous) {
				if previous == c.Observer.Runtime && string(old) == m.render(c) {
					return nil
				}
				return errors.New("observer is running; stop it before changing its runtime directory")
			}
			if string(old) != m.render(c) {
				return errors.New("service already configured differently; uninstall before reinstalling")
			}
		}
		if running(c.Observer.Runtime) {
			return errors.New("observer is running; stop it before reinstalling")
		}
		if err := os.MkdirAll(filepath.Dir(m.definition), 0700); err != nil {
			return err
		}
		if err := m.writeDefinition(m.render(c)); err != nil {
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
		for _, path := range []string{m.definition} {
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
// writeDefinition stores Windows task XML as BOM-prefixed UTF-16LE, the encoding schtasks requires.
func (m *manager) writeDefinition(s string) error {
	b := []byte(s)
	if m.platform == "windows" {
		units := utf16.Encode([]rune(s))
		b = make([]byte, 2+2*len(units))
		b[0], b[1] = 0xFF, 0xFE
		for i, u := range units {
			binary.LittleEndian.PutUint16(b[2+2*i:], u)
		}
	}
	return os.WriteFile(m.definition, b, 0600)
}

// readDefinition returns the installed definition, decoding BOM-prefixed UTF-16LE.
func (m *manager) readDefinition() (string, error) {
	b, err := os.ReadFile(m.definition)
	if err != nil {
		return "", err
	}
	if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE {
		b = b[2:]
		units := make([]uint16, len(b)/2)
		for i := range units {
			units[i] = binary.LittleEndian.Uint16(b[2*i:])
		}
		return string(utf16.Decode(units)), nil
	}
	return string(b), nil
}

func running(dir string) bool {
	f, err := localstate.Lock(filepath.Join(dir, "observer.lock"), false)
	if f != nil {
		f.Close()
	}
	return errors.Is(err, localstate.ErrBusy)
}
