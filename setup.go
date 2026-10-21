package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"rhizome/internal/channel"
	"rhizome/internal/config"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"
)

// skillFS embeds the collaboration skill so `rhizome setup` can install it
// without a checkout. Add further skill files under skills/rhizome-collab and
// they are copied as well.
//
//go:embed skills/rhizome-collab
var skillFS embed.FS

const skillName = "rhizome-collab"

// openCodePlugin is the OpenCode relay plugin; it must match this binary's
// bridge protocol, so setup reinstalls it on every upgrade.
//
//go:embed plugins/opencode/rhizome.js
var openCodePlugin []byte

// openCodeTUIPlugin is the display-only OpenCode TUI plugin that shows a toast
// for Rhizome peer deliveries, which the TUI otherwise hides as synthetic
// input. OpenCode loads TUI plugins from <config>/plugins/<name>/tui.js.
//
//go:embed plugins/opencode/rhizome-tui.js
var openCodeTUIPlugin []byte

// agent is a coding agent Rhizome knows how to serve and where its user-scope
// skill, MCP server registration and hooks live.
type agent struct {
	Name     string   // CLI/flag identifier: claude, codex, opencode, cursor
	Label    string   // human-readable name
	Skills   string   // directory that holds <skill>/SKILL.md
	Plugin   string   // OpenCode plugin destination; empty for other agents
	TUI      string   // OpenCode TUI plugin destination; empty for other agents
	MCP      string   // file holding the user-scope MCP server registration
	Hooks    string   // file holding the user-scope hooks
	Binaries []string // executables that indicate the agent is installed
	Detected bool
	Reason   string // why it was (not) detected
}

// detectAgents reports the agents Rhizome can integrate with. Detection uses the
// same configuration directories the MCP integration documents, plus the agent
// binary on PATH. Undetected agents are still returned so --agents can force them.
// Destinations follow the effective configuration: Claude's config directory
// (CLAUDE_CONFIG_DIR) and Codex's home (CODEX_HOME).
func detectAgents(c config.Config, lookPath func(string) (string, error)) []agent {
	home, _ := os.UserHomeDir()
	claudeDir, codexDir, openCodeDir, cursorDir := c.Claude.ConfigDir, c.Codex.Home, openCodeConfigDir(os.Getenv, os.UserHomeDir), c.Cursor.ConfigDir
	agents := []agent{
		{Name: "claude", Label: "Claude Code", Skills: filepath.Join(claudeDir, "skills"), MCP: claudeGlobalConfig(claudeDir, home), Hooks: filepath.Join(claudeDir, "settings.json"), Binaries: []string{"claude"}},
		{Name: "codex", Label: "Codex", Skills: filepath.Join(codexDir, "skills"), MCP: filepath.Join(codexDir, "config.toml"), Hooks: filepath.Join(codexDir, "hooks.json"), Binaries: []string{"codex"}},
		{Name: "opencode", Label: "OpenCode", Skills: filepath.Join(openCodeDir, "skills"), Plugin: filepath.Join(openCodeDir, "plugins", "rhizome.js"), TUI: filepath.Join(openCodeDir, "plugins", "rhizome-tui", "tui.js"), Binaries: []string{"opencode"}},
		// Setup installs only the skill for Cursor; the user merges examples/cursor/ into mcp.json and hooks.json.
		// The Cursor CLI binary is `agent` (formerly `cursor-agent`); `cursor` is the IDE launcher.
		{Name: "cursor", Label: "Cursor", Skills: filepath.Join(cursorDir, "skills"), Binaries: []string{"cursor", "agent", "cursor-agent"}},
	}
	for i := range agents {
		a := &agents[i]
		dir := filepath.Dir(a.Skills)
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			a.Detected, a.Reason = true, "found "+dir
			continue
		}
		for _, bin := range a.Binaries {
			if p, err := lookPath(bin); err == nil {
				a.Detected, a.Reason = true, "found "+p+" on PATH"
				break
			}
		}
		if !a.Detected {
			a.Reason = "no " + dir + " and no " + strings.Join(a.Binaries, "/") + " on PATH"
		}
	}
	return agents
}

