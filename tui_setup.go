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
)

type wizardField struct {
	key, label string
	choices    []string
}

// setupResult is what apply achieved. pairing names the runtime directory
// when the service and its gateway were confirmed ready, so the pairing phase
// may follow; it is blank whenever pairing must happen later.
type setupResult struct {
	steps   []string
	err     error
	pairing string
}
type probeResult struct{ report channel.TailscaleReport }
type pairingResult struct {
	lines []string
	err   error
}

// pairingPhase is the post-apply enrollment offer: it stages nothing and only
// calls the service's own invite/join through admin.
type pairingPhase struct {
	runtime string
	choice  int  // 0 invite, 1 join, 2 later
	editing bool // entering an invitation
	code    string
	busy    bool
	lines   []string // outcome shown on screen and printed on exit
	err     error    // last failed attempt; the menu stays open for a retry
	closed  bool     // finished or deferred
}

var pairingChoices = []string{"create an invitation for another device to join", "join with an invitation from another device", "later"}

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
	touched                             map[string]bool // fields the user edited, so recommendations stop moving them
	recommend                           bool            // the stored file has no pairing transport, so Tailscale may be suggested
	step, choice, offset, width, height int
	busy, done, applied                 bool
	err                                 error
	warning                             string
	result                              setupResult
	tailscale                           *channel.TailscaleReport // nil until the advisory probe answered
	probing, probed                     bool
	pair                                pairingPhase
	installService                      serviceInstaller
	apply                               func(context.Context) setupResult
	probe                               tailscaleProbe
	admin                               gatewayAdmin
	explicit                            bool                                        // configuration selected with --config/RHIZOME_CONFIG
	reload                              func() (config.Config, error)               // re-resolves after the file is written
	installAgent                        func(config.Config, agent, io.Writer) error // skill, MCP registration and hooks for one agent
}

func newSetupModel(c config.Config, path string, agents []agent, checked []bool, answers *configAnswers, replace bool) *setupModel {
	_, err := os.Lstat(path)
	connect, transport := storedConnection(c)
	values := map[string]string{"existing": "keep", "connect": connect, "transport": "lan", "endpoints": "automatic", "gw_listen": c.Gateway.Listen, "gw_advertise": c.Gateway.Advertise, "mode": "lan", "mdns": "yes", "port": "7946", "interval": "20s", "custom": "no", "service": "no", "updates": map[bool]string{true: "yes", false: "no"}[c.AutoUpdate.Enabled]}
	switch connect {
	case connectPaired:
		values["transport"] = transport
		if c.Gateway.Listen != "" || c.Gateway.Advertise != "" {
			values["endpoints"] = "custom"
		}
	case connectDirect:
		values["mode"] = transport
	}
	m := &setupModel{work: newUIWork(), c: c, path: path, agents: agents, fixedAgents: checked != nil, fixedAnswers: answers, exists: err == nil, replace: replace, values: values, touched: map[string]bool{}, recommend: connect != connectPaired, width: 80, height: 24, probe: channel.ProbeTailscale, admin: channel.GatewayAdmin}
	m.checked = append([]bool(nil), checked...)
	if checked == nil {
		m.checked = make([]bool, len(agents))
		for i, a := range agents {
			m.checked[i] = a.Detected
		}
	}
	m.installService = quietServiceInstaller
	m.apply = m.applyAnswers
	m.installAgent = m.defaultAgentInstaller
	return m
}

