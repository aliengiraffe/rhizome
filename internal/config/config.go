// Package config resolves the single per-user configuration before startup.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"rhizome/internal/localstate"
)

// Config contains effective settings and their provenance. It never holds tokens.
// Settings are grouped by the agent they describe; which agent a process serves
// is a per-launch choice (`rhizome mcp --agent`) and is never configured here.
type Config struct {
	RuntimeDir string `toml:"runtime_dir"`
	Hostname   string `toml:"hostname"`
	Claude     struct {
		ConfigDir   string      `toml:"config_dir"`   // CLAUDE_CONFIG_DIR; skills and transcripts live under it.
		ProjectsDir string      `toml:"projects_dir"` // Transcript root the observer watches; default <config_dir>/projects.
		PeerHistory PeerHistory `toml:"peer_history"` // Override of gateway.history for Claude-sourced history.
	} `toml:"claude"`
	Codex struct {
		Bin         string      `toml:"bin"`          // Codex CLI executable name or path.
		Home        string      `toml:"home"`         // CODEX_HOME; App Server socket and skills live under it.
		SessionsDir string      `toml:"sessions_dir"` // Session log root the observer watches; default <home>/sessions.
		PeerHistory PeerHistory `toml:"peer_history"` // Override of gateway.history for Codex-sourced history.
	} `toml:"codex"`
	OpenCode struct {
		DataDir     string      `toml:"data_dir"`     // Directory holding opencode.db; default $XDG_DATA_HOME/opencode or ~/.local/share/opencode.
		PeerHistory PeerHistory `toml:"peer_history"` // Override of gateway.history for OpenCode-sourced history.
	} `toml:"opencode"`
	Gateway      Gateway           `toml:"gateway"`
	Network      Network           `toml:"network"`
	Sessions     Sessions          `toml:"sessions"`
	Update       Update            `toml:"update"`
	PeerHistory  HistoryPolicy     `toml:"-"` // Effective policy for paired hosts reading local history.
	AutoUpdate   UpdatePolicy      `toml:"-"` // Effective [update] policy; only `service run` acts on it.
	RecentWindow time.Duration     `toml:"-"` // Effective [sessions].recent_window; zero disables recent-only rows.
	Path         string            `toml:"-"`
	Found        bool              `toml:"-"`
	Sources      map[string]string `toml:"-"`
}

// Network selects how `rhizome mcp` peers. Mode "local" (the default) is
// loopback only; "lan" and "tailscale" are the two opt-in wider modes. Unset
// fields keep their built-in defaults. Flags and RHIZOME_* variables override
// every key, and none of them ever holds the cluster token.
type Network struct {
	Mode              string   `toml:"mode"`
	BindAddr          string   `toml:"bind_addr"`
	AdvertiseAddr     string   `toml:"advertise_addr"`
	Join              []string `toml:"join"`
	MDNS              *bool    `toml:"mdns"`
	GossipPort        int      `toml:"gossip_port"`
	DiscoveryInterval string   `toml:"discovery_interval"`
}

// Network modes accepted by network.mode.
const (
	ModeLocal     = "local"
	ModeLAN       = "lan"
	ModeTailscale = "tailscale"
)

var networkKeys = []string{"mode", "bind_addr", "advertise_addr", "join", "mdns", "gossip_port", "discovery_interval"}

// Validate rejects unknown modes and options that the selected mode refuses at
// startup, so a bad file fails before any listener is opened.
func (n Network) Validate() error {
	switch n.Mode {
	case "", ModeLocal:
		if n.BindAddr != "" || n.AdvertiseAddr != "" || len(n.Join) > 0 || n.MDNS != nil {
			return errors.New("network: bind_addr, advertise_addr, join and mdns require mode \"lan\" or \"tailscale\"")
		}
	case ModeLAN, ModeTailscale:
	default:
		return fmt.Errorf("network.mode %q must be local, lan or tailscale", n.Mode)
	}
	if n.Mode != ModeTailscale && (n.GossipPort != 0 || n.DiscoveryInterval != "") {
		return errors.New("network: gossip_port and discovery_interval require mode \"tailscale\"")
	}
	if n.GossipPort < 0 || n.GossipPort > 65535 {
		return errors.New("network.gossip_port must be 1-65535")
	}
	if n.DiscoveryInterval != "" {
		if d, err := time.ParseDuration(n.DiscoveryInterval); err != nil || d <= 0 {
			return fmt.Errorf("network.discovery_interval %q must be a positive duration such as 20s", n.DiscoveryInterval)
		}
	}
	return nil
}

