package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"rhizome/internal/config"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// configAnswers holds deliberate setup edits; omitted settings remain stored.
type configAnswers struct {
	Stored     map[string]any
	Selected   map[string]bool
	Gateway    config.Gateway
	Hostname   string
	Network    config.Network
	RuntimeDir string
	ClaudeDir  string
	CodexHome  string
	CodexBin   string
	AutoUpdate *bool // [update].enabled; nil leaves the stored value
}

// storedConfig reads the TOML document at path as the base for edits. A
// missing file yields an empty document; a malformed one is an error and is
// never replaced.
func storedConfig(path string) (map[string]any, error) {
	stored := map[string]any{}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return stored, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := toml.Decode(string(body), &stored); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return stored, nil
}

// render merges deliberate edits into the stored TOML, without materializing
// effective defaults or environment overrides. The shared resolver validates it.
func (a configAnswers) render() (string, error) {
	d := a.Stored
	if d == nil {
		d = map[string]any{}
	}
	set := func(table, key string, value any) {
		if table == "" {
			d[key] = value
			return
		}
		m, ok := d[table].(map[string]any)
		if !ok {
			m = map[string]any{}
			d[table] = m
		}
		m[key] = value
	}
	selected := func(key string, nonzero bool) bool { return a.Selected[key] || a.Selected == nil && nonzero }
	hostname, err := config.NormalizeHostname(a.Hostname)
	if err != nil {
		return "", err
	}
	if selected("hostname", hostname != "") {
		if hostname == "" {
			delete(d, "hostname")
		} else {
			set("", "hostname", hostname)
		}
	}
	for _, f := range []struct{ table, key, flag, value string }{
		{"", "runtime_dir", "runtime-dir", a.RuntimeDir},
		{"claude", "config_dir", "claude-dir", a.ClaudeDir},
		{"codex", "home", "codex-home", a.CodexHome},
		{"codex", "bin", "codex-bin", a.CodexBin},
	} {
		if selected(f.flag, f.value != "") {
			set(f.table, f.key, f.value)
		}
	}
	n := a.Network
	if selected("network", n.Mode != "") {
		// Switching transport explicitly resets its transport-specific options.
		old, _ := d["network"].(map[string]any)
		if mode, _ := old["mode"].(string); mode != n.Mode {
			d["network"] = map[string]any{}
		}
		set("network", "mode", n.Mode)
	}
	if _, ok := d["network"]; !ok {
		set("network", "mode", "local")
	}
	for _, f := range []struct {
		key, flag string
		value     any
		nonzero   bool
	}{
		{"bind_addr", "bind-addr", n.BindAddr, n.BindAddr != ""},
		{"advertise_addr", "advertise-addr", n.AdvertiseAddr, n.AdvertiseAddr != ""},
		{"join", "join", n.Join, len(n.Join) > 0},
		{"gossip_port", "gossip-port", n.GossipPort, n.GossipPort != 0},
		{"discovery_interval", "discovery-interval", n.DiscoveryInterval, n.DiscoveryInterval != ""},
	} {
		if selected(f.flag, f.nonzero) {
			set("network", f.key, f.value)
		}
	}
	if n.MDNS != nil && selected("mdns", true) {
		set("network", "mdns", *n.MDNS)
	}
	g := a.Gateway
	if selected("gateway-mode", g.Mode != "") {
		set("gateway", "mode", g.Mode)
		if g.Mode == "off" {
			m := d["gateway"].(map[string]any)
			delete(m, "listen")
			delete(m, "advertise")
		}
	}
	if selected("gateway-listen", g.Listen != "") {
		set("gateway", "listen", g.Listen)
	}
	if selected("gateway-advertise", g.Advertise != "") {
		set("gateway", "advertise", g.Advertise)
	}
	if a.AutoUpdate != nil && selected("auto-update", true) {
		set("update", "enabled", *a.AutoUpdate)
	}
	var b bytes.Buffer
	b.WriteString("# Written by `rhizome setup`; see the README Configuration section.\n")
	b.WriteString("# Unset keys use defaults; flags and environment override this file.\n")
	if err := toml.NewEncoder(&b).Encode(d); err != nil {
		return "", err
	}
	return b.String(), nil
}

