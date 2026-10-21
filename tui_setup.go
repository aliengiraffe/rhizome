package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"rhizome/internal/channel"
	"rhizome/internal/config"
	"rhizome/internal/service"
)

type wizardField struct {
	key, label string
	choices    []string
}
type setupResult struct {
	steps []string
	err   error
}
type probeResult struct{ err error }
type setupModel struct {
	work                                *uiWork
	c                                   config.Config
	path                                string
	agents                              []agent
	checked                             []bool
	fixedAgents                         bool
	fixedAnswers                        *configAnswers
	exists, replace                     bool
	values                              map[string]string
	step, choice, offset, width, height int
	busy, done, applied                 bool
	err                                 error
	warning                             string
	result                              setupResult
	installService                      func(context.Context, string) error
	apply                               func(context.Context) setupResult
	probe                               func(context.Context) error
	explicit                            bool                                        // configuration selected with --config/RHIZOME_CONFIG
	reload                              func() (config.Config, error)               // re-resolves after the file is written
	installAgent                        func(config.Config, agent, io.Writer) error // skill, MCP registration and hooks for one agent
}

func newSetupModel(c config.Config, path string, agents []agent, checked []bool, answers *configAnswers, replace bool) *setupModel {
	_, err := os.Lstat(path)
	m := &setupModel{work: newUIWork(), c: c, path: path, agents: agents, fixedAgents: checked != nil, fixedAnswers: answers, exists: err == nil, replace: replace, values: map[string]string{"existing": "keep", "mode": "local", "mdns": "yes", "port": "7946", "interval": "20s", "custom": "no", "service": "no"}, width: 80, height: 24, probe: channel.CheckTailscale}
	m.checked = append([]bool(nil), checked...)
	if checked == nil {
		m.checked = make([]bool, len(agents))
		for i, a := range agents {
			m.checked[i] = a.Detected
		}
	}
	m.installService = func(ctx context.Context, path string) error {
		c, err := config.Resolve(map[string]string{"config": path}, os.Getenv)
		if err != nil {
			return err
		}
		return service.ManageQuiet(ctx, "install", service.FromConfig(c))
	}
	m.apply = m.applyAnswers
	m.installAgent = m.defaultAgentInstaller
	return m
}
func (m *setupModel) fields() []wizardField {
	var f []wizardField
	if !m.fixedAgents {
		f = append(f, wizardField{"agents", "Select skills to install", nil})
	}
	if m.fixedAnswers == nil {
		if m.exists {
			f = append(f, wizardField{"existing", "Existing config: keep or back up and replace?", []string{"keep", "replace"}})
		}
		if !m.exists || m.values["existing"] == "replace" {
			f = append(f, wizardField{"mode", "Network mode", []string{"local", "lan", "tailscale"}})
			switch m.values["mode"] {
			case "lan":
				f = append(f, wizardField{"bind", "Listen address (blank: all interfaces)", nil}, wizardField{"advertise", "Address peers dial (blank: first private interface)", nil}, wizardField{"join_lan", "Gossip host:port seeds (comma-separated, blank: none)", nil}, wizardField{"mdns", "Advertise and browse mDNS?", []string{"yes", "no"}})
			case "tailscale":
				f = append(f, wizardField{"port", "Well-known gossip port", nil}, wizardField{"interval", "Tailnet discovery interval", nil}, wizardField{"join_tailscale", "Extra gossip host:port seeds (comma-separated)", nil})
			}
			f = append(f, wizardField{"hostname", "Hostname override (blank: OS hostname)", nil}, wizardField{"custom", "Customize directories or Codex binary?", []string{"no", "yes"}})
			if m.values["custom"] == "yes" {
				f = append(f, wizardField{"runtime", "Runtime directory (blank: " + m.c.RuntimeDir + ")", nil}, wizardField{"claude", "Claude directory (blank: " + m.c.Claude.ConfigDir + ")", nil}, wizardField{"codex", "Codex home (blank: " + m.c.Codex.Home + ")", nil}, wizardField{"bin", "Codex executable (blank: " + m.c.Codex.Bin + ")", nil})
			}
			f = append(f, wizardField{"service", "Install background service after writing config?", []string{"no", "yes"}})
		}
	}
	return append(f, wizardField{"review", "Review / Apply", nil})
}
func (m *setupModel) answers() (*configAnswers, error) {
	if m.fixedAnswers != nil {
		return m.fixedAnswers, nil
	}
	if m.exists && m.values["existing"] == "keep" {
		return nil, nil
	}
	a := &configAnswers{Hostname: m.values["hostname"], Network: config.Network{Mode: m.values["mode"]}}
	n := &a.Network
	if n.Mode == "lan" || n.Mode == "tailscale" {
		for _, seed := range strings.Split(m.values["join_"+n.Mode], ",") {
			if seed = strings.TrimSpace(seed); seed != "" {
				n.Join = append(n.Join, seed)
			}
		}
	}
	if n.Mode == "lan" {
		n.BindAddr = m.values["bind"]
		n.AdvertiseAddr = m.values["advertise"]
		if m.values["mdns"] == "no" {
			v := false
			n.MDNS = &v
		}
	}
	if n.Mode == "tailscale" {
		port, err := strconv.Atoi(m.values["port"])
		if err != nil || port < 1 || port > 65535 {
			return nil, errors.New("gossip port must be 1-65535")
		}
		if port != 7946 {
			n.GossipPort = port
		}
		if m.values["interval"] != "20s" {
			n.DiscoveryInterval = m.values["interval"]
		}
	}
	if m.values["custom"] == "yes" {
		for _, v := range []struct {
			key, def string
			dest     *string
		}{{"runtime", m.c.RuntimeDir, &a.RuntimeDir}, {"claude", m.c.Claude.ConfigDir, &a.ClaudeDir}, {"codex", m.c.Codex.Home, &a.CodexHome}, {"bin", m.c.Codex.Bin, &a.CodexBin}} {
			if value := m.values[v.key]; value != "" && value != v.def {
				*v.dest = value
			}
		}
	}
	return a, nil
}
func (m *setupModel) validate() error {
	a, err := m.answers()
	if err != nil || a == nil {
		return err
	}
	body, err := a.render()
	if err != nil {
		return err
	}
	_, err = config.Preview(body)
	return err
}
func (m *setupModel) validateField(key string) error {
	a := configAnswers{Network: config.Network{Mode: "local"}}
	switch key {
	case "hostname":
		a.Hostname = m.values[key]
	case "runtime":
		a.RuntimeDir = m.values[key]
	case "claude":
		a.ClaudeDir = m.values[key]
	case "codex":
		a.CodexHome = m.values[key]
	case "bin":
		a.CodexBin = m.values[key]
	case "port":
		port, err := strconv.Atoi(m.values[key])
		if err != nil || port < 1 || port > 65535 {
			return errors.New("gossip port must be 1-65535")
		}
	case "interval":
		d, err := time.ParseDuration(m.values[key])
		if err != nil || d <= 0 {
			return errors.New("discovery interval must be positive (for example 20s)")
		}
	}
	body, err := a.render()
	if err != nil {
		return err
	}
	_, err = config.Preview(body)
	return err
}
func (m *setupModel) Init() tea.Cmd { return nil }
func (m *setupModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = v.Width
		m.height = v.Height
	case probeResult:
		m.busy = false
		if v.err != nil {
			m.warning = "Tailscale advisory: " + safeText(v.err.Error()) + ". Setup can continue; MCP requires running Tailscale."
		} else {
			m.warning = "Tailscale is available."
		}
	case setupResult:
		m.busy = false
		m.done = true
		m.result = v
		m.err = v.err
	case tea.KeyMsg:
		key := v.String()
		if key == "ctrl+c" || key == "esc" {
			m.work.cancel()
			return m, tea.Quit
		}
		if m.done {
			if key == "enter" || key == "q" {
				return m, tea.Quit
			}
			return m, nil
		}
		if m.busy {
			return m, nil
		}
		fields := m.fields()
		field := fields[m.step]
		if key == "pgdown" {
			m.offset += max(1, m.height-4)
			return m, nil
		}
		if key == "pgup" {
			m.offset = max(0, m.offset-max(1, m.height-4))
			return m, nil
		}
		if key == "shift+tab" {
			m.step = max(0, m.step-1)
			m.choice = 0
			m.offset = 0
			m.err = nil
			return m, nil
		}
		if key == "enter" {
			if field.key == "review" {
				if err := m.validate(); err != nil {
					m.err = err
					return m, nil
				}
				m.busy = true
				m.applied = true
				return m, m.work.command(func(ctx context.Context) tea.Msg { r := m.apply(ctx); m.result = r; return r })
			}
			// Validate the staged configuration as each answer is accepted.
			if field.key != "agents" && field.key != "existing" {
				if err := m.validateField(field.key); err != nil {
					m.err = err
					return m, nil
				}
			}
			m.step++
			m.choice = 0
			m.offset = 0
			m.err = nil
			if field.key == "mode" && m.values["mode"] == "tailscale" {
				m.busy = true
				return m, m.work.command(func(ctx context.Context) tea.Msg {
					ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
					defer cancel()
					return probeResult{m.probe(ctx)}
				})
			}
			return m, nil
		}
		if field.key == "agents" {
			switch key {
			case "down", "j":
				m.choice = min(m.choice+1, len(m.agents)-1)
			case "up", "k":
				m.choice = max(0, m.choice-1)
			case " ":
				m.checked[m.choice] = !m.checked[m.choice]
			case "a":
				all := true
				for _, v := range m.checked {
					all = all && v
				}
				for i := range m.checked {
					m.checked[i] = !all
				}
			}
		} else if len(field.choices) > 0 {
			index := 0
			for i, s := range field.choices {
				if s == m.values[field.key] {
					index = i
				}
			}
			switch key {
			case "down", "right", " ", "j":
				index = (index + 1) % len(field.choices)
			case "up", "left", "k":
				index = (index + len(field.choices) - 1) % len(field.choices)
			}
			m.values[field.key] = field.choices[index]
		} else if field.key != "review" {
			value := []rune(m.values[field.key])
			if key == "backspace" && len(value) > 0 {
				value = value[:len(value)-1]
			} else if key == "ctrl+u" {
				value = nil
			} else if v.Type == tea.KeyRunes && len(value)+len(v.Runes) <= 4096 {
				value = append(value, v.Runes...)
			}
			m.values[field.key] = string(value)
		}
	}
	return m, nil
}
func (m *setupModel) review() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Config: %s\n", safeText(m.path))
	for i, a := range m.agents {
		if m.checked[i] {
			fmt.Fprintf(&b, "Install skill: %s (%s)\n", a.Label, safeText(a.Skills))
		}
	}
	a, err := m.answers()
	if err != nil {
		return b.String() + safeText(err.Error())
	}
	if a == nil || m.fixedAnswers != nil && m.exists && !m.replace {
		b.WriteString("Keep existing configuration.\n")
	} else {
		body, err := a.render()
		if err != nil {
			return b.String() + safeText(err.Error())
		}
		if m.exists {
			b.WriteString("Back up and replace configuration.\n")
		}
		for _, line := range strings.Split(body, "\n") {
			if !strings.HasPrefix(line, "#") {
				b.WriteString(safeText(line) + "\n")
			}
		}
		fmt.Fprintf(&b, "Install service: %s\n", m.values["service"])
	}
	b.WriteString("Enter applies these changes. Esc cancels before any writes.")
	return b.String()
}
func (m *setupModel) View() string {
	field := m.fields()[m.step]
	body := safeText(field.label) + "\n"
	switch {
	case m.done:
		body = strings.Join(m.result.steps, "\n")
		if m.err == nil {
			body += "\nSetup completed. Enter exits."
		}
	case field.key == "review":
		body = m.review()
	case field.key == "agents":
		for i, a := range m.agents {
			cursor, box := " ", "[ ]"
			if i == m.choice {
				cursor = ">"
			}
			if m.checked[i] {
				box = "[x]"
			}
			body += fmt.Sprintf("%s %s %s — %s\n", cursor, box, a.Label, safeText(a.Skills))
		}
	case len(field.choices) > 0:
		for _, s := range field.choices {
			prefix := "  "
			if s == m.values[field.key] {
				prefix = "> "
			}
			body += prefix + s + "\n"
		}
	default:
		body += "> " + safeText(m.values[field.key]) + "_"
	}
	if m.values["mode"] != "local" {
		body += "\nNetwork access requires the separately managed cluster token."
	}
	if m.warning != "" {
		body += "\n" + m.warning
	}
	if m.err != nil {
		body = "Error: " + safeText(m.err.Error()) + "\n" + body
	}
	if m.busy {
		body = "Working… Esc cancels; completed steps remain.\n" + body
	}
	return uiView("Rhizome setup", body, "Esc: cancel | Enter: next/apply | Shift+Tab: back | arrows/space: select | PgUp/Dn: scroll | Ctrl+U: clear", m.width, m.height, m.offset)
}
func (m *setupModel) applyAnswers(ctx context.Context) setupResult {
	r := setupResult{}
	fail := func(err error) setupResult { r.err = err; return r }
	if err := m.validate(); err != nil {
		return fail(err)
	}
	a, err := m.answers()
	if err != nil {
		return fail(err)
	}
	// Configuration first: directory answers decide where agents are installed.
	wrote := false
	if a == nil || m.fixedAnswers != nil && m.exists && !m.replace {
		r.steps = append(r.steps, "Kept configuration")
	} else {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		body, err := a.render()
		if err != nil {
			return fail(err)
		}
		backup, err := writeConfig(m.path, body, m.exists && (m.replace || m.values["existing"] == "replace"), time.Now())
		if backup != "" {
			r.steps = append(r.steps, "Backed up "+safeText(backup))
		}
		if err != nil {
			return fail(err)
		}
		wrote = true
		r.steps = append(r.steps, "Wrote "+safeText(m.path))
	}
	if err := m.installAgents(ctx, &r, wrote); err != nil {
		return fail(err)
	}
	if wrote && m.values["service"] == "yes" {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		path, err := filepath.Abs(m.path)
		if err != nil {
			return fail(err)
		}
		if err := m.installService(ctx, path); err != nil {
			return fail(fmt.Errorf("service installation failed; completed steps remain: %w", err))
		}
		r.steps = append(r.steps, "Installed background service")
	}
	return r
}