// asksConnection reports whether the wizard will ask the connection question.
func (m *setupModel) asksConnection() bool {
	return m.fixedAnswers == nil && (!m.exists || m.values["existing"] == "replace")
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
		if m.asksConnection() {
			f = append(f, wizardField{"connect", "How should this device connect?", connectChoices})
			switch m.values["connect"] {
			case connectPaired:
				f = append(f, wizardField{"transport", "Pairing transport", []string{"tailscale", "lan"}}, wizardField{"endpoints", "Gateway endpoints", []string{"automatic", "custom"}})
				if m.values["endpoints"] == "custom" {
					f = append(f, wizardField{"gw_listen", "Gateway listen IP:port (blank: automatic)", nil}, wizardField{"gw_advertise", "Gateway advertise IP:port (blank: same as listen)", nil})
				}
			case connectDirect:
				f = append(f, wizardField{"mode", "Direct network transport (shared cluster token)", []string{"lan", "tailscale"}})
				switch m.values["mode"] {
				case "lan":
					f = append(f, wizardField{"bind", "Listen address (blank: all interfaces)", nil}, wizardField{"advertise", "Address peers dial (blank: first private interface)", nil}, wizardField{"join_lan", "Gossip host:port seeds (comma-separated, blank: none)", nil}, wizardField{"mdns", "Advertise and browse mDNS?", []string{"yes", "no"}})
				case "tailscale":
					f = append(f, wizardField{"port", "Well-known gossip port", nil}, wizardField{"interval", "Tailnet discovery interval", nil}, wizardField{"join_tailscale", "Extra gossip host:port seeds (comma-separated)", nil})
				}
			}
			f = append(f, wizardField{"hostname", "Hostname override (blank: OS hostname)", nil}, wizardField{"updates", "Install verified updates automatically? (service only; HTTPS to github.com)", []string{"yes", "no"}}, wizardField{"custom", "Customize directories or Codex binary?", []string{"no", "yes"}})
			if m.values["custom"] == "yes" {
				f = append(f, wizardField{"runtime", "Runtime directory (blank: " + m.c.RuntimeDir + ")", nil}, wizardField{"claude", "Claude directory (blank: " + m.c.Claude.ConfigDir + ")", nil}, wizardField{"codex", "Codex home (blank: " + m.c.Codex.Home + ")", nil}, wizardField{"bin", "Codex executable (blank: " + m.c.Codex.Bin + ")", nil})
			}
		}
	}
	f = append(f, wizardField{"service", "Install and start the background service?", []string{"no", "yes"}})
	return append(f, wizardField{"review", "Review / Apply", nil})
}

// plan is the staged connection decision.
func (m *setupModel) plan() connectionPlan {
	p := connectionPlan{Choice: m.values["connect"], Stored: m.c, Tailscale: m.tailscale}
	switch p.Choice {
	case connectPaired:
		p.Transport = m.values["transport"]
		if m.values["endpoints"] == "custom" {
			p.Listen, p.Advertise = strings.TrimSpace(m.values["gw_listen"]), strings.TrimSpace(m.values["gw_advertise"])
		}
	case connectDirect:
		p.Transport = m.values["mode"]
	}
	return p
}

