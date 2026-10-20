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

// configAnswers is what the setup wizard (or --network flags) decided. Only
// answered settings are written; everything else keeps its default.
type configAnswers struct {
	Hostname   string
	Network    config.Network
	RuntimeDir string
	ClaudeDir  string
	CodexHome  string
	CodexBin   string
}

// fileDoc mirrors the TOML schema with omitempty so only answered keys appear.
// It deliberately has no token field: tokens never enter configuration.
type fileDoc struct {
	Hostname   string `toml:"hostname,omitempty"`
	RuntimeDir string `toml:"runtime_dir,omitempty"`
	Claude     *struct {
		ConfigDir string `toml:"config_dir,omitempty"`
	} `toml:"claude,omitempty"`
	Codex *struct {
		Bin  string `toml:"bin,omitempty"`
		Home string `toml:"home,omitempty"`
	} `toml:"codex,omitempty"`
	Network struct {
		Mode              string   `toml:"mode"`
		BindAddr          string   `toml:"bind_addr,omitempty"`
		AdvertiseAddr     string   `toml:"advertise_addr,omitempty"`
		Join              []string `toml:"join,omitempty"`
		MDNS              *bool    `toml:"mdns,omitempty"`
		GossipPort        *int     `toml:"gossip_port,omitempty"`
		DiscoveryInterval string   `toml:"discovery_interval,omitempty"`
	} `toml:"network"`
}

// render returns the config file body.
func (a configAnswers) render() (string, error) {
	var d fileDoc
	var err error
	d.Hostname, err = config.NormalizeHostname(a.Hostname)
	if err != nil {
		return "", err
	}
	d.RuntimeDir = a.RuntimeDir
	if a.ClaudeDir != "" {
		d.Claude = &struct {
			ConfigDir string `toml:"config_dir,omitempty"`
		}{a.ClaudeDir}
	}
	if a.CodexBin != "" || a.CodexHome != "" {
		d.Codex = &struct {
			Bin  string `toml:"bin,omitempty"`
			Home string `toml:"home,omitempty"`
		}{a.CodexBin, a.CodexHome}
	}
	n := a.Network
	if n.Mode == "" {
		n.Mode = config.ModeLocal
	}
	d.Network.Mode, d.Network.BindAddr, d.Network.AdvertiseAddr = n.Mode, n.BindAddr, n.AdvertiseAddr
	d.Network.Join, d.Network.MDNS = n.Join, n.MDNS
	if n.GossipPort != 0 {
		d.Network.GossipPort = &n.GossipPort
	}
	d.Network.DiscoveryInterval = n.DiscoveryInterval
	var b bytes.Buffer
	b.WriteString("# Written by `rhizome setup`; see the README \"Configuration\" section.\n")
	b.WriteString("# Unset keys use their defaults. Flags and RHIZOME_* variables override this file.\n")
	b.WriteString("# The cluster token is never stored here; see `rhizome token`.\n\n")
	if err := toml.NewEncoder(&b).Encode(d); err != nil {
		return "", err
	}
	return b.String(), nil
}

// writeConfig validates body with the real resolver, then installs it at path
// with mode 0600, creating the parent directory privately. An existing file is
// only replaced when replace is set, and is first moved aside to a timestamped
// backup, whose path is returned.
func writeConfig(path, body string, replace bool, now time.Time) (backup string, err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	_, statErr := os.Lstat(path)
	exists := statErr == nil
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
		if err := os.Rename(path, backup); err != nil {
			return "", err
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

func (p prompter) list(question string) ([]string, error) {
	ans, err := p.ask(question+" (comma-separated, Enter for none)", "")
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
	var a configAnswers
	fmt.Fprint(p.out, `
How will Rhizome be used on this device?
  1) local      this device only (loopback; nothing is exposed)  [default]
  2) lan        peer with other devices on your local network
  3) tailscale  peer with devices on your tailnet via the local Tailscale daemon
`)
	for a.Network.Mode == "" {
		ans, err := p.ask("Installation type", "local")
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
	n := &a.Network
	switch n.Mode {
	case config.ModeLAN, config.ModeTailscale:
		fmt.Fprintln(p.out, "\nNote: this makes the device reachable by peers that hold the cluster token (see `rhizome token`).")
		fmt.Fprintln(p.out, "The token is managed separately and is never written to this file.")
	}
	var err error
	switch n.Mode {
	case config.ModeLAN:
		if n.BindAddr, err = p.ask("Listen address (private IP; Enter for all interfaces)", ""); err != nil {
			return a, err
		}
		if n.AdvertiseAddr, err = p.ask("Address peers should dial (Enter for the first private interface)", ""); err != nil {
			return a, err
		}
		if n.Join, err = p.list("Gossip host:port seeds to join"); err != nil {
			return a, err
		}
		mdns, err := p.yesNo("Advertise and browse _rhizome._tcp over mDNS", true)
		if err != nil {
			return a, err
		}
		if !mdns {
			n.MDNS = &mdns
		}
	case config.ModeTailscale:
		if err := tailscale(); err != nil {
			fmt.Fprintf(p.out, "Warning: %v\nThe configuration will still be written, but `rhizome mcp` will refuse to start in this mode until Tailscale is running.\n", err)
		}
		port, err := p.ask("Well-known gossip port", "7946")
		if err != nil {
			return a, err
		}
		if port != "7946" {
			if n.GossipPort, err = strconv.Atoi(port); err != nil {
				return a, fmt.Errorf("gossip port %q: %w", port, err)
			}
		}
		interval, err := p.ask("Tailnet peer discovery interval", "20s")
		if err != nil {
			return a, err
		}
		if interval != "20s" {
			n.DiscoveryInterval = interval
		}
		if n.Join, err = p.list("Extra gossip host:port seeds to join (MagicDNS names allowed)"); err != nil {
			return a, err
		}
	}
	for {
		value, err := p.ask("Hostname override (blank uses OS hostname and clears any previous override)", "")
		if err != nil {
			return a, err
		}
		a.Hostname, err = config.NormalizeHostname(value)
		if err == nil {
			break
		}
		fmt.Fprintln(p.out, err)
	}
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
	return a, nil
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