// openCodeConfigDir is OpenCode's global configuration directory:
// $XDG_CONFIG_HOME/opencode, or ~/.config/opencode. Rhizome only adds its skill
// and plugin there and never edits OpenCode's own configuration files.
func openCodeConfigDir(getenv func(string) string, home func() (string, error)) string {
	base := getenv("XDG_CONFIG_HOME")
	if base == "" {
		h, err := home()
		if err != nil {
			h = "."
		}
		base = filepath.Join(h, ".config")
	}
	return filepath.Join(base, "opencode")
}

// installSkill writes the embedded skill into <skillsDir>/<skillName>. It returns
// "installed", "updated", "unchanged" or "skipped". An existing symlink is left
// alone: it already points at a checkout the user manages.
func installSkill(skillsDir string) (string, error) {
	dest := filepath.Join(skillsDir, skillName)
	if st, err := os.Lstat(dest); err == nil && st.Mode()&os.ModeSymlink != 0 {
		target, _ := os.Readlink(dest)
		return "skipped (symlink to " + target + ")", nil
	} else if err == nil && !st.IsDir() {
		return "", fmt.Errorf("%s exists and is not a directory", dest)
	}
	root := "skills/" + skillName
	status := "unchanged"
	err := fs.WalkDir(skillFS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.FromSlash(p))
		out := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		want, err := skillFS.ReadFile(p)
		if err != nil {
			return err
		}
		have, readErr := os.ReadFile(out)
		switch {
		case readErr == nil && bytes.Equal(have, want):
			return nil
		case readErr == nil:
			status = "updated"
		case errors.Is(readErr, fs.ErrNotExist):
			if status == "unchanged" {
				status = "installed"
			}
		default:
			return readErr
		}
		return os.WriteFile(out, want, 0o644)
	})
	if err != nil {
		return "", err
	}
	return status, nil
}