type setting struct {
	key, env, flag string
	value          *string
	path           bool
}

func (c *Config) settings() []setting {
	return []setting{
		{"hostname", "", "", &c.Hostname, false},
		{"gateway.mode", "RHIZOME_GATEWAY_MODE", "gateway-mode", &c.Gateway.Mode, false},
		{"gateway.listen", "RHIZOME_GATEWAY_LISTEN", "gateway-listen", &c.Gateway.Listen, false},
		{"gateway.advertise", "RHIZOME_GATEWAY_ADVERTISE", "gateway-advertise", &c.Gateway.Advertise, false},
		{"runtime_dir", "RHIZOME_RUNTIME_DIR", "runtime-dir", &c.RuntimeDir, true},
		{"claude.config_dir", "CLAUDE_CONFIG_DIR", "", &c.Claude.ConfigDir, true},
		{"claude.projects_dir", "", "claude-root", &c.Claude.ProjectsDir, true},
		{"codex.bin", "", "codex-bin", &c.Codex.Bin, false},
		{"codex.home", "CODEX_HOME", "", &c.Codex.Home, true},
		{"codex.sessions_dir", "", "codex-root", &c.Codex.SessionsDir, true},
		{"opencode.data_dir", "", "opencode-root", &c.OpenCode.DataDir, true},
	}
}

// Bind adds configuration selection and the requested override flags.
// "update" and "update-check-interval" override the [update] table.
func Bind(fs *flag.FlagSet, names ...string) {
	fs.String("config", "", "configuration TOML path (default ~/.rhizome/config.toml)")
	var c Config
	for _, name := range names {
		switch name {
		case "update":
			fs.String(name, "", "override update.enabled: true or false (RHIZOME_UPDATE)")
		case "update-check-interval":
			fs.String(name, "", "override update.check_interval, e.g. 24h (RHIZOME_UPDATE_CHECK_INTERVAL)")
		}
		for _, s := range c.settings() {
			if s.flag == name {
				fs.String(name, "", "override "+s.key)
			}
		}
	}
}

// FromFlags resolves explicitly visited flags over environment, file and defaults.
func FromFlags(fs *flag.FlagSet) (Config, error) {
	flags := map[string]string{}
	fs.Visit(func(f *flag.Flag) { flags[f.Name] = f.Value.String() })
	c, err := Resolve(flags, os.Getenv)
	if err == nil {
		slog.Info("configuration", "path", c.Path, "found", c.Found)
	}
	return c, err
}

// Target returns the configuration file a process would read and whether it was
// selected explicitly (--config or RHIZOME_CONFIG) rather than defaulted.
func Target(flags map[string]string, getenv func(string) string) (string, bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, err
	}
	path, explicit := filepath.Join(home, ".rhizome", "config.toml"), false
	if p := getenv("RHIZOME_CONFIG"); p != "" {
		path, explicit = p, true
	}
	if p, ok := flags["config"]; ok {
		path, explicit = p, true
	}
	path, err = expand(path, home)
	if err != nil {
		return path, explicit, fmt.Errorf("config path %q: %w", path, err)
	}
	return path, explicit, nil
}

// Resolve reads settings once. getenv may be replaced to resolve without an environment.
func Resolve(flags map[string]string, getenv func(string) string) (Config, error) {
	return resolve(flags, getenv, false)
}

// LoadForSetup uses the shared resolver while allowing a new destination.
// The editor must supply an empty environment to avoid persisting shell overrides.
func LoadForSetup(path string, getenv func(string) string) (Config, error) {
	return resolve(map[string]string{"config": path}, getenv, true)
}

