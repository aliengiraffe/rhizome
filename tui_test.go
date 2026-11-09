package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"rhizome/internal/channel"
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
	m.width = 400 // assertions match whole phrases; wrapping is covered by the resize test
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
	if m.fields()[m.step].key != "connect" || m.values["connect"] != connectLocal {
		t.Fatal("connection question missing or not defaulting to this device only")
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
	m.values["connect"] = connectDirect
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
	m.values["port"] = "7946"
	m.values["connect"] = connectLocal
	a, err = m.answers()
	if err != nil || len(a.Network.Join) > 0 || a.Network.GossipPort != 0 || a.Network.BindAddr != "" || a.Gateway.Mode != "off" {
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
	// The probe starts with the screen, never blocks a key, explains every
	// state in place and only moves an untouched transport default.
	for _, tc := range []struct {
		name      string
		report    channel.TailscaleReport
		transport string
	}{
		{"usable", channel.TailscaleReport{State: channel.TailscaleUsable, Detail: "Tailscale is running", Address: "100.64.0.1"}, "tailscale"},
		{"missing cli", channel.TailscaleReport{State: channel.TailscaleMissingCLI, Detail: "Tailscale CLI not found; install Tailscale or add its CLI to PATH"}, "lan"},
		{"daemon stopped", channel.TailscaleReport{State: channel.TailscaleDaemonDown, Detail: "daemon stopped"}, "lan"},
		{"needs login", channel.TailscaleReport{State: channel.TailscaleNeedsLogin, Detail: "not logged in"}, "lan"},
		{"no address", channel.TailscaleReport{State: channel.TailscaleNoAddress, Detail: "no Tailscale IPv4 address"}, "lan"},
		{"timeout", channel.TailscaleReport{State: channel.TailscaleProbeTimeout, Detail: "the Tailscale check timed out"}, "lan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := wizard(t)
			started := make(chan struct{})
			m.probe = func(ctx context.Context) channel.TailscaleReport {
				close(started)
				if _, ok := ctx.Deadline(); !ok {
					t.Error("probe without deadline")
				}
				return tc.report
			}
			cmd := m.Init()
			if cmd == nil || m.busy || !m.probing {
				t.Fatal("probe not started asynchronously")
			}
			gotoField(t, m, "connect")
			if view := m.View(); !strings.Contains(view, "checking (read-only)") || !strings.Contains(view, "no service or pairing is needed") {
				t.Fatal(view)
			}
			m.Update(key("down")) // keys are accepted while the probe runs
			m.Update(key("up"))
			m.Update(cmd())
			<-started
			if m.probing || m.tailscale == nil || m.values["transport"] != tc.transport || m.values["connect"] != connectLocal {
				t.Fatalf("%+v %v", m.tailscale, m.values)
			}
			view := m.View()
			if !strings.Contains(view, tc.report.Detail) && !strings.Contains(view, "100.64.0.1") {
				t.Fatal(view)
			}
			if tc.report.Usable() != strings.Contains(view, "Recommended") {
				t.Fatal(view)
			}
			if m.validate() != nil {
				t.Fatal("probe became mandatory")
			}
			if again := m.Init(); again != nil {
				t.Fatal("probe repeated")
			}
		})
	}
	t.Run("touched transport is not moved", func(t *testing.T) {
		m := wizard(t)
		m.values["connect"] = connectPaired
		gotoField(t, m, "transport")
		m.Update(key("down")) // lan -> tailscale -> ... user picked explicitly
		picked := m.values["transport"]
		m.Update(probeResult{channel.TailscaleReport{State: channel.TailscaleUsable, Address: "100.64.0.1"}})
		if m.values["transport"] != picked {
			t.Fatal("recommendation overrode an explicit choice")
		}
	})
	t.Run("stored pairing transport is not moved", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		c, _ := config.Resolve(nil, func(string) string { return "" })
		c.Gateway.Mode = "lan"
		m := newSetupModel(c, filepath.Join(home, "config.toml"), nil, []bool{}, nil, false)
		defer m.work.close()
		m.Update(probeResult{channel.TailscaleReport{State: channel.TailscaleUsable, Address: "100.64.0.1"}})
		if m.values["connect"] != connectPaired || m.values["transport"] != "lan" {
			t.Fatalf("%v", m.values)
		}
	})
}