// installPlugin writes an embedded OpenCode plugin file to dest and returns
// "installed", "updated", "unchanged" or "skipped (symlink to …)".
func installPlugin(dest string, content []byte) (string, error) {
	if st, err := os.Lstat(dest); err == nil && st.Mode()&os.ModeSymlink != 0 {
		target, _ := os.Readlink(dest)
		return "skipped (symlink to " + target + ")", nil
	}
	have, err := os.ReadFile(dest)
	switch {
	case err == nil && bytes.Equal(have, content):
		return "unchanged", nil
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return "", err
	}
	status := "updated"
	if err != nil {
		status = "installed"
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(dest, content, 0o644); err != nil {
		return "", err
	}
	return status, nil
}

// pickAgents is a minimal checkbox prompt. in must deliver raw key bytes (the
// caller puts the terminal in raw mode). Keys: up/down or j/k move, space
// toggles, a toggles all, enter confirms, q or Esc or Ctrl-C aborts.
func pickAgents(in io.Reader, out io.Writer, agents []agent, checked []bool) ([]bool, error) {
	n := len(agents)
	cur := 0
	draw := func() {
		for i, a := range agents {
			cursor, box := "  ", "[ ]"
			if i == cur {
				cursor = "> "
			}
			if checked[i] {
				box = "[x]"
			}
			note := ""
			if !a.Detected {
				note = "  (not detected)"
			}
			fmt.Fprintf(out, "\r\x1b[2K%s%s %s  %s%s\r\n", cursor, box, a.Label, a.Skills, note)
		}
	}
	fmt.Fprint(out, "Install Rhizome (MCP server, binding hooks and the /rhizome-collab skill) for which agents?\r\n")
	fmt.Fprint(out, "space: toggle  a: all/none  enter: confirm  q: cancel\r\n")
	draw()
	keys := bufio.NewReader(in)
	for {
		key, err := readKey(keys)
		if err != nil {
			return nil, err
		}
		switch key {
		case "up", "k":
			cur = (cur + n - 1) % n
		case "down", "j":
			cur = (cur + 1) % n
		case " ":
			checked[cur] = !checked[cur]
		case "a":
			all := true
			for _, c := range checked {
				all = all && c
			}
			for i := range checked {
				checked[i] = !all
			}
		case "\r", "\n":
			return checked, nil
		case "q", "esc", "\x03":
			return nil, errors.New("cancelled")
		default:
			continue
		}
		fmt.Fprintf(out, "\x1b[%dA", n)
		draw()
	}
}

// readKey returns one key press: a single byte as a string, or "up", "down" or
// "esc" for the arrow and escape keys. Other CSI sequences are returned verbatim
// and ignored by the caller. A lone Esc is only recognized when no bytes follow
// it in the buffer, which is how terminals deliver it in raw mode.
func readKey(r *bufio.Reader) (string, error) {
	b, err := r.ReadByte()
	if err != nil {
		return "", err
	}
	if b != '\x1b' {
		return string(b), nil
	}
	if r.Buffered() == 0 {
		return "esc", nil
	}
	next, err := r.ReadByte()
	if err != nil {
		return "", err
	}
	if next != '[' {
		return "esc", nil
	}
	final, err := r.ReadByte()
	if err != nil {
		return "", err
	}
	switch final {
	case 'A':
		return "up", nil
	case 'B':
		return "down", nil
	}
	return "\x1b[" + string(final), nil
}

// parseAgentFlag resolves --agents into a selection over agents.
func parseAgentFlag(value string, agents []agent) ([]bool, error) {
	checked := make([]bool, len(agents))
	if value == "all" {
		for i := range checked {
			checked[i] = true
		}
		return checked, nil
	}
	known := map[string]int{}
	for i, a := range agents {
		known[a.Name] = i
	}
	for _, name := range strings.Split(value, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		i, ok := known[name]
		if !ok {
			names := make([]string, 0, len(known))
			for k := range known {
				names = append(names, k)
			}
			sort.Strings(names)
			return nil, fmt.Errorf("unknown agent %q (known: %s, all)", name, strings.Join(names, ", "))
		}
		checked[i] = true
	}
	return checked, nil
}

func setup(args []string) error {
	opts := flag.NewFlagSet("setup", flag.ContinueOnError)
	agentsFlag := opts.String("agents", "", "comma-separated agents to install for (claude, codex, opencode, cursor) or \"all\": installs the skill, and for Claude and Codex also registers the MCP server and hooks at user scope (OpenCode: skill and plugins; its MCP entry is added by hand, see docs/opencode.md; Cursor: skill only, merge examples/cursor by hand, see docs/cursor.md); omit for an interactive picker")
	config.Bind(opts, "runtime-dir", "gateway-mode", "gateway-listen", "gateway-advertise")
	opts.Lookup("gateway-mode").Usage = "configure service-owned gateway pairing: off, lan or tailscale; requires local MCP networking"
	opts.Lookup("gateway-listen").Usage = "gateway concrete listen IP:port; blank selects automatically"
	opts.Lookup("gateway-advertise").Usage = "gateway reachable IP:port; blank uses listen endpoint"
	opts.Lookup("runtime-dir").Usage = "persist runtime directory (changing it selects different token, host identity and pairing state)"
	plain := opts.Bool("plain", false, "use the original line/picker prompts instead of Bubble Tea")
	list := opts.Bool("list", false, "print detected agents and exit")
	hostname := opts.String("hostname", "", "with --network or --gateway-mode: device label override; blank uses the OS hostname")
	network := opts.String("network", "", "write the configuration non-interactively: local, lan or tailscale; omit for the interactive questions")
	var join listFlag
	opts.Var(&join, "join", "with --network lan|tailscale: gossip host:port seed to join; repeatable")
	bindAddr := opts.String("bind-addr", "", "with --network lan: listen address")
	advertiseAddr := opts.String("advertise-addr", "", "with --network lan: address peers dial")
	mdns := opts.Bool("mdns", true, "with --network lan: set false to disable mDNS")
	gossipPort := opts.Int("gossip-port", 0, "with --network tailscale: well-known gossip port (default 7946)")
	discovery := opts.String("discovery-interval", "", "with --network tailscale: peer discovery interval (default 20s)")
	replaceConfig := opts.Bool("replace-config", false, "with --network or --gateway-mode: update selected settings, preserving others (old file backed up)")
	if err := opts.Parse(args); err != nil {
		return err
	}
	if opts.NArg() != 0 {
		return fmt.Errorf("unexpected setup arguments")
	}
	visited := map[string]bool{}
	opts.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	noninteractive := visited["network"] || visited["gateway-mode"]
	if !noninteractive {
		for _, name := range []string{"hostname", "join", "bind-addr", "advertise-addr", "mdns", "gossip-port", "discovery-interval", "replace-config", "gateway-listen", "gateway-advertise", "runtime-dir"} {
			if visited[name] {
				return fmt.Errorf("--%s requires --network or --gateway-mode", name)
			}
		}
	}
	c, path, explicit, err := resolveForSetup(opts, false)
	if err != nil {
		return err
	}
	tty := usableTerminal()
	agents := detectAgents(c, exec.LookPath)
	if *list {
		for _, a := range agents {
			state := "not detected"
			if a.Detected {
				state = "detected"
			}
			fmt.Printf("%-8s %-12s %s (%s)\n", a.Name, state, a.Skills, a.Reason)
			if a.MCP != "" {
				fmt.Printf("%-8s %-12s mcp %s; hooks %s\n", "", "", a.MCP, a.Hooks)
			} else if a.Plugin == "" {
				fmt.Printf("%-8s %-12s skill only; merge examples/%s into the agent's own configuration\n", "", "", a.Name)
			}
		}
		return nil
	}
	var answers *configAnswers
	if noninteractive {
		a := configAnswers{
			Selected:   visited,
			Gateway:    config.Gateway{Mode: opts.Lookup("gateway-mode").Value.String(), Listen: opts.Lookup("gateway-listen").Value.String(), Advertise: opts.Lookup("gateway-advertise").Value.String()},
			RuntimeDir: opts.Lookup("runtime-dir").Value.String(),
			Hostname:   *hostname,
			Network:    config.Network{Mode: *network, BindAddr: *bindAddr, AdvertiseAddr: *advertiseAddr, Join: join, GossipPort: *gossipPort, DiscoveryInterval: *discovery}}
		if visited["mdns"] {
			a.Network.MDNS = mdns
		}
		if err := a.Network.Validate(); err != nil {
			return fmt.Errorf("--network %s: %w", *network, err)
		}
		answers = &a
	}
	var checked []bool
	if tty && !*plain && (*agentsFlag == "" || answers == nil) {
		if *agentsFlag != "" {
			checked, err = parseAgentFlag(*agentsFlag, agents)
			if err != nil {
				return err
			}
		}
		m := newSetupModel(c, path, agents, checked, answers, *replaceConfig)
		m.explicit = explicit
		m.reload = func() (config.Config, error) {
			c, _, _, err := resolveForSetup(opts, true)
			return c, err
		}
		return runSetupTUI(m)
	}
	configOnly := false
	switch {
	case *agentsFlag != "":
		if checked, err = parseAgentFlag(*agentsFlag, agents); err != nil {
			return err
		}
	case noninteractive && !tty:
		checked, configOnly = make([]bool, len(agents)), true
	case tty:
		checked = make([]bool, len(agents))
		for i, a := range agents {
			checked[i] = a.Detected
		}
		state, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			return err
		}
		checked, err = pickAgents(os.Stdin, os.Stdout, agents, checked)
		_ = term.Restore(int(os.Stdin.Fd()), state)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("stdin is not a terminal; pass --agents claude,codex,opencode,cursor or --agents all")
	}
	selected := false
	for _, ok := range checked {
		selected = selected || ok
	}
	if !selected && !configOnly {
		fmt.Println("nothing selected; no agent installed")
	}
	// Configuration first: directory answers decide where agents are installed.
	// One buffered reader serves the configuration questions and the service
	// question so no answer is lost between them.
	stdin := bufio.NewReader(os.Stdin)
	wizard, err := setupConfig(c, path, answers, *replaceConfig, tty, stdin, os.Stdout, os.Stderr)
	if err != nil {
		return err
	}
	if selected {
		// Re-read the (possibly new) file so the installs see its directories.
		if c, path, explicit, err = resolveForSetup(opts, true); err != nil {
			return err
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if exe, err = filepath.Abs(exe); err != nil {
			return err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		in := agentInstaller{exe: exe, home: home, c: c, explicit: explicit, lookPath: exec.LookPath, now: time.Now, out: os.Stdout}
		for i, a := range detectAgents(c, exec.LookPath) {
			if !checked[i] {
				continue
			}
			if err := in.install(a); err != nil {
				return err
			}
		}
	}
	if !wizard {
		return nil
	}
	return offerService(prompter{in: stdin, out: os.Stdout}, path, os.Stdout, plainServiceInstaller)
}

// resolveForSetup resolves configuration like every other command, except that
// an explicitly selected file that does not exist yet is allowed: setup is the
// command that creates it. explicit reports a --config/RHIZOME_CONFIG selection.
// The first resolution logs like other commands; later ones (after the file was
// written) are quiet. The result selects agent installation directories only:
// setupConfig reloads the stored file for editor defaults, so shell overrides
// are never persisted.
func resolveForSetup(opts *flag.FlagSet, quiet bool) (config.Config, string, bool, error) {
	flags := map[string]string{}
	opts.Visit(func(f *flag.Flag) { flags[f.Name] = f.Value.String() })
	path, explicit, err := config.Target(flags, os.Getenv)
	if err != nil {
		return config.Config{}, "", false, err
	}
	// Only the selected file is read, so a new destination ignores the default
	// file, and a missing explicit one is allowed.
	c, err := config.LoadForSetup(path, func(key string) string {
		// Agent installation honors native agent directory overrides; no other
		// shell override may reach the stored editor defaults.
		if key == "CLAUDE_CONFIG_DIR" || key == "CODEX_HOME" {
			return os.Getenv(key)
		}
		return ""
	})
	if err == nil && !quiet {
		slog.Info("configuration", "path", c.Path, "found", c.Found)
	}
	return c, path, explicit, err
}

// setupConfig edits the configuration file. answers is set by setup flags;
// otherwise a terminal gets the interactive questions and anything else leaves
// configuration alone and says so. It reports whether the interactive questions
// ran and a valid file exists at path, whether created, replaced, or kept; the
// caller then offers the background service once agents are installed.
func setupConfig(c config.Config, path string, answers *configAnswers, replace, tty bool, in io.Reader, out, errOut io.Writer) (bool, error) {
	c, err := config.LoadForSetup(path, func(string) string { return "" })
	if err != nil {
		return false, err
	}
	wizard := answers == nil
	p := prompter{in: bufio.NewReader(in), out: out}
	_, statErr := os.Lstat(path)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return false, statErr
	}
	stored, err := storedConfig(path)
	if err != nil {
		return false, err
	}
	if wizard {
		if !tty {
			fmt.Fprintln(errOut, "configuration not written: no terminal for the questions; pass --network local|lan|tailscale or --gateway-mode off|lan|tailscale")
			return false, nil
		}
		if exists {
			if replace, err = confirmExisting(p, path); err != nil {
				return false, err
			}
		}
		if !exists || replace {
			a, err := askConfig(p, c, tailscaleCheck(channel.CheckTailscale))
			if err != nil {
				return false, err
			}
			answers = &a
		}
	} else if exists && !replace {
		fmt.Fprintf(errOut, "configuration not written: %s already exists; pass --replace-config to back it up and replace it\n", path)
		return false, nil
	}
	if answers == nil {
		fmt.Fprintf(out, "kept existing %s\n", path)
		return wizard, nil
	}
	answers.Stored = stored
	body, err := answers.render()
	if err != nil {
		return false, err
	}
	backup, err := writeConfig(path, body, exists && replace, time.Now())
	if err != nil {
		return false, err
	}
	if backup != "" {
		fmt.Fprintf(out, "backed up previous configuration to %s\n", backup)
	}
	saved, err := config.Resolve(map[string]string{"config": path}, func(string) string { return "" })
	if err != nil {
		return false, err
	}
	fmt.Fprintf(out, "wrote %s (network mode: %s; gateway mode: %s)\n", path, defaultString(saved.Network.Mode, "local"), saved.Gateway.Mode)
	fmt.Fprintln(out, "Restart the service and MCP processes to apply changes. Configuration saved; peer connectivity has not been verified. History policy is unchanged.")
	if answers.RuntimeDir != "" && answers.RuntimeDir != c.RuntimeDir {
		fmt.Fprintln(out, "Runtime directory changed: existing tokens, host identity and pairings remain in the old directory; keep service and MCP runtime scopes aligned.")
	}
	if !wizard && (saved.Gateway.Mode == "tailscale" || saved.Network.Mode == "tailscale") {
		fmt.Fprintln(out, "Tailscale requires a running CLI/daemon with an IP; startup fails closed if unavailable.")
	}
	return wizard, nil
}

// offerService asks the opt-in service question after a valid configuration
// exists at path. Declining leaves the service untouched; accepting installs
// and starts it and reports success only once the observer is running.
func offerService(p prompter, path string, out io.Writer, install serviceInstaller) error {
	accept, err := p.yesNo("Install and start the background service now", false)
	if err != nil {
		return err
	}
	if !accept {
		fmt.Fprintf(out, "background service unchanged; run `rhizome service install --config %s` later\n", path)
		return nil
	}
	outcome, err := install(context.Background(), path)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "background service installed and running (configuration %s, runtime %s)\n", path, outcome.Runtime)
	return nil
}
