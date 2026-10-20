package config

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func fixture(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}
func noenv(string) string { return "" }

func TestLayers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var schema Config
	for _, s := range schema.settings() {
		t.Run(s.key, func(t *testing.T) {
			file, env, override := "/file", "/env", "/flag"
			if s.key == "codex.bin" {
				file, env, override = "file-codex", "env-codex", "flag-codex"
			}
			path := fixture(t, fmt.Sprintf("%s = %q\n", s.key, file))
			for _, layer := range []string{"default", "file", "env", "flag"} {
				if layer == "env" && s.env == "" || layer == "flag" && s.flag == "" {
					continue
				}
				flags := map[string]string{}
				vars := map[string]string{}
				if layer != "default" {
					flags["config"] = path
				}
				want := file
				if layer == "env" || layer == "flag" {
					if s.env != "" {
						vars[s.env] = env
					}
					want = env
				}
				if layer == "flag" {
					flags[s.flag] = override
					want = override
				}
				c, err := Resolve(flags, func(k string) string { return vars[k] })
				if err != nil {
					t.Fatal(err)
				}
				if c.Sources[s.key] != layer {
					t.Fatalf("%s: source %s", layer, c.Sources[s.key])
				}
				for _, got := range c.settings() {
					if got.key == s.key && layer != "default" && *got.value != want {
						t.Fatalf("%s: %q != %q", layer, *got.value, want)
					}
				}
			}
		})
	}
}

func TestInvalidFiles(t *testing.T) {
	cases := []struct{ body, key string }{
		{"unknown = 1", "unknown"}, {`RUNTIME_DIR = "/tmp/runtime"`, "RUNTIME_DIR"}, {"[CODEX]", "CODEX"}, {"[unknown]", "unknown"},
		{"[mcp]\nagent = 'claude'", "mcp"}, {"[agents]", "agents"}, {"[service]", "service"},
		{"codex.bin = 1", "bin"}, {"codex.bin = [", ""},
		{`runtime_dir = "relative"`, "runtime_dir"},
		{`codex.home = "relative"`, "codex.home"},
		{`claude.projects_dir = "relative"`, "claude.projects_dir"},
		{"claude.projects_dir = '/same'\ncodex.sessions_dir = '/same'", "claude.projects_dir"},
	}
	for _, tc := range cases {
		t.Run(tc.body, func(t *testing.T) {
			p := fixture(t, tc.body)
			_, err := Resolve(map[string]string{"config": p}, noenv)
			if err == nil || !strings.Contains(err.Error(), p) || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("got %v", err)
			}
		})
	}
	p := fixture(t, `runtime_dir = "relative"`)
	if _, err := Resolve(map[string]string{"config": p, "runtime-dir": "/valid"}, noenv); err == nil {
		t.Fatal("override hid invalid file")
	}
}

