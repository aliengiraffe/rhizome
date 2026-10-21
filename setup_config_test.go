package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"rhizome/internal/channel"
	"rhizome/internal/config"
	"runtime"
	"strings"
	"testing"
	"time"
)

func noEnv(string) (string, bool) { return "", false }

func TestApplyNetwork(t *testing.T) {
	off := false
	file := config.Network{Mode: config.ModeTailscale, Join: []string{"f:7946"}, GossipPort: 7950, DiscoveryInterval: "45s", MDNS: &off}
	env := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	t.Run("file fills unset", func(t *testing.T) {
		o := channel.Options{MDNS: true}
		applyNetwork(&o, file, nil, noEnv)
		if !o.Tailscale || o.LAN || o.GossipPort != 7950 || o.Discovery != 45*time.Second || o.MDNS || len(o.Join) != 1 {
			t.Fatalf("%+v", o)
		}
	})
	t.Run("local file keeps loopback", func(t *testing.T) {
		o := channel.Options{MDNS: true}
		applyNetwork(&o, config.Network{}, nil, noEnv)
		if o.LAN || o.Tailscale || o.BindAddr != "" || len(o.Join) != 0 || !o.MDNS {
			t.Fatalf("%+v", o)
		}
	})
	t.Run("env beats file", func(t *testing.T) {
		o := channel.Options{LAN: true, GossipPort: 0, Join: []string{"env:1"}, MDNS: true}
		applyNetwork(&o, file, nil, env(map[string]string{"RHIZOME_LAN": "1", "RHIZOME_JOIN": "env:1", "RHIZOME_GOSSIP_PORT": "9000"}))
		if o.Tailscale || !o.LAN || len(o.Join) != 1 || o.Join[0] != "env:1" || o.GossipPort == 7950 {
			t.Fatalf("%+v", o)
		}
	})
	t.Run("flag beats file", func(t *testing.T) {
		o := channel.Options{GossipPort: 9100, MDNS: true, Tailscale: true}
		applyNetwork(&o, file, map[string]bool{"gossip-port": true, "mdns": true, "tailscale": true}, noEnv)
		if o.GossipPort != 9100 || !o.MDNS || !o.Tailscale {
			t.Fatalf("%+v", o)
		}
	})
}

func TestRenderAndWriteConfig(t *testing.T) {
	off := false
	cases := []struct {
		name string
		a    configAnswers
		want []string
		not  []string
	}{
		{"local", configAnswers{}, []string{`mode = "local"`}, []string{"bind_addr", "join", "[claude]", "[codex]", "runtime_dir"}},
		{"lan", configAnswers{Network: config.Network{Mode: "lan", BindAddr: "192.168.1.5", Join: []string{"a:7946"}, MDNS: &off}},
			[]string{`mode = "lan"`, `bind_addr = "192.168.1.5"`, `join = ["a:7946"]`, "mdns = false"}, []string{"gossip_port", "token"}},
		{"tailscale", configAnswers{Network: config.Network{Mode: "tailscale", GossipPort: 7950, DiscoveryInterval: "45s"}},
			[]string{`mode = "tailscale"`, "gossip_port = 7950", `discovery_interval = "45s"`}, []string{"bind_addr", "mdns"}},
		{"paths", configAnswers{RuntimeDir: "/r", ClaudeDir: "/c", CodexHome: "/h", CodexBin: "mycodex"},
			[]string{`runtime_dir = "/r"`, "[claude]", `config_dir = "/c"`, "[codex]", `bin = "mycodex"`, `home = "/h"`}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			body, err := tc.a.render()
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(body, w) {
					t.Fatalf("missing %q in\n%s", w, body)
				}
			}
			var data []string // the header comments mention the token; keys must not
			for _, l := range strings.Split(body, "\n") {
				if !strings.HasPrefix(l, "#") {
					data = append(data, l)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(strings.Join(data, "\n"), n) {
					t.Fatalf("unexpected %q in\n%s", n, body)
				}
			}
			path := filepath.Join(t.TempDir(), "sub", "config.toml")
			if _, err := writeConfig(path, body, false, time.Now()); err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" {
				if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
					t.Fatalf("mode %v", st.Mode())
				}
				if st, _ := os.Stat(filepath.Dir(path)); st.Mode().Perm() != 0o700 {
					t.Fatalf("dir mode %v", st.Mode())
				}
			}
			c, err := config.Resolve(map[string]string{"config": path}, func(string) string { return "" })
			if err != nil {
				t.Fatalf("written file does not load: %v\n%s", err, body)
			}
			want := tc.a.Network.Mode
			if want == "" {
				want = "local"
			}
			if c.Network.Mode != want || c.Sources["network.mode"] != "file" {
				t.Fatalf("%+v %v", c.Network, c.Sources)
			}
		})
	}
}

