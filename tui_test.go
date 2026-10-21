package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"rhizome/internal/config"
)

func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "back":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}
func wizard(t *testing.T) *setupModel {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	c, err := config.Resolve(nil, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	m := newSetupModel(c, filepath.Join(home, "config.toml"), []agent{{Name: "claude", Label: "Claude", Skills: filepath.Join(home, "skills"), Detected: true}, {Name: "codex", Label: "Codex", Skills: filepath.Join(home, "other")}}, nil, nil, false)
	t.Cleanup(m.work.close)
	m.installAgent = func(_ config.Config, a agent, out io.Writer) error {
		status, err := installSkill(a.Skills)
		fmt.Fprintf(out, "%s:\n  skill: %s\n", a.Label, status)
		return err
	}
	return m
}
func gotoField(t *testing.T, m *setupModel, name string) {
	t.Helper()
	for i, f := range m.fields() {
		if f.key == name {
			m.step = i
			return
		}
	}
	t.Fatalf("missing field %s", name)
}
func TestWizardDefaultsBackAndCancel(t *testing.T) {
	m := wizard(t)
	if !m.checked[0] || m.checked[1] {
		t.Fatal("agent defaults")
	}
	m.Update(key("enter"))
	if m.fields()[m.step].key != "mode" {
		t.Fatal("mode")
	}
	m.Update(key("enter"))
	m.Update(key("device"))
	m.Update(key("enter"))
	m.Update(key("back"))
	if m.values["hostname"] != "device" {
		t.Fatal("lost answer")
	}
	_, cmd := m.Update(key("esc"))
	if cmd == nil || m.applied {
		t.Fatal("cancel")
	}
	if entries, _ := os.ReadDir(filepath.Dir(m.path)); len(entries) != 0 {
		t.Fatal("cancel wrote files")
	}
}
func TestWizardBranchesAndValidation(t *testing.T) {
	m := wizard(t)
	m.values["mode"] = "lan"
	m.values["bind"] = "192.168.1.2"
	m.values["mdns"] = "no"
	m.values["join_lan"] = "lan:123"
	a, err := m.answers()
	if err != nil || a.Network.MDNS == nil || *a.Network.MDNS {
		t.Fatal(a, err)
	}
	m.values["mode"] = "tailscale"
	m.values["join_tailscale"] = "tail:123"
	a, err = m.answers()
	if err != nil || a.Network.BindAddr != "" || a.Network.MDNS != nil || len(a.Network.Join) != 1 || a.Network.Join[0] != "tail:123" {
		t.Fatal(a, err)
	}
	m.values["port"] = "bad"
	gotoField(t, m, "port")
	m.Update(key("enter"))
	if m.err == nil || m.fields()[m.step].key != "port" {
		t.Fatal("invalid port advanced")
	}
	m.Update(key("back"))
	m.Update(key("enter")) // returning through mode must not trap invalid later answers
	if m.fields()[m.step].key != "port" {
		t.Fatal("back navigation trapped")
	}
	m.busy = false
	m.values["port"] = "7946"
	m.values["mode"] = "local"
	a, err = m.answers()
	if err != nil || len(a.Network.Join) > 0 || a.Network.GossipPort != 0 || a.Network.BindAddr != "" {
		t.Fatal("stale fields", a, err)
	}
	m.values["hostname"] = "bad\x1bname"
	if m.validate() == nil {
		t.Fatal("hostname accepted")
	}
	m.values["hostname"] = "valid"
	m.values["custom"] = "yes"
	m.values["runtime"] = "relative"
	if m.validate() == nil {
		t.Fatal("relative directory accepted")
	}
	m.values["custom"] = "no"
	if err := m.validate(); err != nil {
		t.Fatal(err)
	}
}
func TestWizardReviewApplyAndFailure(t *testing.T) {
	m := wizard(t)
	gotoField(t, m, "review")
	if !strings.Contains(m.View(), "Enter applies") {
		t.Fatal(m.View())
	}
	_, cmd := m.Update(key("enter"))
	if !m.applied || !m.busy || cmd == nil {
		t.Fatal("not pending")
	}
	if _, duplicate := m.Update(key("enter")); duplicate != nil {
		t.Fatal("duplicate apply")
	}
	m.Update(cmd())
	if m.err != nil || !m.done {
		t.Fatal(m.err)
	}
	if st, err := os.Stat(m.path); err != nil || st.Mode().Perm() != 0600 {
		t.Fatal(st, err)
	}
	if _, err := os.Stat(filepath.Join(m.agents[0].Skills, skillName, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.agents[1].Skills); !os.IsNotExist(err) {
		t.Fatal("unselected skill written")
	}
	failed := wizard(t)
	gotoField(t, failed, "review")
	failed.apply = func(context.Context) setupResult {
		return setupResult{steps: []string{"completed step"}, err: errors.New("write failed")}
	}
	_, cmd = failed.Update(key("enter"))
	failed.Update(cmd())
	if !strings.Contains(failed.View(), "write failed") || !strings.Contains(failed.View(), "completed step") {
		t.Fatal(failed.View())
	}
}
func TestWizardExistingKeepAndReplace(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(strconvBool(replace), func(t *testing.T) {
			m := wizard(t)
			original := "[network]\nmode = 'local'\n"
			if err := os.WriteFile(m.path, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			m.exists = true
			if replace {
				m.values["existing"] = "replace"
				m.values["hostname"] = "new"
			}
			r := m.applyAnswers(context.Background())
			if r.err != nil {
				t.Fatal(r.err)
			}
			b, _ := os.ReadFile(m.path)
			backups, _ := filepath.Glob(m.path + ".bak-*")
			if replace && (len(backups) != 1 || !strings.Contains(string(b), "new")) {
				t.Fatal("replace", string(b), backups)
			}
			if !replace && (string(b) != original || len(backups) != 0) {
				t.Fatal("keep changed file")
			}
		})
	}
}
func strconvBool(v bool) string {
	if v {
		return "replace"
	}
	return "keep"
}
func TestWizardTailscaleAdvisory(t *testing.T) {
	m := wizard(t)
	m.values["mode"] = "tailscale"
	m.probe = func(context.Context) error { return errors.New("daemon stopped") }
	gotoField(t, m, "mode")
	_, cmd := m.Update(key("enter"))
	if cmd == nil || !m.busy {
		t.Fatal("probe not asynchronous")
	}
	m.Update(cmd())
	if m.busy || !strings.Contains(m.warning, "daemon stopped") || m.validate() != nil {
		t.Fatal("probe became mandatory")
	}
}
func TestPanelActionsAndReadOnly(t *testing.T) {
	for _, ro := range []bool{false, true} {
		calls := []string{}
		m := &panelModel{work: newUIWork(), readOnly: ro, width: 80, height: 24, load: func(context.Context) panelResult { return panelResult{body: "status"} }, act: func(_ context.Context, a string) error { calls = append(calls, a); return nil }}
		m.Update(m.Init()())
		m.Update(key("x"))
		if ro {
			if m.confirm != "" {
				t.Fatal("read-only confirmation")
			}
			m.work.close()
			continue
		}
		if m.confirm != "stop" || len(calls) != 0 {
			t.Fatal("missing confirmation")
		}
		m.Update(key("n"))
		if m.confirm != "" {
			t.Fatal("confirmation retained")
		}
		m.Update(key("u"))
		_, cmd := m.Update(key("y"))
		if !m.busy {
			t.Fatal("not pending")
		}
		if _, again := m.Update(key("i")); again != nil {
			t.Fatal("duplicate action")
		}
		m.Update(cmd())
		if len(calls) != 1 || calls[0] != "uninstall" || !strings.Contains(m.notice, "completed") {
			t.Fatal(calls, m.notice)
		}
		m.act = func(context.Context, string) error { return errors.New("denied") }
		_, cmd = m.Update(key("s"))
		m.Update(cmd())
		if !strings.Contains(m.View(), "denied") {
			t.Fatal(m.View())
		}
		m.work.close()
	}
}
func TestPeerIdentityResizeAndErrors(t *testing.T) {
	id := strings.Repeat("a", 64)
	m := &panelModel{work: newUIWork(), readOnly: true, width: 80, height: 24}
	defer m.work.close()
	m.Update(panelResult{peers: []peerRow{{HostID: id, Endpoint: "bad\x1b[2J\u202ehost"}, {HostID: "other"}}})
	view := m.View()
	if !strings.Contains(view, id) || strings.ContainsAny(view, "\x1b\u202e") {
		t.Fatal(view)
	}
	m.Update(key("down"))
	if m.selected != 1 {
		t.Fatal("selection")
	}
	m.Update(tea.WindowSizeMsg{Width: 20, Height: 8})
	view = m.View()
	if len(strings.Split(view, "\n")) > 8 {
		t.Fatal("height", view)
	}
	for _, line := range strings.Split(view, "\n") {
		if ansi.StringWidth(line) > 20 {
			t.Fatal("width", line)
		}
	}
	m.Update(panelResult{body: "No paired hosts."})
	if !strings.Contains(m.View(), "No paired hosts") {
		t.Fatal(m.View())
	}
	m.Update(panelResult{err: errors.New("gateway service unavailable")})
	if !strings.Contains(m.View(), "Error:") {
		t.Fatal(m.View())
	}
}
func TestUIWorkerCancellation(t *testing.T) {
	w := newUIWork()
	started := make(chan struct{})
	done := make(chan struct{})
	cmd := w.command(func(ctx context.Context) tea.Msg { close(started); <-ctx.Done(); return ctx.Err() })
	go func() { cmd(); close(done) }()
	<-started
	w.close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
	if result := w.command(func(context.Context) tea.Msg { t.Error("started after close"); return nil })(); result != nil {
		t.Fatal(result)
	}
}
func TestCLIRejectsTUIAndPreservesPlain(t *testing.T) {
	t.Setenv("TERM", "dumb")
	for _, fn := range []func() error{func() error { return serviceCommand([]string{"status", "--tui"}) }, func() error { return gatewayCommand("peers", []string{"list", "--tui"}) }} {
		if err := fn(); err == nil || !strings.Contains(err.Error(), "requires terminal") {
			t.Fatal(err)
		}
	}
	if err := serviceCommand(nil); err == nil {
		t.Fatal("bare service accepted")
	}
	m := wizard(t)
	t.Setenv("RHIZOME_CONFIG", "")
	for _, args := range [][]string{{"--help"}, {"--list", "--plain"}, {"--plain", "--agents", ",", "--network", "local", "--config", m.path}} {
		err := setup(args)
		if err != nil && !errors.Is(err, flag.ErrHelp) {
			t.Fatal(args, err)
		}
	}
	if _, err := os.Stat(m.path); err != nil {
		t.Fatal(err)
	}
}

func TestWizardServiceOptInAndPartialFailure(t *testing.T) {
	m := wizard(t)
	calls := 0
	m.installService = func(_ context.Context, path string) (serviceOutcome, error) {
		calls++
		if path != m.path || !filepath.IsAbs(path) {
			t.Error(path)
		}
		return serviceOutcome{}, errors.New("override differs")
	}
	m.values["service"] = "yes"
	if !strings.Contains(m.review(), "Install and start service: yes") {
		t.Fatal(m.review())
	}
	r := m.applyAnswers(context.Background())
	if calls != 1 || r.err == nil || len(r.steps) != 2 || !strings.Contains(r.err.Error(), "override differs") {
		t.Fatal(calls, r)
	}
	if _, err := os.Stat(m.path); err != nil {
		t.Fatal("completed config missing", err)
	}
	m2 := wizard(t)
	m2.installService = func(context.Context, string) (serviceOutcome, error) {
		t.Error("implicit service installation")
		return serviceOutcome{}, nil
	}
	if r := m2.applyAnswers(context.Background()); r.err != nil {
		t.Fatal(r.err)
	}
	m3 := wizard(t)
	m3.installService = func(context.Context, string) (serviceOutcome, error) {
		return serviceOutcome{Installed: true, Running: true, Runtime: "/run"}, nil
	}
	m3.values["service"] = "yes"
	r = m3.applyAnswers(context.Background())
	if r.err != nil || len(r.steps) != 3 || !strings.Contains(r.steps[2], "installed and running (configuration "+m3.path+", runtime /run)") {
		t.Fatal(r)
	}
}
func TestWizardKeptConfigOffersService(t *testing.T) {
	original := "[network]\nmode = 'local'\n"
	for _, fixed := range []bool{false, true} {
		t.Run(map[bool]string{false: "wizard", true: "network flag"}[fixed], func(t *testing.T) {
			m := wizard(t)
			if err := os.WriteFile(m.path, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			m.exists = true
			if fixed {
				m.fixedAnswers = &configAnswers{Network: config.Network{Mode: "lan"}}
			}
			keys := []string{}
			for _, f := range m.fields() {
				keys = append(keys, f.key)
			}
			joined := strings.Join(keys, ",")
			if !strings.Contains(joined, "service,review") || strings.Contains(joined, "mode") || fixed == strings.Contains(joined, "existing") {
				t.Fatal(joined)
			}
			gotoField(t, m, "service")
			if !strings.Contains(m.View(), "Install and start the background service?") {
				t.Fatal(m.View())
			}
			if !strings.Contains(m.review(), "Keep existing configuration.") || !strings.Contains(m.review(), "Install and start service: no") {
				t.Fatal(m.review())
			}
			var paths []string
			m.installService = func(_ context.Context, path string) (serviceOutcome, error) {
				paths = append(paths, path)
				return serviceOutcome{Installed: true, Running: true, Runtime: "/run"}, nil
			}
			if r := m.applyAnswers(context.Background()); r.err != nil || len(paths) != 0 || len(r.steps) != 2 || r.steps[0] != "Kept configuration" {
				t.Fatal("declined service", r, paths)
			}
			m.values["service"] = "yes"
			r := m.applyAnswers(context.Background())
			if r.err != nil || len(paths) != 1 || paths[0] != m.path || len(r.steps) != 3 || r.steps[0] != "Kept configuration" || !strings.Contains(r.steps[2], "installed and running") {
				t.Fatal(r, paths)
			}
			b, _ := os.ReadFile(m.path)
			backups, _ := filepath.Glob(m.path + ".bak-*")
			if string(b) != original || len(backups) != 0 {
				t.Fatal("keep changed file", string(b), backups)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if r := m.applyAnswers(ctx); !errors.Is(r.err, context.Canceled) || len(paths) != 1 {
				t.Fatal("cancelled apply reached the service step", r, paths)
			}
		})
	}
}
func TestWizardCancelledApply(t *testing.T) {
	m := wizard(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r := m.applyAnswers(ctx); !errors.Is(r.err, context.Canceled) || len(r.steps) != 0 {
		t.Fatal(r)
	}
	if entries, _ := os.ReadDir(filepath.Dir(m.path)); len(entries) != 0 {
		t.Fatal("cancelled apply wrote files")
	}
}

func TestWizardApplyInstallsMCPAndHooks(t *testing.T) {
	m := wizard(t)
	m.installAgent = m.defaultAgentInstaller
	home := filepath.Dir(m.path)
	m.agents[0].MCP = filepath.Join(home, ".claude.json")
	m.agents[0].Hooks = filepath.Join(home, "settings.json")
	m.checked = []bool{true, false}
	r := m.applyAnswers(context.Background())
	if r.err != nil {
		t.Fatal(r.err)
	}
	for _, path := range []string{m.agents[0].MCP, m.agents[0].Hooks, filepath.Join(m.agents[0].Skills, skillName, "SKILL.md")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := os.ReadFile(m.agents[0].MCP); !strings.Contains(string(b), "rhizome") {
		t.Fatal(string(b))
	}
	joined := strings.Join(r.steps, "\n")
	for _, want := range []string{"Claude: skill:", "Claude: mcp:", "Claude: hooks:"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %s", want, joined)
		}
	}
}