func TestPathsAndSelection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c, err := Resolve(nil, noenv)
	if err != nil || c.Found {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".rhizome")); !os.IsNotExist(err) {
		t.Fatal("read created directory")
	}
	p := fixture(t, "runtime_dir = '~/run'\ncodex.home = '$HOME/codex'\nclaude.config_dir = '${HOME}/claude'\n")
	env := func(k string) string {
		if k == "RHIZOME_CONFIG" {
			return p
		}
		return ""
	}
	c, err = Resolve(nil, env)
	if err != nil {
		t.Fatal(err)
	}
	if c.RuntimeDir != filepath.Join(home, "run") || c.Codex.SessionsDir != filepath.Join(home, "codex", "sessions") || c.Claude.ProjectsDir != filepath.Join(home, "claude", "projects") {
		t.Fatalf("%+v", c)
	}
	missing := filepath.Join(home, "missing.toml")
	for _, flags := range []map[string]string{{"config": missing}, {"config": ""}} {
		if _, err := Resolve(flags, env); err == nil {
			t.Fatal("explicit missing accepted")
		}
	}
	if _, err := Resolve(nil, func(string) string { return missing }); err == nil {
		t.Fatal("env missing accepted")
	}
	other := fixture(t, "codex.bin = 'other-codex'")
	c, err = Resolve(map[string]string{"config": other}, env)
	if err != nil || c.Path != other {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestFileSafety(t *testing.T) {
	p := fixture(t, "")
	if runtime.GOOS != "windows" {
		for _, mode := range []os.FileMode{0664, 0666} {
			if err := os.Chmod(p, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := Resolve(map[string]string{"config": p}, noenv); err == nil {
				t.Fatal("writable accepted")
			}
		}
		if err := os.Chmod(p, 0644); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(p, link); err == nil {
		if _, err := Resolve(map[string]string{"config": link}, noenv); err == nil {
			t.Fatal("symlink accepted")
		}
	}
	if _, err := Resolve(map[string]string{"config": t.TempDir()}, noenv); err == nil {
		t.Fatal("directory accepted")
	}
	p = fixture(t, strings.Repeat("#", 64*1024+1))
	if _, err := Resolve(map[string]string{"config": p}, noenv); err == nil {
		t.Fatal("oversize accepted")
	}
}

func TestFlagsAndOutput(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, key := range []string{"RHIZOME_CONFIG", "RHIZOME_RUNTIME_DIR", "CLAUDE_CONFIG_DIR", "CODEX_HOME"} {
		t.Setenv(key, "")
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	Bind(fs, "codex-bin", "runtime-dir")
	if err := fs.Parse([]string{"--codex-bin", "flag-codex"}); err != nil {
		t.Fatal(err)
	}
	c, err := FromFlags(fs)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := c.Write(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "not found") || !strings.Contains(out.String(), `codex.bin = "flag-codex" # source: flag`) || strings.Contains(out.String(), "token") {
		t.Fatal(out.String())
	}
	for _, s := range c.settings() {
		if !strings.Contains(out.String(), s.key+" = ") {
			t.Fatal(s.key)
		}
	}
}

func TestNetwork(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// No file, and an empty one, mean local mode with every key defaulted.
	for _, body := range []string{"", "# empty"} {
		c, err := Resolve(map[string]string{"config": fixture(t, body)}, noenv)
		if err != nil {
			t.Fatal(err)
		}
		if c.Network.Mode != "" || c.Sources["network.mode"] != "default" || c.Network.MDNS != nil {
			t.Fatalf("not defaults: %+v %v", c.Network, c.Sources)
		}
	}
	c, err := Resolve(nil, noenv)
	if err != nil || c.Found || c.Network.Mode != "" {
		t.Fatalf("missing file: %+v %v", c.Network, err)
	}

	ok := []struct {
		name, body string
		check      func(Network) bool
	}{
		{"lan", "[network]\nmode='lan'\nbind_addr='192.168.1.5'\nadvertise_addr='192.168.1.5'\njoin=['a:7946','b:7946']\nmdns=false", func(n Network) bool {
			return n.Mode == ModeLAN && n.BindAddr == "192.168.1.5" && len(n.Join) == 2 && n.MDNS != nil && !*n.MDNS
		}},
		{"tailscale", "[network]\nmode='tailscale'\ngossip_port=7950\ndiscovery_interval='45s'\njoin=['host.ts.net:7946']", func(n Network) bool {
			return n.Mode == ModeTailscale && n.GossipPort == 7950 && n.DiscoveryInterval == "45s"
		}},
		{"explicit local", "[network]\nmode='local'", func(n Network) bool { return n.Mode == ModeLocal }},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Resolve(map[string]string{"config": fixture(t, tc.body)}, noenv)
			if err != nil || !tc.check(c.Network) || c.Sources["network.mode"] != "file" {
				t.Fatalf("%+v %v %v", c.Network, c.Sources, err)
			}
			var out bytes.Buffer
			if err := c.Write(&out); err != nil || !strings.Contains(out.String(), "network.mode = ") || strings.Contains(out.String(), "token") {
				t.Fatalf("%v\n%s", err, out.String())
			}
		})
	}

	bad := []struct{ body, want string }{
		{"[network]\nmode='wan'", "network.mode"},
		{"[network]\nmode='local'\njoin=['a:1']", "require mode"},
		{"[network]\nbind_addr='10.0.0.1'", "require mode"},
		{"[network]\nmode='lan'\ngossip_port=7946", "tailscale"},
		{"[network]\nmode='tailscale'\ngossip_port=70000", "gossip_port"},
		{"[network]\nmode='tailscale'\ndiscovery_interval='soon'", "discovery_interval"},
		{"[network]\nmode='tailscale'\ndiscovery_interval='-5s'", "discovery_interval"},
		{"[network]\nmode='lan'\ntoken='secret'", "token"},
		{"[network]\nMode='lan'", "Mode"},
		{"[network]\nmode=1", "mode"},
		{"[NETWORK]\nmode='lan'", "NETWORK"},
	}
	for _, tc := range bad {
		t.Run(tc.body, func(t *testing.T) {
			p := fixture(t, tc.body)
			_, err := Resolve(map[string]string{"config": p}, noenv)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p, explicit, err := Target(nil, noenv)
	if err != nil || explicit || p != filepath.Join(home, ".rhizome", "config.toml") {
		t.Fatal(p, explicit, err)
	}
	p, explicit, err = Target(map[string]string{"config": "/flag.toml"}, func(string) string { return "/env.toml" })
	if err != nil || !explicit || p != "/flag.toml" {
		t.Fatal(p, explicit, err)
	}
}
