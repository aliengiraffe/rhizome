package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rhizome/internal/config"
	"rhizome/internal/service"
)

func TestAskConfigAutoUpdate(t *testing.T) {
	cases := []struct {
		name, stored, answer string
		wantKey              bool   // the rendered file carries update.enabled
		wantValue            string // its value when carried
	}{
		{"default keeps file free of the key", "", "", false, ""},
		{"explicit yes on a fresh file", "", "y", false, ""},
		{"disable", "", "n", true, "false"},
		{"stored false kept by default", "[update]\nenabled = false\n", "", true, "false"},
		{"stored false re-enabled", "[update]\nenabled = false\n", "y", true, "true"},
		{"stored true disabled", "[update]\nenabled = true\n", "no", true, "false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.stored), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := config.LoadForSetup(path, func(string) string { return "" })
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			a, err := askConfig(prompter{in: bufioReader("off\nlocal\n\n" + tc.answer + "\nn\n"), out: &out}, c, func() error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "Install verified updates automatically") || !strings.Contains(out.String(), "github.com") {
				t.Fatalf("question missing:\n%s", &out)
			}
			stored, err := storedConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			a.Stored = stored
			body, err := a.render()
			if err != nil {
				t.Fatal(err)
			}
			has := strings.Contains(body, "enabled = ")
			if has != tc.wantKey || has && !strings.Contains(body, "enabled = "+tc.wantValue) {
				t.Fatalf("rendered:\n%s", body)
			}
			if _, err := config.Preview(body); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSetupAutoUpdateFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RHIZOME_CONFIG", "")
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := setup([]string{"--config", path, "--network", "local", "--auto-update", "false"}); err != nil {
		t.Fatal(err)
	}
	c, err := config.Resolve(map[string]string{"config": path}, func(string) string { return "" })
	if err != nil || c.AutoUpdate.Enabled || c.Sources["update.enabled"] != "file" {
		t.Fatalf("%+v %v", c.AutoUpdate, err)
	}
	// A later edit that does not mention updates preserves the stored choice.
	if err := setup([]string{"--config", path, "--network", "local", "--hostname", "box", "--replace-config"}); err != nil {
		t.Fatal(err)
	}
	c, err = config.Resolve(map[string]string{"config": path}, func(string) string { return "" })
	if err != nil || c.AutoUpdate.Enabled || c.Hostname != "box" {
		t.Fatalf("%+v %v", c, err)
	}
	if err := setup([]string{"--config", path, "--network", "local", "--auto-update", "on", "--replace-config"}); err != nil {
		t.Fatal(err)
	}
	c, err = config.Resolve(map[string]string{"config": path}, func(string) string { return "" })
	if err != nil || !c.AutoUpdate.Enabled || c.Sources["update.enabled"] != "file" {
		t.Fatalf("%+v %v", c.AutoUpdate, err)
	}
	if err := setup([]string{"--config", path, "--network", "local", "--auto-update", "maybe", "--replace-config"}); err == nil || !strings.Contains(err.Error(), "--auto-update") {
		t.Fatalf("got %v", err)
	}
	if err := setup([]string{"--auto-update", "false"}); err == nil || !strings.Contains(err.Error(), "requires --network") {
		t.Fatalf("got %v", err)
	}
}

func TestWizardAutoUpdateField(t *testing.T) {
	m := wizard(t)
	keys := []string{}
	for _, f := range m.fields() {
		keys = append(keys, f.key)
	}
	if !strings.Contains(strings.Join(keys, ","), "hostname,updates,custom") {
		t.Fatalf("fields %v", keys)
	}
	if m.values["updates"] != "yes" {
		t.Fatalf("default %q", m.values["updates"])
	}
	// Default answer: the file stays free of the key.
	a, err := m.answers()
	if err != nil || a.AutoUpdate != nil {
		t.Fatalf("%+v %v", a, err)
	}
	m.values["updates"] = "no"
	a, err = m.answers()
	if err != nil || a.AutoUpdate == nil || *a.AutoUpdate || !a.Selected["auto-update"] {
		t.Fatalf("%+v %v", a, err)
	}
	if !strings.Contains(m.review(), "Automatic updates: no") {
		t.Fatal(m.review())
	}
	r := m.applyAnswers(context.Background())
	if r.err != nil {
		t.Fatal(r.err)
	}
	c, err := config.Resolve(map[string]string{"config": m.path}, func(string) string { return "" })
	if err != nil || c.AutoUpdate.Enabled || c.Sources["update.enabled"] != "file" {
		t.Fatalf("%+v %v", c.AutoUpdate, err)
	}
	// Reopening the wizard on that file defaults to the stored choice and can re-enable.
	m2 := newSetupModel(c, m.path, m.agents, nil, nil, true)
	t.Cleanup(m2.work.close)
	m2.exists = true
	m2.values["existing"] = "replace"
	if m2.values["updates"] != "no" {
		t.Fatalf("stored default %q", m2.values["updates"])
	}
	m2.values["updates"] = "yes"
	a, err = m2.answers()
	if err != nil || a.AutoUpdate == nil || !*a.AutoUpdate {
		t.Fatalf("%+v %v", a, err)
	}
}

func TestUpdateSummary(t *testing.T) {
	service.SetBuild("1.2.3", "")
	t.Cleanup(func() { service.SetBuild("", "") })
	if got := updateSummary(nil); !strings.Contains(got, "Version: 1.2.3") || !strings.Contains(got, "no check recorded") {
		t.Fatal(got)
	}
	raw, _ := json.Marshal(map[string]any{"version": "1.2.3", "last_check_at": "2026-10-23T10:00:00Z", "last_outcome": "not_writable", "last_detail": "evil\x1b[31m text", "available_version": "1.3.0", "blocked": "not_writable", "pending": map[string]string{"from": "1.2.3", "to": "1.3.0"}, "next_check_at": "2026-10-24T10:00:00Z"})
	got := updateSummary(raw)
	for _, want := range []string{"Last update check: 2026-10-23T10:00:00Z", "Last update outcome: not_writable (evil[31m text)", "Update available: 1.3.0", "Update blocked: not_writable", "Update pending restart: 1.2.3 -> 1.3.0", "Next update check: 2026-10-24T10:00:00Z"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b") {
		t.Fatal("escape sequence not sanitized")
	}
	if got := updateSummary(json.RawMessage("{")); !strings.Contains(got, "unreadable") {
		t.Fatal(got)
	}
}