// effectiveGateway is the gateway mode in force after apply: the staged plan,
// the fixed flag answers, or the kept file.
func (m *setupModel) effectiveGateway() string {
	switch {
	case m.fixedAnswers != nil && (!m.exists || m.replace):
		return defaultString(m.fixedAnswers.Gateway.Mode, defaultString(m.c.Gateway.Mode, "off"))
	case m.asksConnection():
		return m.plan().gatewayMode()
	}
	return defaultString(m.c.Gateway.Mode, "off")
}
func (m *setupModel) answers() (*configAnswers, error) {
	stored, err := storedConfig(m.path)
	if err != nil {
		return nil, err
	}
	if m.fixedAnswers != nil {
		m.fixedAnswers.Stored = stored
		return m.fixedAnswers, nil
	}
	if m.exists && m.values["existing"] == "keep" {
		return nil, nil
	}
	// Replacing merges these answers into the stored file: unanswered settings
	// (history, agent options) survive, and a blank hostname clears. The
	// connection answer decides both the network and the gateway table.
	plan := m.plan()
	a := &configAnswers{Stored: stored, Selected: map[string]bool{"hostname": true, "network": true}, Hostname: m.values["hostname"], Network: config.Network{Mode: plan.networkMode()}, Gateway: config.Gateway{Mode: plan.gatewayMode(), Listen: plan.Listen, Advertise: plan.Advertise}}
	storedGateway, _ := stored["gateway"].(map[string]any)
	storedMode, _ := storedGateway["mode"].(string)
	storedListen, _ := storedGateway["listen"].(string)
	storedAdvertise, _ := storedGateway["advertise"].(string)
	a.Selected["gateway-mode"] = a.Gateway.Mode != defaultString(storedMode, "off")
	a.Selected["gateway-listen"] = a.Gateway.Listen != storedListen
	a.Selected["gateway-advertise"] = a.Gateway.Advertise != storedAdvertise
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
	// The update answer is written when it differs from the effective value
	// or when the file already holds the key, so an explicit choice persists.
	if enabled := m.values["updates"] == "yes"; enabled != m.c.AutoUpdate.Enabled || m.c.Sources["update.enabled"] == "file" {
		a.AutoUpdate = &enabled
		a.Selected["auto-update"] = true
	}
	for flag, set := range map[string]bool{
		"bind-addr": n.BindAddr != "", "advertise-addr": n.AdvertiseAddr != "", "join": len(n.Join) > 0,
		"gossip-port": n.GossipPort != 0, "discovery-interval": n.DiscoveryInterval != "", "mdns": n.MDNS != nil,
		"runtime-dir": a.RuntimeDir != "", "claude-dir": a.ClaudeDir != "", "codex-home": a.CodexHome != "", "codex-bin": a.CodexBin != "",
	} {
		a.Selected[flag] = set
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
	case "gw_listen", "gw_advertise":
		if v := strings.TrimSpace(m.values[key]); v != "" {
			return config.ValidateGatewayEndpoint(v, m.values["transport"])
		}
		return nil
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

// startProbe runs the advisory Tailscale detection once, in the background
// and without blocking any answer; the result only updates the hints and, when
// the stored file has no pairing transport yet, the untouched transport default.
func (m *setupModel) startProbe() tea.Cmd {
	if m.probed || m.probing {
		return nil
	}
	m.probing = true
	return m.work.command(func(ctx context.Context) tea.Msg {
		return probeResult{probeTailscaleBounded(ctx, m.probe)}
	})
}
func (m *setupModel) Init() tea.Cmd {
	if m.fixedAnswers == nil {
		return m.startProbe()
	}
	return nil
}
func (m *setupModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = v.Width
		m.height = v.Height
	case probeResult:
		m.probing, m.probed = false, true
		report := v.report
		m.tailscale = &report
		if m.recommend && !m.touched["transport"] {
			m.values["transport"] = recommendedTransport(&report)
		}
	case setupResult:
		m.busy = false
		m.done = true
		m.result = v
		m.err = v.err
		m.pair = pairingPhase{runtime: v.pairing, choice: 2, closed: v.pairing == ""}
	case pairingResult:
		m.pair.busy = false
		m.pair.err = v.err
		if v.err == nil {
			m.pair.lines, m.pair.closed, m.pair.editing = v.lines, true, false
		}
	case tea.KeyMsg:
		key := v.String()
		if key == "ctrl+c" || key == "esc" {
			m.work.cancel()
			return m, tea.Quit
		}
		if m.done {
			return m, m.updatePairing(v)
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
			m.touched[field.key] = true
			m.step++
			m.choice = 0
			m.offset = 0
			m.err = nil
			if field.key == "existing" && m.values["existing"] == "replace" {
				return m, m.startProbe()
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
			default:
				return m, nil
			}
			m.values[field.key] = field.choices[index]
			m.touched[field.key] = true
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

// updatePairing handles keys after apply: the pairing menu, the invitation
// entry, or exit once pairing is finished or was never offered.
func (m *setupModel) updatePairing(v tea.KeyMsg) tea.Cmd {
	key := v.String()
	if m.pair.closed {
		if key == "enter" || key == "q" {
			return tea.Quit
		}
		return nil
	}
	if m.pair.busy {
		return nil
	}
	if m.pair.editing {
		switch {
		case key == "enter":
			code := m.pair.code
			if _, err := normalizeInvitation(code); err != nil {
				m.pair.err = err
				return nil
			}
			m.pair.busy, m.pair.err = true, nil
			return m.work.command(func(ctx context.Context) tea.Msg {
				j, err := pairingJoinHost(ctx, m.pair.runtime, m.admin, code)
				if err != nil {
					return pairingResult{err: err}
				}
				return pairingResult{lines: joinLines(j)}
			})
		case key == "shift+tab":
			m.pair.editing = false
		case key == "backspace":
			if r := []rune(m.pair.code); len(r) > 0 {
				m.pair.code = string(r[:len(r)-1])
			}
		case key == "ctrl+u":
			m.pair.code = ""
		case v.Type == tea.KeyRunes && len(m.pair.code)+len(v.Runes) <= maxInvitation+16:
			m.pair.code += string(v.Runes)
		}
		return nil
	}
	switch key {
	case "down", "j":
		m.pair.choice = min(m.pair.choice+1, len(pairingChoices)-1)
	case "up", "k":
		m.pair.choice = max(0, m.pair.choice-1)
	case "enter":
		switch m.pair.choice {
		case 0:
			m.pair.busy, m.pair.err = true, nil
			return m.work.command(func(ctx context.Context) tea.Msg {
				command, id, err := pairingInvite(ctx, m.pair.runtime, m.admin)
				if err != nil {
					return pairingResult{err: err}
				}
				return pairingResult{lines: inviteLines(command, id)}
			})
		case 1:
			m.pair.editing, m.pair.err = true, nil
		default:
			m.pair.closed, m.pair.lines = true, pairingNextSteps(m.path, true)
		}
	}
	return nil
}

// tailscaleLine is the probe hint for the current moment.
func (m *setupModel) tailscaleLine() string {
	if m.tailscale == nil {
		return "Tailscale: checking (read-only)… you can continue; the result updates here."
	}
	return tailscaleSummary(*m.tailscale)
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
		if m.asksConnection() {
			for _, line := range m.plan().lines() {
				b.WriteString(safeText(line) + "\n")
			}
		}
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
	}
	if m.asksConnection() {
		fmt.Fprintf(&b, "Automatic updates: %s\n", m.values["updates"])
	}
	fmt.Fprintf(&b, "Install and start service: %s\n", m.values["service"])
	if m.effectiveGateway() != "off" {
		if m.values["service"] == "yes" {
			b.WriteString("Pairing: offered after the service and its gateway are confirmed running (invite or join; nothing is sent before you choose).\n")
		} else {
			b.WriteString("Pairing: later; the exact commands are printed after apply.\n")
		}
	}
	b.WriteString("Enter applies these changes. Esc cancels before any writes.")
	return b.String()
}
func (m *setupModel) View() string {
	field := m.fields()[m.step]
	body := safeText(field.label) + "\n"
	switch {
	case m.done:
		body = m.doneView()
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
	if !m.done {
		switch field.key {
		case "connect":
			body += "\n" + connectExplanation(m.values["connect"]) + "\n" + m.tailscaleLine()
		case "transport":
			body += "\n" + transportExplanation(m.values["transport"], m.tailscale) + "\n" + m.tailscaleLine()
		case "endpoints":
			body += "\nAutomatic resolves the listen endpoint when the service starts (no address entry). Custom sets concrete IP:port endpoints for the " + m.values["transport"] + " policy."
		case "mode":
			body += "\n" + connectExplanation(connectDirect)
			if m.values["mode"] == "tailscale" {
				body += "\n" + m.tailscaleLine()
			}
		case "service":
			if m.effectiveGateway() != "off" {
				body += "\nPaired devices need the running service; choosing yes offers pairing right after it starts. Choosing no prints the commands for later."
			}
		}
		if m.values["connect"] == connectDirect && m.asksConnection() {
			body += "\nNetwork access requires the separately managed cluster token."
		}
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

// doneView shows the completed steps and, when the gateway is ready, the
// pairing menu; otherwise the exit prompt.
func (m *setupModel) doneView() string {
	body := strings.Join(m.result.steps, "\n")
	if m.err != nil {
		return body
	}
	if m.pair.closed {
		if len(m.pair.lines) > 0 {
			body += "\n" + strings.Join(m.pair.lines, "\n")
		}
		return body + "\nSetup completed. Enter exits."
	}
	body += "\nSetup completed. Pair this device now? (Esc exits; pairing can also be done later.)\n"
	switch {
	case m.pair.editing:
		body += "Invitation (paste the code printed by `rhizome invite` on the other device; Shift+Tab goes back):\n> " + safeText(m.pair.code) + "_"
	default:
		for i, s := range pairingChoices {
			prefix := "  "
			if i == m.pair.choice {
				prefix = "> "
			}
			body += prefix + s + "\n"
		}
	}
	if m.pair.err != nil {
		body = "Pairing error: " + safeText(m.pair.err.Error()) + " (retry here, or later with `rhizome invite` / `rhizome join <invitation>`)\n" + body
	}
	if m.pair.busy {
		body = "Pairing… Esc cancels; completed steps remain.\n" + body
	}
	return body
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
	gateway := m.effectiveGateway()
	if m.values["service"] != "yes" {
		if gateway != "off" {
			r.steps = append(r.steps, pairingNextSteps(m.path, false)...)
		}
		return r
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	path, err := filepath.Abs(m.path)
	if err != nil {
		return fail(err)
	}
	outcome, err := m.installService(ctx, path)
	if err != nil {
		return fail(fmt.Errorf("service step failed; completed steps remain: %w", err))
	}
	r.steps = append(r.steps, "Background service installed and running (configuration "+safeText(path)+", runtime "+safeText(outcome.Runtime)+")")
	if gateway == "off" {
		return r
	}
	// Pairing only against the configuration the service actually runs, and
	// only once its gateway answers; otherwise the exact commands for later.
	if outcome.WasRunning && wrote {
		r.steps = append(r.steps, "The service was already running with the previous configuration; run `rhizome service restart` to apply the new one, then pair:")
		r.steps = append(r.steps, pairingNextSteps(path, true)[1:]...)
		return r
	}
	if err := waitGatewayReady(ctx, outcome.Runtime, m.admin, gatewayReadyWait); err != nil {
		r.steps = append(r.steps, "Service running but "+safeText(err.Error())+"; check `rhizome service status`, then pair:")
		r.steps = append(r.steps, pairingNextSteps(path, true)[1:]...)
		return r
	}
	r.pairing = outcome.Runtime
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
		for _, line := range m.pair.lines {
			fmt.Println(line)
		}
	}
	return m.exitStatus(err)
}

// exitStatus is the command result once the program loop ended: apply errors
// first, then an interrupted apply or pairing, then a pairing attempt that
// failed and was not retried; each names what remains and the retry path.
func (m *setupModel) exitStatus(runErr error) error {
	if !m.applied {
		return errors.New("setup cancelled; no changes applied")
	}
	switch {
	case m.result.err != nil:
		return m.result.err
	case !m.done:
		return errors.New("setup interrupted; completed steps above were not rolled back")
	case m.pair.busy:
		return errors.New("pairing interrupted after setup completed; check `rhizome peers list` and retry with `rhizome invite` or `rhizome join <invitation>`")
	case m.pair.err != nil && !m.pair.closed:
		return fmt.Errorf("pairing failed after setup completed: %w; retry with `rhizome invite` or `rhizome join <invitation>`", m.pair.err)
	}
	return runErr
}