func resolve(flags map[string]string, getenv func(string) string, allowMissing bool) (Config, error) {
	var c Config
	home, err := os.UserHomeDir()
	if err != nil {
		return c, err
	}
	path, explicit, err := Target(flags, getenv)
	if err != nil {
		return c, err
	}
	c.Path = path
	c.Sources = map[string]string{}
	file, found, err := read(path)
	if err != nil {
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	if !found && explicit && !allowMissing {
		return c, fmt.Errorf("config %s: %w", path, os.ErrNotExist)
	}
	c.Found = found
	return resolveDocument(c, home, file, flags, getenv)
}

// Preview validates staged TOML through the resolver without reading or writing a file.
func Preview(body string) (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, err
	}
	return resolveDocument(Config{Sources: map[string]string{}}, home, []byte(body), nil, func(string) string { return "" })
}

func resolveDocument(c Config, home string, file []byte, flags map[string]string, getenv func(string) string) (Config, error) {
	path := c.Path
	var err error
	var raw Config
	md, err := toml.Decode(string(file), &raw)
	if err != nil {
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	if keys := md.Undecoded(); len(keys) > 0 {
		return c, fmt.Errorf("config %s: unknown key %s", path, keys[0])
	}
	// TOML keys are case-sensitive even though the decoder can match Go fields
	// case-insensitively. Reject alternate spellings instead of losing their source.
	known := map[string]bool{"claude": true, "codex": true, "opencode": true, "network": true, "gateway": true, "sessions": true, "update": true}
	for _, k := range networkKeys {
		known["network."+k] = true
	}
	for _, k := range sessionsKeys {
		known["sessions."+k] = true
	}
	for _, k := range updateKeys {
		known["update."+k] = true
	}
	for _, table := range []string{"gateway.history", "claude.peer_history", "codex.peer_history", "opencode.peer_history"} {
		known[table] = true
		for _, k := range peerHistoryKeys {
			known[table+"."+k] = true
		}
	}
	for _, s := range raw.settings() {
		known[s.key] = true
	}
	for _, key := range md.Keys() {
		if !known[key.String()] {
			return c, fmt.Errorf("config %s: unknown key %s", path, key)
		}
	}
	// Validate file paths even if an override would hide the invalid value.
	for _, s := range raw.settings() {
		if (s.path || s.key == "codex.bin" && binaryPath(*s.value)) && *s.value != "" {
			if _, err := expand(*s.value, home); err != nil {
				return c, fmt.Errorf("config %s: %s: %w", path, s.key, err)
			}
		}
	}
	if err := raw.Network.Validate(); err != nil {
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	if err := raw.Gateway.Validate(); err != nil {
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	c.PeerHistory, err = historyPolicy(raw.Gateway.History, map[string]PeerHistory{"claude": raw.Claude.PeerHistory, "codex": raw.Codex.PeerHistory, "opencode": raw.OpenCode.PeerHistory}, func(table, key string) bool {
		return md.IsDefined(append(strings.Split(table, "."), key)...)
	})
	if err != nil {
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	c.Gateway.History, c.Claude.PeerHistory, c.Codex.PeerHistory, c.OpenCode.PeerHistory = raw.Gateway.History, raw.Claude.PeerHistory, raw.Codex.PeerHistory, raw.OpenCode.PeerHistory
	for _, table := range []string{"gateway.history", "claude.peer_history", "codex.peer_history", "opencode.peer_history"} {
		for _, k := range peerHistoryKeys {
			source := "default"
			if md.IsDefined(append(strings.Split(table, "."), k)...) {
				source = "file"
			}
			c.Sources[table+"."+k] = source
		}
	}
	c.Sessions, c.RecentWindow = raw.Sessions, DefaultRecentWindow
	c.Sources["sessions.recent_window"] = "default"
	if md.IsDefined("sessions", "recent_window") {
		if c.RecentWindow, err = ParseRecentWindow(raw.Sessions.RecentWindow); err != nil {
			return c, fmt.Errorf("config %s: %w", path, err)
		}
		c.Sources["sessions.recent_window"] = "file"
	}
	c.Update = raw.Update
	var updateSources map[string]string
	c.AutoUpdate, updateSources, err = resolveUpdate(raw.Update, func(key string) bool { return md.IsDefined("update", key) }, flags, getenv)
	if err != nil {
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	for k, v := range updateSources {
		c.Sources[k] = v
	}
	c.Network = raw.Network
	for _, k := range networkKeys {
		source := "default"
		if md.IsDefined("network", k) {
			source = "file"
		}
		c.Sources["network."+k] = source
	}
	c.Gateway.Mode = "off"
	c.RuntimeDir = filepath.Join(home, ".rhizome", "run")
	c.Codex.Bin = "codex"
	c.Claude.ConfigDir, c.Codex.Home = filepath.Join(home, ".claude"), filepath.Join(home, ".codex")
	dataHome := getenv("XDG_DATA_HOME")
	if !filepath.IsAbs(dataHome) {
		dataHome = filepath.Join(home, ".local", "share")
	}
	c.OpenCode.DataDir = filepath.Join(dataHome, "opencode")
	rawSettings := raw.settings()
	for i, s := range c.settings() {
		source := "default"
		if md.IsDefined(strings.Split(s.key, ".")...) && (*rawSettings[i].value != "" || s.key == "runtime_dir" || s.key == "codex.bin" || s.key == "hostname") {
			*s.value, source = *rawSettings[i].value, "file"
		}
		if s.env != "" {
			if v := getenv(s.env); v != "" {
				*s.value, source = v, "env"
			}
		}
		if v, ok := flags[s.flag]; s.flag != "" && ok {
			*s.value, source = v, "flag"
		}
		if source == "default" {
			switch s.key {
			case "claude.projects_dir":
				*s.value = filepath.Join(c.Claude.ConfigDir, "projects")
			case "codex.sessions_dir":
				*s.value = filepath.Join(c.Codex.Home, "sessions")
			case "opencode.data_dir":
				// The default was computed above from XDG_DATA_HOME.
			}
		}
		if s.key == "hostname" {
			*s.value, err = NormalizeHostname(*s.value)
			if err != nil {
				return c, fmt.Errorf("config %s: %w", path, err)
			}
		}
		if *s.value == "" && s.key != "hostname" && !strings.HasPrefix(s.key, "gateway.") {
			return c, fmt.Errorf("config %s: %s must not be empty (%s)", path, s.key, source)
		}
		if s.path || s.key == "codex.bin" && binaryPath(*s.value) {
			*s.value, err = expand(*s.value, home)
			if err != nil {
				return c, fmt.Errorf("config %s: %s (%s): %w", path, s.key, source, err)
			}
		}
		c.Sources[s.key] = source
	}
	if err := c.Gateway.Validate(); err != nil {
		return c, err
	}
	if c.Gateway.Mode != "off" && c.Gateway.Mode != "" && c.Network.Mode != "" && c.Network.Mode != ModeLocal {
		return c, errors.New("gateway pairing requires network.mode=local for MCP; use gateway.mode for host networking")
	}
	c.RuntimeDir, err = localstate.Runtime(c.RuntimeDir)
	if err != nil {
		return c, err
	}
	if c.Claude.ProjectsDir == c.Codex.SessionsDir {
		return c, fmt.Errorf("config %s: claude.projects_dir and codex.sessions_dir must differ", path)
	}
	return c, nil
}

func binaryPath(value string) bool {
	return strings.ContainsAny(value, `/\`) || strings.HasPrefix(value, "~") || strings.HasPrefix(value, "$HOME") || strings.HasPrefix(value, "${HOME}")
}

func expand(path, home string) (string, error) {
	if path == "~" {
		path = home
	} else if strings.HasPrefix(path, "~/") {
		path = filepath.Join(home, path[2:])
	}
	path = strings.ReplaceAll(strings.ReplaceAll(path, "${HOME}", home), "$HOME", home)
	if !filepath.IsAbs(path) {
		return path, errors.New("path must be absolute")
	}
	return filepath.Clean(path), nil
}

func read(path string) ([]byte, bool, error) {
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !st.Mode().IsRegular() {
		return nil, false, errors.New("file must be regular and not a symlink")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !os.SameFile(st, opened) {
		return nil, false, errors.New("file changed while opening")
	}
	if err := checkOwner(f, opened); err != nil {
		return nil, false, err
	}
	b, err := io.ReadAll(io.LimitReader(f, 64*1024+1))
	if len(b) > 64*1024 {
		return nil, false, errors.New("file exceeds 64 KiB")
	}
	return b, true, err
}

// Write prints all effective values and sources as annotated TOML.
func (c Config) Write(w io.Writer) error {
	state := "found"
	if !c.Found {
		state = "not found"
	}
	if _, err := fmt.Fprintf(w, "# config: %s (%s)\n", c.Path, state); err != nil {
		return err
	}
	for _, s := range c.settings() {
		if _, err := fmt.Fprintf(w, "%s = %q # source: %s\n", s.key, *s.value, c.Sources[s.key]); err != nil {
			return err
		}
	}
	n := c.Network
	mode := n.Mode
	if mode == "" {
		mode = ModeLocal
	}
	mdns := "unset"
	if n.MDNS != nil {
		mdns = fmt.Sprint(*n.MDNS)
	}
	lines := []struct{ key, value string }{
		{"network.mode", fmt.Sprintf("%q", mode)},
		{"network.bind_addr", fmt.Sprintf("%q", n.BindAddr)},
		{"network.advertise_addr", fmt.Sprintf("%q", n.AdvertiseAddr)},
		{"network.join", fmt.Sprintf("%q", n.Join)},
		{"network.mdns", mdns},
		{"network.gossip_port", fmt.Sprint(n.GossipPort)},
		{"network.discovery_interval", fmt.Sprintf("%q", n.DiscoveryInterval)},
	}
	for _, l := range lines {
		if _, err := fmt.Fprintf(w, "%s = %s # source: %s\n", l.key, l.value, c.Sources[l.key]); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "sessions.recent_window = %q # source: %s\n", c.RecentWindow.String(), c.Sources["sessions.recent_window"]); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "update.enabled = %t # source: %s\n", c.AutoUpdate.Enabled, c.Sources["update.enabled"]); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "update.check_interval = %q # source: %s\n", c.AutoUpdate.CheckInterval.String(), c.Sources["update.check_interval"]); err != nil {
		return err
	}
	// Peer history shows the effective, inherited policy, not only file values.
	if _, err := fmt.Fprintf(w, "gateway.history.enabled = %t # source: %s\n", c.PeerHistory.Enabled, c.Sources["gateway.history.enabled"]); err != nil {
		return err
	}
	global := c.Gateway.History.Lookback
	if global == "" {
		global = DefaultLookback
	}
	if _, err := fmt.Fprintf(w, "gateway.history.lookback = %q # source: %s\n", global, c.Sources["gateway.history.lookback"]); err != nil {
		return err
	}
	for _, agent := range HistoryAgents {
		a := c.PeerHistory.Agent(agent)
		for _, l := range []struct{ key, value string }{{"enabled", fmt.Sprint(a.Enabled)}, {"lookback", fmt.Sprintf("%q", a.Lookback())}} {
			key := agent + ".peer_history." + l.key
			source := c.Sources[key]
			if source == "default" {
				source = "inherited"
			}
			if _, err := fmt.Fprintf(w, "%s = %s # effective; source: %s\n", key, l.value, source); err != nil {
				return err
			}
		}
	}
	return nil
}

// NormalizeHostname validates a display label; empty means the OS hostname.
// Labels are bounded by the existing 64-byte host metadata budget.
func NormalizeHostname(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !utf8.ValidString(value) {
		return "", errors.New("hostname must be valid UTF-8")
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", errors.New("hostname must not contain control characters")
		}
	}
	if len(value) > 64 {
		return "", errors.New("hostname must be at most 64 UTF-8 bytes")
	}
	return value, nil
}