func TestWriteConfigRefusesAndBacksUp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("# mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := writeConfig(path, "[network]\nmode = \"local\"\n", false, time.Now()); err == nil {
		t.Fatal("overwrote without replace")
	}
	if b, _ := os.ReadFile(path); string(b) != "# mine\n" {
		t.Fatalf("existing file changed: %q", b)
	}
	if _, err := writeConfig(path, "[network]\nmode = \"bogus\"\n", true, time.Now()); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("invalid body accepted: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "# mine\n" {
		t.Fatal("invalid replacement damaged the existing file")
	}
	backup, err := writeConfig(path, "[network]\nmode = \"local\"\n", true, time.Date(2026, 10, 20, 1, 2, 3, 0, time.UTC))
	if err != nil || !strings.HasSuffix(backup, ".bak-20261020T010203Z") {
		t.Fatalf("%q %v", backup, err)
	}
	if b, _ := os.ReadFile(backup); string(b) != "# mine\n" {
		t.Fatalf("backup content %q", b)
	}
}

func resolved(t *testing.T) config.Config {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	c, err := config.Resolve(nil, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAskConfig(t *testing.T) {
	ok := func() error { return nil }
	cases := []struct {
		name, input string
		ts          func() error
		check       func(configAnswers) bool
		wantOut     string
	}{
		{"defaults are local", "\n\n\n", ok, func(a configAnswers) bool { return a.Network.Mode == "local" && a.RuntimeDir == "" }, ""},
		{"numeric local", "1\n\nn\n", ok, func(a configAnswers) bool { return a.Network.Mode == "local" }, ""},
		{"lan", "lan\n192.168.1.5\n\na:7946, b:7946\nn\n\nn\n", ok, func(a configAnswers) bool {
			n := a.Network
			return n.Mode == "lan" && n.BindAddr == "192.168.1.5" && n.AdvertiseAddr == "" && len(n.Join) == 2 && n.MDNS != nil && !*n.MDNS
		}, "never written"},
		{"lan keeps default mdns unset", "2\n\n\n\n\n\nn\n", ok, func(a configAnswers) bool { return a.Network.Mode == "lan" && a.Network.MDNS == nil }, ""},
		{"tailscale defaults", "3\n\n\n\n\nn\n", ok, func(a configAnswers) bool {
			n := a.Network
			return n.Mode == "tailscale" && n.GossipPort == 0 && n.DiscoveryInterval == ""
		}, ""},
		{"tailscale custom and warning", "tailscale\n7950\n45s\nhost.ts.net:7946\n\nn\n", func() error { return errors.New("tailscale is not running") }, func(a configAnswers) bool {
			n := a.Network
			return n.GossipPort == 7950 && n.DiscoveryInterval == "45s" && len(n.Join) == 1
		}, "Warning: tailscale is not running"},
		{"retry bad choice", "wan\n1\n\nn\n", ok, func(a configAnswers) bool { return a.Network.Mode == "local" }, "Choose 1, 2 or 3"},
		{"paths only when non-default", "1\n\ny\n\n/c\n\n\n", ok, func(a configAnswers) bool {
			return a.ClaudeDir == "/c" && a.RuntimeDir == "" && a.CodexHome == "" && a.CodexBin == ""
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := resolved(t)
			var out bytes.Buffer
			a, err := askConfig(prompter{in: bufioReader(tc.input), out: &out}, c, tc.ts)
			if err != nil || !tc.check(a) || !strings.Contains(out.String(), tc.wantOut) {
				t.Fatalf("%+v %v\n%s", a, err, out.String())
			}
		})
	}
	t.Run("input ends", func(t *testing.T) {
		if _, err := askConfig(prompter{in: bufioReader(""), out: &bytes.Buffer{}}, resolved(t), ok); err == nil {
			t.Fatal("EOF accepted")
		}
	})
}

// fakeInstaller records service step calls without touching a service manager.
type fakeInstaller struct {
	paths []string
	err   error
}

func (f *fakeInstaller) install(_ context.Context, path string) (serviceOutcome, error) {
	f.paths = append(f.paths, path)
	if f.err != nil {
		return serviceOutcome{}, f.err
	}
	return serviceOutcome{Installed: true, Running: true, Runtime: "/run"}, nil
}

func TestSetupConfig(t *testing.T) {
	lan := func() *configAnswers {
		return &configAnswers{Network: config.Network{Mode: "lan", Join: []string{"a:7946"}}}
	}
	path := func(t *testing.T) string {
		t.Setenv("HOME", t.TempDir())
		return filepath.Join(t.TempDir(), "config.toml")
	}
	never := func(context.Context, string) (serviceOutcome, error) {
		t.Error("service step ran implicitly")
		return serviceOutcome{}, nil
	}
	// run drives the plain flow like setup: the configuration step, then the
	// service question on the same reader when the interactive questions ran.
	run := func(t *testing.T, c config.Config, p string, answers *configAnswers, replace, tty bool, input string, out, errOut *bytes.Buffer, install serviceInstaller) error {
		t.Helper()
		in := bufioReader(input)
		wizard, err := setupConfig(c, p, answers, replace, tty, in, out, errOut)
		if err != nil || !wizard {
			return err
		}
		return offerService(prompter{in: in, out: out}, p, out, install)
	}
	t.Run("flags write without a terminal", func(t *testing.T) {
		p := path(t)
		var out, errOut bytes.Buffer
		if err := run(t, resolved(t), p, lan(), false, false, "", &out, &errOut, never); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(p)
		if err != nil || !strings.Contains(string(b), `mode = "lan"`) || strings.Contains(out.String(), "service") {
			t.Fatalf("%v %s %s", err, b, out.String())
		}
	})
	t.Run("no terminal and no network writes nothing", func(t *testing.T) {
		p := path(t)
		var out, errOut bytes.Buffer
		if err := run(t, resolved(t), p, nil, false, false, "", &out, &errOut, never); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) || !strings.Contains(errOut.String(), "--network") || out.Len() != 0 {
			t.Fatalf("%v %q %q", err, errOut.String(), out.String())
		}
	})
	t.Run("existing file kept without replace flag", func(t *testing.T) {
		p := path(t)
		os.WriteFile(p, []byte("# mine\n"), 0o600)
		var out, errOut bytes.Buffer
		if err := run(t, resolved(t), p, lan(), false, false, "", &out, &errOut, never); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(p); string(b) != "# mine\n" || !strings.Contains(errOut.String(), "--replace-config") {
			t.Fatalf("%q %q", b, errOut.String())
		}
	})
	t.Run("replace flag backs up", func(t *testing.T) {
		p := path(t)
		os.WriteFile(p, []byte("# mine\n"), 0o600)
		var out, errOut bytes.Buffer
		if err := run(t, resolved(t), p, lan(), true, false, "", &out, &errOut, never); err != nil {
			t.Fatal(err)
		}
		matches, _ := filepath.Glob(p + ".bak-*")
		if b, _ := os.ReadFile(p); len(matches) != 1 || !strings.Contains(string(b), "lan") {
			t.Fatalf("%v %s", matches, b)
		}
	})
	t.Run("interactive keep leaves file byte-identical", func(t *testing.T) {
		p := path(t)
		os.WriteFile(p, []byte("# mine\n"), 0o600)
		var out, errOut bytes.Buffer
		if err := run(t, resolved(t), p, nil, false, true, "\n\n", &out, &errOut, never); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(p); string(b) != "# mine\n" || !strings.Contains(out.String(), "# mine") {
			t.Fatalf("%q %q", b, out.String())
		}
		if o := out.String(); !strings.Contains(o, "kept existing "+p) || !strings.Contains(o, "Install and start the background service") || !strings.Contains(o, "service unchanged") {
			t.Fatalf("%q", o)
		}
	})
	t.Run("interactive keep then install and start", func(t *testing.T) {
		p := path(t)
		os.WriteFile(p, []byte("# mine\n"), 0o600)
		var out, errOut bytes.Buffer
		f := &fakeInstaller{}
		if err := run(t, resolved(t), p, nil, false, true, "keep\ny\n", &out, &errOut, f.install); err != nil {
			t.Fatal(err)
		}
		if len(f.paths) != 1 || f.paths[0] != p {
			t.Fatalf("installer calls %v", f.paths)
		}
		if b, _ := os.ReadFile(p); string(b) != "# mine\n" {
			t.Fatalf("kept file changed: %q", b)
		}
		if backups, _ := filepath.Glob(p + ".bak-*"); len(backups) != 0 {
			t.Fatalf("keep created backups %v", backups)
		}
		if o := out.String(); !strings.Contains(o, "installed and running (configuration "+p+", runtime /run)") {
			t.Fatalf("%q", o)
		}
	})
	t.Run("interactive replace then install and start", func(t *testing.T) {
		p := path(t)
		os.WriteFile(p, []byte("# mine\n"), 0o600)
		var out, errOut bytes.Buffer
		f := &fakeInstaller{}
		// replace, local, no hostname, no custom paths, install service.
		if err := run(t, resolved(t), p, nil, false, true, "replace\n1\n\nn\ny\n", &out, &errOut, f.install); err != nil {
			t.Fatal(err)
		}
		backups, _ := filepath.Glob(p + ".bak-*")
		if b, _ := os.ReadFile(p); len(backups) != 1 || !strings.Contains(string(b), `mode = "local"`) || len(f.paths) != 1 || f.paths[0] != p {
			t.Fatalf("%v %s %v", backups, b, f.paths)
		}
		if o := out.String(); !strings.Contains(o, "wrote "+p) || !strings.Contains(o, "installed and running") {
			t.Fatalf("%q", o)
		}
	})
	t.Run("service failure keeps written configuration", func(t *testing.T) {
		p := path(t)
		var out, errOut bytes.Buffer
		f := &fakeInstaller{err: errors.New("observer did not acquire its runtime lock")}
		err := run(t, resolved(t), p, nil, false, true, "1\n\nn\ny\n", &out, &errOut, f.install)
		if err == nil || !errors.Is(err, f.err) {
			t.Fatalf("got %v", err)
		}
		if b, _ := os.ReadFile(p); !strings.Contains(string(b), `mode = "local"`) {
			t.Fatalf("configuration lost on service failure: %s", b)
		}
		if o := out.String(); !strings.Contains(o, "wrote "+p) || strings.Contains(o, "running") {
			t.Fatalf("%q", o)
		}
	})
	t.Run("input ends at service question", func(t *testing.T) {
		p := path(t)
		os.WriteFile(p, []byte("# mine\n"), 0o600)
		var out, errOut bytes.Buffer
		if err := run(t, resolved(t), p, nil, false, true, "\n", &out, &errOut, never); err == nil {
			t.Fatal("EOF at the service question accepted")
		}
		if b, _ := os.ReadFile(p); string(b) != "# mine\n" {
			t.Fatalf("kept file changed: %q", b)
		}
	})
	t.Run("flags with a terminal never ask", func(t *testing.T) {
		p := path(t)
		var out, errOut bytes.Buffer
		if err := run(t, resolved(t), p, lan(), false, true, "y\n", &out, &errOut, never); err != nil {
			t.Fatal(err)
		}
		if o := out.String(); !strings.Contains(o, "wrote "+p) || strings.Contains(o, "service") {
			t.Fatalf("%q", o)
		}
	})
	t.Run("interactive wizard creates file", func(t *testing.T) {
		p := path(t)
		var out, errOut bytes.Buffer
		// local, no custom paths, no service install.
		if err := run(t, resolved(t), p, nil, false, true, "1\n\nn\nn\n", &out, &errOut, never); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(p); !strings.Contains(string(b), `mode = "local"`) {
			t.Fatalf("%s", b)
		}
		if o := out.String(); !strings.Contains(o, "Install and start the background service now (y/N)") {
			t.Fatalf("%q", o)
		}
	})
}

func bufioReader(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }

func TestSetupHostname(t *testing.T) {
	for _, hostname := range []string{"  My workstation  ", "   "} {
		t.Run(hostname, func(t *testing.T) {
			c := resolved(t)
			c.Hostname = "previous"
			var out bytes.Buffer
			a, err := askConfig(prompter{in: bufioReader("local\n" + hostname + "\nn\n"), out: &out}, c, func() error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if a.Hostname != strings.TrimSpace(hostname) || !strings.Contains(out.String(), "clears any previous override") {
				t.Fatalf("%+v %s", a, &out)
			}
			path := filepath.Join(t.TempDir(), "config.toml")
			old := "hostname = 'previous'\n"
			if err := os.WriteFile(path, []byte(old), 0600); err != nil {
				t.Fatal(err)
			}
			body, err := a.render()
			if err != nil {
				t.Fatal(err)
			}
			backup, err := writeConfig(path, body, true, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if b, err := os.ReadFile(backup); err != nil || string(b) != old {
				t.Fatalf("backup %q %v", b, err)
			}
			loaded, err := config.Resolve(map[string]string{"config": path}, func(string) string { return "" })
			if err != nil || loaded.Hostname != a.Hostname {
				t.Fatalf("%+v %v", loaded, err)
			}
			if a.Hostname == "" && strings.Contains(body, "hostname") {
				t.Fatal("persisted fallback")
			}
		})
	}
	var out bytes.Buffer
	a, err := askConfig(prompter{in: bufioReader("local\nbad\x00label\ngood\nn\n"), out: &out}, resolved(t), func() error { return nil })
	if err != nil || a.Hostname != "good" || !strings.Contains(out.String(), "control characters") {
		t.Fatalf("%+v %v %s", a, err, &out)
	}
	if _, err := (configAnswers{Hostname: strings.Repeat("x", 65)}).render(); err == nil {
		t.Fatal("invalid hostname rendered")
	}
}

func TestSetupHostnameFlags(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RHIZOME_CONFIG", "")
	path := filepath.Join(t.TempDir(), "config.toml")
	for _, value := range []string{"  Laptop  ", ""} {
		args := []string{"--config", path, "--network", "local", "--hostname", value}
		if value == "" {
			args = append(args, "--replace-config")
		}
		if err := setup(args); err != nil {
			t.Fatal(err)
		}
		c, err := config.Resolve(map[string]string{"config": path}, func(string) string { return "" })
		if err != nil || c.Hostname != strings.TrimSpace(value) {
			t.Fatalf("%q %v", c.Hostname, err)
		}
	}
	if err := setup([]string{"--hostname", "Laptop"}); err == nil || !strings.Contains(err.Error(), "requires --network") {
		t.Fatalf("got %v", err)
	}
}
