package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"rhizome/internal/config"
)

func storedDocument(t *testing.T, path string) map[string]any {
	t.Helper()
	var d map[string]any
	if _, err := toml.DecodeFile(path, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestSetupPreservesStoredConfiguration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("RHIZOME_CONFIG", "")
	path := filepath.Join(home, "custom.toml")
	body := `hostname = "stored host"
runtime_dir = "` + filepath.Join(home, "state") + `"
[network]
mode = "local"
[gateway]
mode = "lan"
listen = "127.0.0.1:7947"
advertise = "127.0.0.2:7947"
[gateway.history]
enabled = true
lookback = "45d"
[claude]
config_dir = "~/claude"
projects_dir = "~/projects"
[claude.peer_history]
enabled = false
lookback = "all"
[codex]
home = "~/codex"
bin = "custom-codex"
sessions_dir = "~/sessions"
[codex.peer_history]
enabled = true
lookback = "15d"
`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	before := storedDocument(t, path)
	t.Setenv("RHIZOME_RUNTIME_DIR", filepath.Join(home, "shell-state"))
	t.Setenv("RHIZOME_GATEWAY_MODE", "off")
	t.Setenv("CODEX_HOME", filepath.Join(home, "shell-codex"))
	for i := 0; i < 2; i++ {
		if err := setup([]string{"--config", path, "--network", "local", "--replace-config"}); err != nil {
			t.Fatal(err)
		}
		if after := storedDocument(t, path); !reflect.DeepEqual(before, after) {
			t.Fatalf("lost stored settings:\nbefore=%v\nafter=%v", before, after)
		}
	}
	backups, _ := filepath.Glob(path + ".bak-*")
	if len(backups) != 2 {
		t.Fatalf("backups: %v", backups)
	}
	original, _ := os.ReadFile(backups[0])
	if string(original) != body {
		t.Fatal("original backup changed")
	}
	var out bytes.Buffer
	if _, err := setupConfig(config.Config{}, path, nil, false, true, strings.NewReader("replace\n"+strings.Repeat("\n", 7)), &out, &out); err != nil {
		t.Fatal(err)
	}
	if after := storedDocument(t, path); !reflect.DeepEqual(before, after) {
		t.Fatalf("wizard lost settings: %v", after)
	}
	if err := setup([]string{"--config", path, "--gateway-mode", "off", "--replace-config"}); err != nil {
		t.Fatal(err)
	}
	g := storedDocument(t, path)["gateway"].(map[string]any)
	if g["mode"] != "off" || g["listen"] != nil || g["advertise"] != nil || !reflect.DeepEqual(g["history"], before["gateway"].(map[string]any)["history"]) {
		t.Fatalf("disable: %v", g)
	}
}

func TestSetupGatewayFlags(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantError string
	}{
		{"off", []string{"--gateway-mode", "off"}, ""},
		{"lan", []string{"--gateway-mode", "lan", "--gateway-listen", "192.168.1.2:7947", "--gateway-advertise", "192.168.1.3:7948"}, ""},
		{"tailscale", []string{"--gateway-mode", "tailscale", "--gateway-listen", "100.64.1.2:7947"}, ""},
		{"conflict", []string{"--network", "lan", "--gateway-mode", "lan"}, "requires network.mode=local"},
		{"public", []string{"--gateway-mode", "lan", "--gateway-listen", "8.8.8.8:7947"}, "address policy"},
		{"wrong transport", []string{"--gateway-mode", "tailscale", "--gateway-listen", "192.168.1.2:7947"}, "address policy"},
		{"wildcard", []string{"--gateway-mode", "lan", "--gateway-listen", "0.0.0.0:7947"}, "concrete"},
		{"dns", []string{"--gateway-mode", "lan", "--gateway-listen", "localhost:7947"}, "concrete"},
		{"off address", []string{"--gateway-mode", "off", "--gateway-listen", "127.0.0.1:7947"}, "require mode"},
		{"unknown", []string{"--gateway-mode", "invalid"}, "must be off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("RHIZOME_CONFIG", "")
			path := filepath.Join(home, "custom", "config.toml")
			args := append([]string{"--config", path, "--runtime-dir", filepath.Join(home, "runtime")}, tc.args...)
			err := setup(args)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("got %v", err)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("invalid config written")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			c, err := config.LoadForSetup(path, func(string) string { return "" })
			if err != nil {
				t.Fatal(err)
			}
			if c.Network.Mode != "local" || c.Gateway.History.Enabled || c.RuntimeDir != filepath.Join(home, "runtime") {
				t.Fatalf("unexpected defaults: %+v", c)
			}
		})
	}
}

func TestGatewayWizard(t *testing.T) {
	for _, mode := range []string{"off", "lan", "tailscale"} {
		t.Run(mode, func(t *testing.T) {
			c := resolved(t)
			input := mode + "\n"
			if mode != "off" {
				input += "\n\n"
			}
			input += "\n\n\n"
			var out bytes.Buffer
			a, err := askConfig(prompter{in: bufioReader(input), out: &out}, c, func() error { return os.ErrNotExist })
			if err != nil {
				t.Fatal(err)
			}
			body, err := a.render()
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.toml")
			if _, err := writeConfig(path, body, false, time.Now()); err != nil {
				t.Fatal(err)
			}
			loaded, err := config.LoadForSetup(path, func(string) string { return "" })
			if err != nil || loaded.Gateway.Mode != mode || loaded.Network.Mode != "local" {
				t.Fatalf("%+v %v", loaded, err)
			}
			if mode == "tailscale" && !strings.Contains(out.String(), "service will refuse") {
				t.Fatal("missing warning")
			}
		})
	}
}