func TestWizardConnectionChoices(t *testing.T) {
	m := wizard(t)
	m.Update(probeResult{channel.TailscaleReport{State: channel.TailscaleMissingCLI, Detail: "Tailscale CLI not found"}})
	keys := func() string {
		var out []string
		for _, f := range m.fields() {
			out = append(out, f.key)
		}
		return strings.Join(out, ",")
	}
	if k := keys(); strings.Contains(k, "transport") || strings.Contains(k, "mode") || !strings.Contains(k, "connect,hostname") {
		t.Fatal(k)
	}
	// Paired devices: transport and endpoints, MCP networking pinned to local.
	gotoField(t, m, "connect")
	m.Update(key("down"))
	if m.values["connect"] != connectPaired || !strings.Contains(m.View(), "Requires the service") {
		t.Fatal(m.View())
	}
	if k := keys(); !strings.Contains(k, "connect,transport,endpoints,hostname") {
		t.Fatal(k)
	}
	m.Update(key("enter"))
	if m.fields()[m.step].key != "transport" || m.values["transport"] != "lan" || !strings.Contains(m.View(), "private LAN interface") {
		t.Fatal(m.View())
	}
	m.Update(key("enter"))
	if m.fields()[m.step].key != "endpoints" || !strings.Contains(m.View(), "Automatic resolves the listen endpoint") {
		t.Fatal(m.View())
	}
	a, err := m.answers()
	if err != nil || a.Gateway.Mode != "lan" || a.Network.Mode != "local" || !a.Selected["gateway-mode"] || a.Gateway.Listen != "" {
		t.Fatalf("%+v %v", a, err)
	}
	review := m.review()
	for _, want := range []string{"Connection: paired devices over lan", "Gateway endpoints: automatic (a private LAN interface", "Change: gateway pairing turns on over lan", `[gateway]`, `mode = "lan"`, `[network]`, `mode = "local"`, "Pairing: later; the exact commands are printed after apply"} {
		if !strings.Contains(review, want) {
			t.Fatalf("missing %q in\n%s", want, review)
		}
	}
	m.values["service"] = "yes"
	gotoField(t, m, "service")
	if !strings.Contains(m.View(), "Paired devices need the running service") || !strings.Contains(m.review(), "Pairing: offered after the service") {
		t.Fatal(m.View(), m.review())
	}
	// Custom endpoints are validated against the transport policy inline.
	m.values["endpoints"] = "custom"
	m.values["gw_listen"] = "8.8.8.8:7947"
	gotoField(t, m, "gw_listen")
	m.Update(key("enter"))
	if m.err == nil || m.fields()[m.step].key != "gw_listen" {
		t.Fatal("public endpoint advanced")
	}
	m.values["gw_listen"] = "192.168.1.2:7947"
	m.Update(key("enter"))
	if m.err != nil || m.fields()[m.step].key != "gw_advertise" {
		t.Fatal(m.err)
	}
	if a, err = m.answers(); err != nil || a.Gateway.Listen != "192.168.1.2:7947" || !a.Selected["gateway-listen"] {
		t.Fatalf("%+v %v", a, err)
	}
	if r := m.review(); !strings.Contains(r, "Gateway listen: 192.168.1.2:7947") || !strings.Contains(r, "Gateway advertise: same as listen") {
		t.Fatal(r)
	}
	m.values["transport"] = "tailscale"
	if m.validate() == nil {
		t.Fatal("LAN endpoint accepted for Tailscale pairing")
	}
	m.values["endpoints"] = "automatic"
	if err := m.validate(); err != nil {
		t.Fatal(err)
	}
	if r := m.review(); !strings.Contains(r, "Connection: paired devices over tailscale") || !strings.Contains(r, "Tailscale IPv4, port 7947") || !strings.Contains(r, "Tailscale CLI not found") {
		t.Fatal(r)
	}
	gotoField(t, m, "transport")
	if !strings.Contains(m.View(), "service will refuse to start") {
		t.Fatal(m.View())
	}
	// Direct network: the legacy gossip fields, gateway off, token notice.
	m.values["connect"] = connectDirect
	if k := keys(); !strings.Contains(k, "connect,mode,bind,advertise,join_lan,mdns,hostname") || strings.Contains(k, "transport") {
		t.Fatal(k)
	}
	gotoField(t, m, "mode")
	if v := m.View(); !strings.Contains(v, "shared-token gossip") || !strings.Contains(v, "cluster token") {
		t.Fatal(v)
	}
	if a, err = m.answers(); err != nil || a.Gateway.Mode != "off" || a.Network.Mode != "lan" || a.Selected["gateway-mode"] {
		t.Fatalf("%+v %v", a, err)
	}
	if r := m.review(); !strings.Contains(r, "Connection: direct lan network") || !strings.Contains(r, "Change: MCP networking switches from local to lan gossip") || strings.Contains(r, "Pairing:") {
		t.Fatal(r)
	}
	// Back from the transport field lands on the connection question again.
	m.values["connect"] = connectPaired
	gotoField(t, m, "transport")
	m.Update(key("back"))
	if m.fields()[m.step].key != "connect" || m.values["connect"] != connectPaired {
		t.Fatal("back navigation lost the answer")
	}
	// Cancel before apply: nothing written.
	_, cmd := m.Update(key("esc"))
	if cmd == nil || m.applied {
		t.Fatal("cancel")
	}
	if entries, _ := os.ReadDir(filepath.Dir(m.path)); len(entries) != 0 {
		t.Fatal("cancel wrote files")
	}
}