// writeConfig validates body with the real resolver, then installs it at path
// with mode 0600, creating the parent directory privately. An existing file is
// only replaced when replace is set, and is first copied to a timestamped
// backup, whose path is returned. The original remains until atomic replacement.
func writeConfig(path, body string, replace bool, now time.Time) (backup string, err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	st, statErr := os.Lstat(path)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return "", statErr
	}
	exists := statErr == nil
	if exists && !st.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	if exists && !replace {
		return "", fmt.Errorf("%s already exists", path)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.toml")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if _, err := config.Resolve(map[string]string{"config": tmp.Name()}, func(string) string { return "" }); err != nil {
		return "", fmt.Errorf("generated configuration is invalid: %w", err)
	}
	if exists {
		backup = path + ".bak-" + now.UTC().Format("20060102T150405Z")
		old, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		// Exclusive creation keeps rapid repeated edits from overwriting a backup.
		for i := 0; ; i++ {
			candidate := backup
			if i > 0 {
				candidate += fmt.Sprintf("-%d", i)
			}
			f, err := os.OpenFile(candidate, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if errors.Is(err, os.ErrExist) {
				continue
			}
			if err != nil {
				return "", err
			}
			_, writeErr := f.Write(old)
			closeErr := f.Close()
			if writeErr != nil {
				os.Remove(candidate)
				return "", writeErr
			}
			if closeErr != nil {
				os.Remove(candidate)
				return "", closeErr
			}
			backup = candidate
			break
		}
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return backup, err
	}
	return backup, nil
}

// prompter asks line-based questions. Enter always accepts the default.
type prompter struct {
	in  *bufio.Reader
	out io.Writer
}

func (p prompter) ask(question, def string) (string, error) {
	if def != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", question, def)
	} else {
		fmt.Fprintf(p.out, "%s: ", question)
	}
	line, err := p.in.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		return "", errors.New("input ended before the configuration questions were answered")
	}
	if line = strings.TrimSpace(line); line != "" {
		return line, nil
	}
	return def, nil
}

func (p prompter) yesNo(question string, def bool) (bool, error) {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	for {
		ans, err := p.ask(question+" ("+hint+")", "")
		if err != nil {
			return false, err
		}
		switch strings.ToLower(ans) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		fmt.Fprintln(p.out, "Please answer y or n.")
	}
}