func TestSetupRejectsInvalidStoredFile(t *testing.T) {
	for _, body := range []string{"[broken", "unknown = true", "[gateway.history]\nenabled = 'yes'"} {
		t.Run(body, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			path := filepath.Join(t.TempDir(), "config.toml")
			os.WriteFile(path, []byte(body), 0600)
			var out bytes.Buffer
			_, err := setupConfig(config.Config{}, path, &configAnswers{Network: config.Network{Mode: "local"}}, true, false, nil, &out, &out)
			if err == nil {
				t.Fatal("accepted invalid stored file")
			}
			got, _ := os.ReadFile(path)
			if string(got) != body {
				t.Fatal("damaged old file")
			}
		})
	}
}

func TestWriteConfigBackupFailureKeepsOriginal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.toml")
	original := "# original\n"
	os.WriteFile(path, []byte(original), 0600)
	now := time.Now()
	// A non-empty directory at the backup name must never be renamed over.
	os.Mkdir(path+".bak-"+now.UTC().Format("20060102T150405Z"), 0700)
	backup, err := writeConfig(path, "[network]\nmode='local'", true, now)
	if err != nil || !strings.HasSuffix(backup, "-1") {
		t.Fatalf("%s %v", backup, err)
	}
	// Refuse non-regular destinations without moving or replacing them.
	directory := filepath.Join(t.TempDir(), "config.toml")
	os.Mkdir(directory, 0700)
	if _, err := writeConfig(directory, "", true, now); err == nil {
		t.Fatal("replaced directory")
	}
	if st, err := os.Stat(directory); err != nil || !st.IsDir() {
		t.Fatal("damaged destination")
	}
}

func TestSetupPreservesLegacyNetwork(t *testing.T) {
	for _, mode := range []string{"lan", "tailscale"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			path := filepath.Join(t.TempDir(), "config.toml")
			body := "[network]\nmode='" + mode + "'\nbind_addr='127.0.0.1'\nadvertise_addr='127.0.0.2'\njoin=['127.0.0.3:7946']\nmdns=false\n"
			if mode == "tailscale" {
				body += "gossip_port=7950\ndiscovery_interval='45s'\n"
			}
			os.WriteFile(path, []byte(body), 0600)
			before := storedDocument(t, path)
			if err := setup([]string{"--config", path, "--network", mode, "--hostname", "new label", "--replace-config"}); err != nil {
				t.Fatal(err)
			}
			after := storedDocument(t, path)
			if !reflect.DeepEqual(before["network"], after["network"]) {
				t.Fatalf("lost network options: %v", after)
			}
			// Gateway enable cannot silently switch an existing gossip network.
			if err := setup([]string{"--config", path, "--gateway-mode", "lan", "--replace-config"}); err == nil {
				t.Fatal("silently switched transport")
			}
			if err := setup([]string{"--config", path, "--network", "local", "--gateway-mode", "lan", "--replace-config"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSetupNewCustomPathIgnoresDefaultFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.Mkdir(filepath.Join(home, ".rhizome"), 0700)
	os.WriteFile(filepath.Join(home, ".rhizome", "config.toml"), []byte("unknown=true"), 0600)
	path := filepath.Join(home, "new.toml")
	if err := setup([]string{"--config", path, "--gateway-mode", "lan"}); err != nil {
		t.Fatal(err)
	}
}

func TestWriteConfigFailedBackupPreservesFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// The destination fits NAME_MAX, but its backup name cannot: fail before
	// replacement even on privileged test runners where chmod is ineffective.
	path := filepath.Join(t.TempDir(), strings.Repeat("c", 250))
	original := "# original\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Skipf("filesystem cannot create fixture: %v", err)
	}
	if _, err := writeConfig(path, "[network]\nmode='local'", true, time.Now()); err == nil {
		t.Fatal("expected backup failure")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != original {
		t.Fatalf("original damaged: %q %v", got, err)
	}
}

func TestSetupAgentEnvironmentDoesNotEnterFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "native-codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "native-claude"))
	path := filepath.Join(home, "config.toml")
	if err := setup([]string{"--config", path, "--network", "local", "--agents", "all"}); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"native-codex", "native-claude"} {
		if _, err := os.Stat(filepath.Join(home, dir, "skills", skillName, "SKILL.md")); err != nil {
			t.Fatal(err)
		}
	}
	d := storedDocument(t, path)
	if d["codex"] != nil || d["claude"] != nil {
		t.Fatalf("persisted environment: %v", d)
	}
}

// The terminal screen edits the same stored document: replacing a legacy file
// through it must keep gateway and history settings it never asks about.
func TestSetupModelPreservesStoredSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "hostname = \"old\"\n[gateway]\nmode = \"lan\"\nlisten = \"127.0.0.1:7947\"\n[gateway.history]\nenabled = true\nlookback = \"45d\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newSetupModel(config.Config{}, path, nil, []bool{}, nil, false)
	m.values["existing"], m.values["mode"], m.values["hostname"] = "replace", "local", ""
	a, err := m.answers()
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.render()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`mode = "lan"`, `listen = "127.0.0.1:7947"`, `lookback = "45d"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("lost %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "hostname") {
		t.Fatalf("blank hostname did not clear:\n%s", out)
	}
}