// fakeAdmin answers the gateway control actions setup uses, without a service.
type fakeAdmin struct {
	calls     []string
	failPeers int // peers queries that fail before the gateway is "ready"
	invite    error
	join      error
	connected bool
}

func (f *fakeAdmin) call(_ context.Context, runtime string, cmd channel.GatewayCommand) (json.RawMessage, error) {
	f.calls = append(f.calls, cmd.Action)
	if runtime != "/run" {
		return nil, errors.New("wrong runtime")
	}
	switch cmd.Action {
	case "peers":
		if f.failPeers > 0 {
			f.failPeers--
			return nil, errors.New("gateway service unavailable")
		}
		return json.RawMessage(`[{"host_id":"` + strings.Repeat("b", 64) + `","endpoint":"127.0.0.1:1","connected":` + strconv.FormatBool(f.connected) + `,"pending":false}]`), nil
	case "invite":
		if f.invite != nil {
			return nil, f.invite
		}
		if cmd.Expires != setupInviteExpiry {
			return nil, errors.New("unexpected expiry")
		}
		return json.RawMessage(`{"invitation":"rhz1_secret","id":"inv1"}`), nil
	case "join":
		if f.join != nil {
			return nil, f.join
		}
		if cmd.Code != "rhz1_code" {
			return nil, errors.New("unexpected code " + cmd.Code)
		}
		return json.RawMessage(`{"host_id":"` + strings.Repeat("b", 64) + `","status":"connected"}`), nil
	}
	return nil, errors.New("unexpected action")
}

// pairedWizard stages paired devices with the service accepted and a running
// fake service, ready for the pairing phase.
func pairedWizard(t *testing.T, admin *fakeAdmin) *setupModel {
	t.Helper()
	m := wizard(t)
	m.values["connect"], m.values["transport"], m.values["service"] = connectPaired, "lan", "yes"
	m.installService = func(context.Context, string) (serviceOutcome, error) {
		return serviceOutcome{Installed: true, Running: true, Runtime: "/run"}, nil
	}
	m.admin = admin.call
	return m
}

