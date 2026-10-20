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
			if s.key == "mcp.agent" {
				file, env, override = "codex", "codex", "claude"
			}
			if s.key == "mcp.codex_bin" {
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
		{"unknown = 1", "unknown"}, {`RUNTIME_DIR = "/tmp/runtime"`, "RUNTIME_DIR"}, {"[MCP]", "MCP"}, {"[unknown]", "unknown"},
		{"mcp.agent = 1", "agent"}, {"mcp.agent = [", ""},
		{`runtime_dir = "relative"`, "runtime_dir"},
		{`agents.codex_home = "relative"`, "agents.codex_home"},
		{`service.claude_root = "relative"`, "service.claude_root"},
		{"service.claude_root = '/same'\nservice.codex_root = '/same'", "service.claude_root"},
		{`mcp.agent = "other"`, "mcp.agent"},
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
	p := fixture(t, "runtime_dir = '~/run'\nagents.codex_home = '$HOME/codex'\nagents.claude_config_dir = '${HOME}/claude'\n")
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
	if c.RuntimeDir != filepath.Join(home, "run") || c.Service.CodexRoot != filepath.Join(home, "codex", "sessions") || c.Service.ClaudeRoot != filepath.Join(home, "claude", "projects") {
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
	other := fixture(t, "mcp.agent = 'codex'")
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
	Bind(fs, "agent", "runtime-dir")
	if err := fs.Parse([]string{"--agent", "codex"}); err != nil {
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
	if !strings.Contains(out.String(), "not found") || !strings.Contains(out.String(), `mcp.agent = "codex" # source: flag`) || strings.Contains(out.String(), "token") {
		t.Fatal(out.String())
	}
	for _, s := range c.settings() {
		if !strings.Contains(out.String(), s.key+" = ") {
			t.Fatal(s.key)
		}
	}
}