// installAgents installs the skill, MCP registration and hooks for every checked
// agent, reporting each file through r.steps. When the configuration was just
// written it is re-read so the installs see its directories.
func (m *setupModel) installAgents(ctx context.Context, r *setupResult, reload bool) error {
	selected := false
	for _, ok := range m.checked {
		selected = selected || ok
	}
	if !selected {
		return nil
	}
	c, agents := m.c, m.agents
	if reload && m.reload != nil {
		var err error
		if c, err = m.reload(); err != nil {
			return err
		}
		agents = detectAgents(c, exec.LookPath)
	}
	for i, a := range agents {
		if !m.checked[i] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		var out bytes.Buffer
		err := m.installAgent(c, a, &out)
		for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			if line = strings.TrimSpace(line); line != "" && !strings.HasSuffix(line, ":") {
				r.steps = append(r.steps, a.Label+": "+safeText(line))
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// defaultAgentInstaller installs one agent with the running executable.
func (m *setupModel) defaultAgentInstaller(c config.Config, a agent, out io.Writer) error {
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
	return agentInstaller{exe: exe, home: home, c: c, explicit: m.explicit, lookPath: exec.LookPath, now: time.Now, out: out}.install(a)
}
func runSetupTUI(m *setupModel) error {
	_, err := tea.NewProgram(m, tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout), tea.WithAltScreen()).Run()
	m.work.close()
	if m.applied {
		for _, step := range m.result.steps {
			fmt.Println(step)
		}
		if m.result.err != nil {
			return m.result.err
		}
		if !m.done {
			return errors.New("setup interrupted; completed steps above were not rolled back")
		}
	} else {
		return errors.New("setup cancelled; no changes applied")
	}
	return err
}