func TestWizardPairingPhase(t *testing.T) {
	t.Run("declined service prints the commands for later", func(t *testing.T) {
		admin := &fakeAdmin{}
		m := pairedWizard(t, admin)
		m.values["service"] = "no"
		r := m.applyAnswers(context.Background())
		joined := strings.Join(r.steps, "\n")
		if r.err != nil || r.pairing != "" || len(admin.calls) != 0 || !strings.Contains(joined, "rhizome service install --config "+m.path) || !strings.Contains(joined, "rhizome invite") || !strings.Contains(joined, "rhizome join <invitation>") {
			t.Fatal(r, admin.calls)
		}
	})
	t.Run("service start failure stops before pairing", func(t *testing.T) {
		admin := &fakeAdmin{}
		m := pairedWizard(t, admin)
		m.installService = func(context.Context, string) (serviceOutcome, error) {
			return serviceOutcome{Installed: true}, errors.New("observer did not acquire its runtime lock")
		}
		r := m.applyAnswers(context.Background())
		if r.err == nil || !strings.Contains(r.err.Error(), "completed steps remain") || r.pairing != "" || len(admin.calls) != 0 {
			t.Fatal(r, admin.calls)
		}
		if _, err := os.Stat(m.path); err != nil {
			t.Fatal("written configuration missing", err)
		}
	})
	t.Run("already running service needs a restart first", func(t *testing.T) {
		admin := &fakeAdmin{}
		m := pairedWizard(t, admin)
		m.installService = func(context.Context, string) (serviceOutcome, error) {
			return serviceOutcome{Installed: true, Running: true, WasRunning: true, Runtime: "/run"}, nil
		}
		r := m.applyAnswers(context.Background())
		joined := strings.Join(r.steps, "\n")
		if r.err != nil || r.pairing != "" || len(admin.calls) != 0 || !strings.Contains(joined, "rhizome service restart") || !strings.Contains(joined, "rhizome invite") {
			t.Fatal(r, admin.calls)
		}
	})
	t.Run("gateway readiness timeout defers pairing", func(t *testing.T) {
		admin := &fakeAdmin{failPeers: 1 << 20}
		m := pairedWizard(t, admin)
		ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
		defer cancel()
		r := m.applyAnswers(ctx)
		joined := strings.Join(r.steps, "\n")
		if r.err != nil || r.pairing != "" || !strings.Contains(joined, "gateway not ready") || !strings.Contains(joined, "rhizome invite") || len(admin.calls) < 1 {
			t.Fatal(r, len(admin.calls))
		}
		for _, c := range admin.calls {
			if c != "peers" {
				t.Fatal("enrollment attempted while not ready", admin.calls)
			}
		}
	})
	t.Run("ready after retries then invitation", func(t *testing.T) {
		admin := &fakeAdmin{failPeers: 2}
		m := pairedWizard(t, admin)
		gotoField(t, m, "review")
		_, cmd := m.Update(key("enter"))
		m.Update(cmd())
		if m.err != nil || !m.done || m.result.pairing != "/run" || m.pair.closed {
			t.Fatalf("%+v %+v", m.result, m.pair)
		}
		view := m.View()
		if !strings.Contains(view, "Pair this device now?") || !strings.Contains(view, "create an invitation") || !strings.Contains(view, "> later") {
			t.Fatal(view)
		}
		if _, quit := m.Update(key("enter")); quit != nil && m.pair.closed == false {
			t.Fatal("enter on later did not defer")
		}
		if !m.pair.closed || !strings.Contains(m.View(), "rhizome invite") || !strings.Contains(m.View(), "Setup completed. Enter exits.") {
			t.Fatal(m.View())
		}
		if _, quit := m.Update(key("enter")); quit == nil {
			t.Fatal("exit")
		}
		// Invitation path on a fresh model.
		admin = &fakeAdmin{}
		m = pairedWizard(t, admin)
		m.Update(setupResult{steps: []string{"Wrote"}, pairing: "/run"})
		m.Update(key("up"))
		m.Update(key("up"))
		_, cmd = m.Update(key("enter"))
		if cmd == nil || !m.pair.busy {
			t.Fatal("invite not dispatched")
		}
		if _, again := m.Update(key("enter")); again != nil {
			t.Fatal("duplicate invite")
		}
		m.Update(cmd())
		view = m.View()
		if m.pair.busy || !m.pair.closed || !strings.Contains(view, "rhizome join rhz1_secret") || !strings.Contains(view, "no peer is connected yet") || strings.Contains(view, "connected.") {
			t.Fatal(view)
		}
		if admin.calls[len(admin.calls)-1] != "invite" {
			t.Fatal(admin.calls)
		}
	})
	t.Run("join flow with retry and connection state", func(t *testing.T) {
		admin := &fakeAdmin{join: errors.New("enrollment rejected (HTTP 404); invitation may be expired, revoked or consumed")}
		m := pairedWizard(t, admin)
		m.Update(setupResult{steps: []string{"Wrote"}, pairing: "/run"})
		m.Update(key("up")) // later -> join
		m.Update(key("enter"))
		if !m.pair.editing || !strings.Contains(m.View(), "Invitation (paste the code") {
			t.Fatal(m.View())
		}
		_, cmd := m.Update(key("enter")) // empty code never reaches the service
		if cmd != nil || m.pair.err == nil || len(admin.calls) != 0 {
			t.Fatal(m.pair.err, admin.calls)
		}
		for _, r := range "rhizome join rhz1_code" {
			m.Update(key(string(r)))
		}
		_, cmd = m.Update(key("enter"))
		if cmd == nil || !m.pair.busy {
			t.Fatal("join not dispatched")
		}
		m.Update(cmd())
		if m.pair.busy || m.pair.closed || m.pair.err == nil || !strings.Contains(m.View(), "expired, revoked or consumed") || !strings.Contains(m.View(), "retry here") {
			t.Fatal(m.View())
		}
		if err := runSetupTUIResult(m); err == nil || !strings.Contains(err.Error(), "pairing failed after setup completed") {
			t.Fatal(err)
		}
		// Retry with a fresh invitation on the same screen: pending, then connected.
		admin.join = nil
		m.pair.editing = true
		_, cmd = m.Update(key("enter"))
		m.Update(cmd())
		if m.pair.err != nil || !m.pair.closed || !strings.Contains(m.View(), "Paired with host "+strings.Repeat("b", 64)+": trust saved, connection not established yet") {
			t.Fatal(m.View())
		}
		if got := admin.calls[len(admin.calls)-2:]; got[0] != "join" || got[1] != "peers" {
			t.Fatal(admin.calls)
		}
		if err := runSetupTUIResult(m); err != nil {
			t.Fatal(err)
		}
		admin = &fakeAdmin{connected: true}
		m = pairedWizard(t, admin)
		m.Update(setupResult{steps: []string{"Wrote"}, pairing: "/run"})
		m.Update(key("up"))
		m.Update(key("enter"))
		for _, r := range "rhz1_code" {
			m.Update(key(string(r)))
		}
		_, cmd = m.Update(key("enter"))
		m.Update(cmd())
		if !strings.Contains(m.View(), ": connected.") {
			t.Fatal(m.View())
		}
	})
	t.Run("interrupted pairing is reported", func(t *testing.T) {
		m := pairedWizard(t, &fakeAdmin{})
		m.Update(setupResult{steps: []string{"Wrote"}, pairing: "/run"})
		m.pair.busy = true
		if err := runSetupTUIResult(m); err == nil || !strings.Contains(err.Error(), "pairing interrupted") {
			t.Fatal(err)
		}
	})
	t.Run("no pairing offer for local or fixed answers", func(t *testing.T) {
		m := wizard(t)
		m.values["service"] = "yes"
		admin := &fakeAdmin{}
		m.admin = admin.call
		m.installService = func(context.Context, string) (serviceOutcome, error) {
			return serviceOutcome{Installed: true, Running: true, Runtime: "/run"}, nil
		}
		r := m.applyAnswers(context.Background())
		if r.err != nil || r.pairing != "" || len(admin.calls) != 0 || strings.Contains(strings.Join(r.steps, "\n"), "invite") {
			t.Fatal(r)
		}
		m.Update(r)
		if !m.pair.closed || !strings.Contains(m.View(), "Setup completed. Enter exits.") {
			t.Fatal(m.View())
		}
	})
}

// runSetupTUIResult evaluates the exit status of a model whose program loop ended.
func runSetupTUIResult(m *setupModel) error {
	m.applied, m.done = true, true
	return m.exitStatus(nil)
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