func (p prompter) listDefault(question string, current []string) ([]string, error) {
	ans, err := p.ask(question+" (comma-separated; - for none)", strings.Join(current, ","))
	if ans == "-" {
		ans = ""
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, v := range strings.Split(ans, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out, nil
}

// askConfig runs the interactive questions. c supplies the displayed defaults;
// tailscale reports whether Tailscale mode could start now.
func askConfig(p prompter, c config.Config, tailscale func() error) (configAnswers, error) {
	a := configAnswers{Selected: map[string]bool{}}
	fmt.Fprintln(p.out, "Gateway pairing connects per-user services using invite/join; it requires local MCP networking. Legacy gossip instead shares a cluster token. History sharing is not enabled by setup.")
	gatewayDefault := c.Gateway.Mode
	if gatewayDefault == "" {
		gatewayDefault = "off"
	}
	for {
		mode, err := p.ask("Gateway pairing mode (off, lan, tailscale)", gatewayDefault)
		if err != nil {
			return a, err
		}
		if mode != "off" && mode != "lan" && mode != "tailscale" {
			fmt.Fprintln(p.out, "Choose off, lan or tailscale.")
			continue
		}
		a.Gateway.Mode = mode
		a.Selected["gateway-mode"] = mode != gatewayDefault
		break
	}
	if a.Gateway.Mode != "off" {
		for _, f := range []struct {
			question, def, key string
			dest               *string
		}{
			{"Gateway listen IP:port (- for automatic)", c.Gateway.Listen, "gateway-listen", &a.Gateway.Listen},
			{"Gateway advertise IP:port (- for automatic)", c.Gateway.Advertise, "gateway-advertise", &a.Gateway.Advertise},
		} {
			v, err := p.ask(f.question, f.def)
			if err != nil {
				return a, err
			}
			if v == "-" {
				v = ""
			}
			*f.dest = v
			a.Selected[f.key] = v != f.def
		}
		if a.Gateway.Mode == "tailscale" {
			if err := tailscale(); err != nil {
				fmt.Fprintf(p.out, "Warning: %v; the service will refuse to start until Tailscale is available.\n", err)
			}
		}
	}
	fmt.Fprint(p.out, `
How will Rhizome be used on this device?
  1) local      loopback MCP networking (gateway pairing is separate)
  2) lan        legacy shared-token gossip on your local network
  3) tailscale  legacy shared-token gossip on your tailnet
`)
	for a.Network.Mode == "" {
		ans, err := p.ask("MCP network (gateway pairing requires local)", defaultString(c.Network.Mode, "local"))
		if err != nil {
			return a, err
		}
		switch strings.ToLower(ans) {
		case "1", "local":
			a.Network.Mode = config.ModeLocal
		case "2", "lan":
			a.Network.Mode = config.ModeLAN
		case "3", "tailscale":
			a.Network.Mode = config.ModeTailscale
		default:
			fmt.Fprintln(p.out, "Choose 1, 2 or 3 (or local, lan, tailscale).")
		}
	}
	a.Selected["network"] = !c.Found || a.Network.Mode != defaultString(c.Network.Mode, "local")
	if a.Network.Mode == c.Network.Mode {
		a.Network = c.Network
	}
	if a.Gateway.Mode != "off" && a.Network.Mode != "local" {
		return a, errors.New("gateway pairing requires --network local; disable pairing explicitly with --gateway-mode off to use legacy gossip")
	}
	n := &a.Network
	switch n.Mode {
	case config.ModeLAN, config.ModeTailscale:
		fmt.Fprintln(p.out, "\nNote: this makes the device reachable by peers that hold the cluster token (see `rhizome token`).")
		fmt.Fprintln(p.out, "The token is managed separately and is never written to this file.")
	}
	var err error
	switch n.Mode {
	case config.ModeLAN:
		if n.BindAddr, err = p.ask("Listen address (private IP)", n.BindAddr); err != nil {
			return a, err
		}
		if n.AdvertiseAddr, err = p.ask("Address peers should dial", n.AdvertiseAddr); err != nil {
			return a, err
		}
		if n.Join, err = p.listDefault("Gossip host:port seeds to join", n.Join); err != nil {
			return a, err
		}
		mdns, err := p.yesNo("Advertise and browse _rhizome._tcp over mDNS", n.MDNS == nil || *n.MDNS)
		if err != nil {
			return a, err
		}
		if n.MDNS != nil || !mdns {
			n.MDNS = &mdns
		}
	case config.ModeTailscale:
		if err := tailscale(); err != nil {
			fmt.Fprintf(p.out, "Warning: %v\nThe configuration will still be written, but `rhizome mcp` will refuse to start in this mode until Tailscale is running.\n", err)
		}
		port, err := p.ask("Well-known gossip port", defaultString(strconv.Itoa(n.GossipPort), "7946"))
		if err != nil {
			return a, err
		}
		if n.GossipPort != 0 || port != "7946" {
			if n.GossipPort, err = strconv.Atoi(port); err != nil {
				return a, fmt.Errorf("gossip port %q: %w", port, err)
			}
		}
		interval, err := p.ask("Tailnet peer discovery interval", defaultString(n.DiscoveryInterval, "20s"))
		if err != nil {
			return a, err
		}
		if n.DiscoveryInterval != "" || interval != "20s" {
			n.DiscoveryInterval = interval
		}
		if n.Join, err = p.listDefault("Extra gossip host:port seeds to join (MagicDNS names allowed)", n.Join); err != nil {
			return a, err
		}
	}
	for {
		value, err := p.ask("Hostname override (- clears any previous override; Enter keeps it)", c.Hostname)
		if err != nil {
			return a, err
		}
		if value == "-" {
			value = ""
		}
		a.Hostname, err = config.NormalizeHostname(value)
		if err == nil {
			break
		}
		fmt.Fprintln(p.out, err)
	}
	a.Selected["hostname"] = a.Hostname != c.Hostname
	fmt.Fprintln(p.out, "\nThe background service can check GitHub Releases for newer signed builds and install them (outbound HTTPS to github.com only; `rhizome update` stays available either way).")
	autoUpdate, err := p.yesNo("Install verified updates automatically", c.AutoUpdate.Enabled)
	if err != nil {
		return a, err
	}
	if autoUpdate != c.AutoUpdate.Enabled || c.Sources["update.enabled"] == "file" {
		a.AutoUpdate = &autoUpdate
	}
	a.Selected["auto-update"] = a.AutoUpdate != nil && (c.Sources["update.enabled"] != "file" || autoUpdate != c.AutoUpdate.Enabled)
	a.Selected["bind-addr"] = n.BindAddr != c.Network.BindAddr
	a.Selected["advertise-addr"] = n.AdvertiseAddr != c.Network.AdvertiseAddr
	a.Selected["join"] = strings.Join(n.Join, ",") != strings.Join(c.Network.Join, ",")
	a.Selected["mdns"] = n.MDNS != nil && (c.Network.MDNS == nil || *n.MDNS != *c.Network.MDNS)
	a.Selected["gossip-port"] = n.GossipPort != c.Network.GossipPort
	a.Selected["discovery-interval"] = n.DiscoveryInterval != c.Network.DiscoveryInterval
	custom, err := p.yesNo("\nCustomize directories or the Codex binary", false)
	if err != nil || !custom {
		return a, err
	}
	for _, f := range []struct {
		question, def string
		dest          *string
	}{
		{"Runtime directory shared by MCP and the service", c.RuntimeDir, &a.RuntimeDir},
		{"Claude config directory", c.Claude.ConfigDir, &a.ClaudeDir},
		{"Codex home", c.Codex.Home, &a.CodexHome},
		{"Codex executable", c.Codex.Bin, &a.CodexBin},
	} {
		v, err := p.ask(f.question, f.def)
		if err != nil {
			return a, err
		}
		if v != f.def {
			*f.dest = v
		}
	}
	a.Selected["runtime-dir"] = a.RuntimeDir != ""
	a.Selected["claude-dir"] = a.ClaudeDir != ""
	a.Selected["codex-home"] = a.CodexHome != ""
	a.Selected["codex-bin"] = a.CodexBin != ""
	if a.RuntimeDir != "" {
		fmt.Fprintln(p.out, "Changing runtime directory selects different tokens, host identity and trust state; existing pairings remain in the old directory. Reinstall the service only with matching configuration.")
	}
	return a, nil
}

func defaultString(value, fallback string) string {
	if value == "" || value == "0" {
		return fallback
	}
	return value
}

// confirmExisting shows an existing file and asks whether to replace it. The
// default keeps it untouched.
func confirmExisting(p prompter, path string) (replace bool, err error) {
	if body, err := os.ReadFile(path); err == nil && len(body) <= 64*1024 {
		fmt.Fprintf(p.out, "\n%s already exists:\n%s\n", path, body)
	} else {
		fmt.Fprintf(p.out, "\n%s already exists.\n", path)
	}
	ans, err := p.ask("Keep it, or replace it (the old file is backed up)? keep/replace", "keep")
	if err != nil {
		return false, err
	}
	return strings.EqualFold(ans, "replace"), nil
}

// tailscaleCheck bounds the advisory Tailscale probe.
func tailscaleCheck(check func(context.Context) error) func() error {
	return func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return check(ctx)
	}
}
