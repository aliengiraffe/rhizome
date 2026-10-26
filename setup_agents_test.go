package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"rhizome/internal/config"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

var fixedNow = func() time.Time { return time.Date(2026, 10, 21, 18, 0, 0, 0, time.UTC) }

func noPath(string) (string, error) { return "", errors.New("not found") }

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := decodeJSON(b)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return doc
}

func readTOML(t *testing.T, path string) map[string]any {
	t.Helper()
	var doc map[string]any
	if _, err := toml.DecodeFile(path, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return doc
}

// templateAsJSON round-trips the template through JSON so it compares with
// decoded files.
func templateAsJSON(t *testing.T, agentName string) map[string]any {
	t.Helper()
	b, err := json.Marshal(hookTemplate(agentName))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := decodeJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// The checked-in examples are the documented contract; the installed hooks must
// not drift from them.
func TestHookTemplateMatchesExamples(t *testing.T) {
	for agentName, path := range map[string]string{"claude": ".claude/settings.json", "codex": "examples/codex/hooks.json"} {
		want := readJSON(t, path)["hooks"]
		if got := templateAsJSON(t, agentName); !reflect.DeepEqual(got, want) {
			t.Errorf("%s hook template differs from %s:\n got %v\nwant %v", agentName, path, got, want)
		}
	}
}

func TestRhizomeServer(t *testing.T) {
	home := t.TempDir()
	cfg := filepath.Join(home, "rhizome dir", "config.toml")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("[network]\nmode = \"local\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := func() config.Config {
		var c config.Config
		c.Path = cfg
		c.Claude.ConfigDir, c.Codex.Home, c.Codex.Bin = filepath.Join(home, ".claude"), filepath.Join(home, ".codex"), "codex"
		return c
	}
	exe := filepath.Join(home, "bin dir", "rhizome")
	t.Run("defaults", func(t *testing.T) {
		s, err := rhizomeServer(exe, "claude", base(), false, home, noPath)
		if err != nil || s.Command != exe || !reflect.DeepEqual(s.Args, []string{"mcp", "--agent", "claude"}) || s.Env != nil {
			t.Fatalf("%+v %v", s, err)
		}
		s, err = rhizomeServer(exe, "codex", base(), false, home, noPath)
		if err != nil || !reflect.DeepEqual(s.Args, []string{"mcp", "--agent", "codex"}) || s.Env != nil {
			t.Fatalf("%+v %v", s, err)
		}
	})
	t.Run("explicit config", func(t *testing.T) {
		s, err := rhizomeServer(exe, "claude", base(), true, home, noPath)
		if err != nil || !reflect.DeepEqual(s.Args, []string{"mcp", "--agent", "claude", "--config", cfg}) {
			t.Fatalf("%+v %v", s, err)
		}
		c := base()
		c.Path = filepath.Join(home, "missing.toml")
		if _, err := rhizomeServer(exe, "claude", c, true, home, noPath); err == nil || !strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("missing explicit config accepted: %v", err)
		}
	})
	t.Run("custom homes and codex binary", func(t *testing.T) {
		c := base()
		c.Claude.ConfigDir, c.Codex.Home = filepath.Join(home, "claude personal"), filepath.Join(home, "codex personal")
		s, err := rhizomeServer(exe, "claude", c, false, home, noPath)
		if err != nil || !reflect.DeepEqual(s.Env, map[string]string{"CLAUDE_CONFIG_DIR": c.Claude.ConfigDir}) {
			t.Fatalf("%+v %v", s, err)
		}
		found := func(name string) (string, error) { return "/opt/bin/" + name, nil }
		s, err = rhizomeServer(exe, "codex", c, false, home, found)
		if err != nil || !reflect.DeepEqual(s.Env, map[string]string{"CODEX_HOME": c.Codex.Home}) || !reflect.DeepEqual(s.Args, []string{"mcp", "--agent", "codex", "--codex-bin", "/opt/bin/codex"}) {
			t.Fatalf("%+v %v", s, err)
		}
		c.Codex.Bin = "/custom/codex"
		if s, err = rhizomeServer(exe, "codex", c, false, home, noPath); err != nil || s.Args[len(s.Args)-1] != "/custom/codex" {
			t.Fatalf("%+v %v", s, err)
		}
	})
	if _, err := rhizomeServer(exe, "zed", base(), false, home, noPath); err == nil {
		t.Fatal("unknown agent accepted")
	}
}

func TestMergeHooks(t *testing.T) {
	other := map[string]any{"type": "command", "command": "echo hi"}
	stale := map[string]any{"type": "mcp_tool", "server": "rhizome", "tool": "observe_session", "input": map[string]any{"session_id": "x"}}
	doc := map[string]any{
		"theme": "dark",
		"hooks": map[string]any{
			"PreToolUse":   []any{map[string]any{"matcher": "Bash", "hooks": []any{other, stale}}, map[string]any{"hooks": []any{stale}}},
			"Notification": []any{map[string]any{"hooks": []any{stale}}},
			"PostToolUse":  []any{map[string]any{"hooks": []any{other}}},
		},
	}
	template := hookTemplate("claude")
	if err := mergeHooks(doc, template); err != nil {
		t.Fatal(err)
	}
	hooks := doc["hooks"].(map[string]any)
	if doc["theme"] != "dark" {
		t.Fatal("unrelated key lost")
	}
	if _, ok := hooks["Notification"]; ok {
		t.Fatal("stale Rhizome-only event kept")
	}
	if got := hooks["PostToolUse"]; !reflect.DeepEqual(got, []any{map[string]any{"hooks": []any{other}}}) {
		t.Fatalf("unrelated event changed: %v", got)
	}
	pre := hooks["PreToolUse"].([]any)
	if len(pre) != 2 || !reflect.DeepEqual(pre[0], map[string]any{"matcher": "Bash", "hooks": []any{other}}) || !reflect.DeepEqual(pre[1], template["PreToolUse"][0]) {
		t.Fatalf("mixed group not preserved or template missing: %v", pre)
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"} {
		if !reflect.DeepEqual(hooks[event], template[event]) {
			t.Fatalf("%s: %v", event, hooks[event])
		}
	}
	// A second merge is a no-op.
	before, _ := encodeJSON(doc)
	if err := mergeHooks(doc, template); err != nil {
		t.Fatal(err)
	}
	if after, _ := encodeJSON(doc); !bytes.Equal(before, after) {
		t.Fatalf("second merge changed the document:\n%s\n%s", before, after)
	}
	if err := mergeHooks(map[string]any{"hooks": "nope"}, template); err == nil {
		t.Fatal("non-object hooks accepted")
	}
	if err := mergeHooks(map[string]any{"hooks": map[string]any{"Stop": "nope"}}, template); err == nil {
		t.Fatal("non-array event accepted")
	}
}

func TestMergeJSONServer(t *testing.T) {
	doc := map[string]any{
		"oauthAccount": map[string]any{"email": "x"},
		"mcpServers": map[string]any{
			"other":   map[string]any{"command": "keep"},
			"rhizome": map[string]any{"command": "/old", "args": []any{"mcp"}, "env": map[string]any{"CLAUDE_CONFIG_DIR": "/old", "KEEP": "1"}, "extra": true},
		},
	}
	s := mcpServer{Command: "/new rhizome", Args: []string{"mcp", "--agent", "claude"}}
	if err := mergeJSONServer(doc, s); err != nil {
		t.Fatal(err)
	}
	servers := doc["mcpServers"].(map[string]any)
	want := map[string]any{"type": "stdio", "command": "/new rhizome", "args": []any{"mcp", "--agent", "claude"}, "env": map[string]any{"KEEP": "1"}, "extra": true}
	if !reflect.DeepEqual(servers["rhizome"], want) || !reflect.DeepEqual(servers["other"], map[string]any{"command": "keep"}) || doc["oauthAccount"] == nil {
		t.Fatalf("%v", doc)
	}
	if err := mergeJSONServer(map[string]any{"mcpServers": []any{}}, s); err == nil {
		t.Fatal("non-object mcpServers accepted")
	}
}

func TestMergeCodexConfig(t *testing.T) {
	s := mcpServer{Command: "/Applications/Rhizome Dir/rhizome", Args: []string{"mcp", "--agent", "codex", "--codex-bin", "/opt/codex"}}
	t.Run("empty file", func(t *testing.T) {
		out, err := mergeCodexConfig(nil, s)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if _, err := toml.Decode(string(out), &doc); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		want := map[string]any{"command": s.Command, "args": []any{"mcp", "--agent", "codex", "--codex-bin", "/opt/codex"}}
		if got := tomlLookup(doc, "mcp_servers", "rhizome"); !reflect.DeepEqual(got, want) {
			t.Fatalf("%v\n%s", got, out)
		}
	})
	t.Run("keeps comments and other tables, replaces stale entry", func(t *testing.T) {
		existing := "# my codex config\nmodel = \"gpt-5\"\n\n[mcp_servers.other]\ncommand = \"keep\" # mine\n\n[mcp_servers.rhizome]\ncommand = \"/old\"\nargs = [\"mcp\"]\nenabled = false\n\n[mcp_servers.rhizome.env]\nCODEX_HOME = \"/old\"\nKEEP = \"1\"\n\n[tui]\ntheme = \"x\"\n"
		out, err := mergeCodexConfig([]byte(existing), s)
		if err != nil {
			t.Fatal(err)
		}
		text := string(out)
		for _, keep := range []string{"# my codex config", "command = \"keep\" # mine", "[tui]\ntheme = \"x\""} {
			if !strings.Contains(text, keep) {
				t.Fatalf("lost %q:\n%s", keep, text)
			}
		}
		if strings.Contains(text, "/old") || strings.Count(text, "[mcp_servers.rhizome]") != 1 {
			t.Fatalf("stale content kept:\n%s", text)
		}
		var doc map[string]any
		if _, err := toml.Decode(text, &doc); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"command": s.Command, "args": []any{"mcp", "--agent", "codex", "--codex-bin", "/opt/codex"}, "enabled": false, "env": map[string]any{"KEEP": "1"}}
		if got := tomlLookup(doc, "mcp_servers", "rhizome"); !reflect.DeepEqual(got, want) {
			t.Fatalf("%v", got)
		}
		if again, err := mergeCodexConfig(out, s); err != nil || !bytes.Equal(again, out) {
			t.Fatalf("not idempotent: %v\n%s", err, again)
		}
	})
	t.Run("rejects malformed and inline definitions", func(t *testing.T) {
		if _, err := mergeCodexConfig([]byte("model = \n"), s); err == nil || !strings.Contains(err.Error(), "not valid TOML") {
			t.Fatalf("malformed accepted: %v", err)
		}
		inline := "[mcp_servers]\nrhizome = { command = \"/old\" }\n"
		if _, err := mergeCodexConfig([]byte(inline), s); err == nil || !strings.Contains(err.Error(), "[mcp_servers.rhizome] table") {
			t.Fatalf("inline table accepted: %v", err)
		}
	})
}

func TestEditFile(t *testing.T) {
	dir := t.TempDir()
	upper := func(existing []byte) ([]byte, error) { return []byte(strings.ToUpper(string(existing)) + "x\n"), nil }
	path := filepath.Join(dir, "sub dir", "file.json")
	res, err := editFile(path, upper, fixedNow())
	if err != nil || res.Status != "created" || res.Backup != "" {
		t.Fatalf("%+v %v", res, err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("new file mode %v", st.Mode())
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = editFile(path, upper, fixedNow())
	if err != nil || res.Status != "updated" || res.Backup != path+".bak-20261021T180000Z" {
		t.Fatalf("%+v %v", res, err)
	}
	if b, _ := os.ReadFile(res.Backup); string(b) != "x\n" {
		t.Fatalf("backup content %q", b)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o644 {
		t.Fatalf("mode not preserved: %v", st.Mode())
	}
	// Same second: the backup name gets a suffix instead of failing.
	if res, err = editFile(path, upper, fixedNow()); err != nil || res.Backup != path+".bak-20261021T180000Z-1" {
		t.Fatalf("%+v %v", res, err)
	}
	same := func(existing []byte) ([]byte, error) { return existing, nil }
	if res, err = editFile(path, same, fixedNow()); err != nil || res.Status != "unchanged" || res.Backup != "" {
		t.Fatalf("%+v %v", res, err)
	}
	// A symlink is followed; the link stays and the target is edited.
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(path)
	if res, err = editFile(link, upper, fixedNow()); err != nil || res.Path != resolved || res.Status != "updated" {
		t.Fatalf("%+v %v", res, err)
	}
	if st, _ := os.Lstat(link); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced")
	}
	dangling := filepath.Join(dir, "dangling.json")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), dangling); err != nil {
		t.Fatal(err)
	}
	if _, err := editFile(dangling, upper, fixedNow()); err == nil {
		t.Fatal("dangling symlink accepted")
	}
	if _, err := editFile(dir, upper, fixedNow()); err == nil {
		t.Fatal("directory accepted")
	}
	// An edit error leaves the file alone.
	before, _ := os.ReadFile(path)
	if _, err := editFile(path, func([]byte) ([]byte, error) { return nil, errors.New("bad") }, fixedNow()); err == nil {
		t.Fatal("edit error ignored")
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("file changed despite edit error")
	}
}

func TestEditFileWriteFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "ro")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	_, err := editJSON(path, func(doc map[string]any) error { return mergeHooks(doc, hookTemplate("claude")) }, fixedNow())
	if err == nil {
		t.Fatal("write into read-only directory succeeded")
	}
	if b, _ := os.ReadFile(path); string(b) != "{}\n" {
		t.Fatalf("file changed: %q", b)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("leftovers: %v", entries)
	}
}

func TestEditJSONRejectsMalformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.json")
	for _, body := range []string{"{", "[]", "{} trailing", "null"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := editJSON(path, func(doc map[string]any) error { doc["k"] = 1; return nil }, fixedNow())
		if body == "null" {
			if err != nil {
				t.Fatalf("null document rejected: %v", err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
			t.Fatalf("%q accepted: %v", body, err)
		}
		if b, _ := os.ReadFile(path); string(b) != body {
			t.Fatalf("%q replaced by %q", body, b)
		}
	}
	// Numbers keep their spelling and unrelated keys survive.
	if err := os.WriteFile(path, []byte(`{"n": 1.50, "big": 12345678901234567890, "s": "<&>"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := editJSON(path, func(doc map[string]any) error { doc["k"] = "v"; return nil }, fixedNow()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	for _, want := range []string{`"n": 1.50`, `"big": 12345678901234567890`, `"s": "<&>"`, `"k": "v"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %s in %s", want, b)
		}
	}
}

// installEnv prepares an isolated home and returns the config for it.
func installEnv(t *testing.T, env map[string]string) (config.Config, string) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home dir")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	for k, v := range env {
		t.Setenv(k, v)
	}
	c, err := config.Resolve(nil, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	return c, home
}

func TestInstallAgentsFreshRepeatAndStale(t *testing.T) {
	c, home := installEnv(t, nil)
	exe := filepath.Join(home, "Rhizome App", "rhizome")
	var out bytes.Buffer
	in := agentInstaller{exe: exe, home: home, c: c, lookPath: func(name string) (string, error) { return "/opt/homebrew/bin/" + name, nil }, now: fixedNow, out: &out}
	agents := detectAgents(c, noPath)[:3] // claude, codex and opencode; Cursor is skill-only, see TestInstallCursorSkillOnly
	for _, a := range agents {
		if err := in.install(a); err != nil {
			t.Fatal(err)
		}
	}
	claudeJSON, claudeSettings := filepath.Join(home, ".claude.json"), filepath.Join(home, ".claude", "settings.json")
	codexTOML, codexHooks := filepath.Join(home, ".codex", "config.toml"), filepath.Join(home, ".codex", "hooks.json")
	if agents[0].MCP != claudeJSON || agents[0].Hooks != claudeSettings || agents[1].MCP != codexTOML || agents[1].Hooks != codexHooks {
		t.Fatalf("destinations: %+v", agents)
	}
	// Claude: user-scope server and hooks.
	servers := readJSON(t, claudeJSON)["mcpServers"].(map[string]any)
	wantClaude := map[string]any{"type": "stdio", "command": exe, "args": []any{"mcp", "--agent", "claude"}}
	if !reflect.DeepEqual(servers["rhizome"], wantClaude) {
		t.Fatalf("claude server: %v", servers["rhizome"])
	}
	if got := readJSON(t, claudeSettings)["hooks"]; !reflect.DeepEqual(got, templateAsJSON(t, "claude")) {
		t.Fatalf("claude hooks: %v", got)
	}
	// Codex: config.toml table and hooks.json.
	wantCodex := map[string]any{"command": exe, "args": []any{"mcp", "--agent", "codex", "--codex-bin", "/opt/homebrew/bin/codex"}}
	if got := tomlLookup(readTOML(t, codexTOML), "mcp_servers", "rhizome"); !reflect.DeepEqual(got, wantCodex) {
		t.Fatalf("codex server: %v", got)
	}
	if got := readJSON(t, codexHooks)["hooks"]; !reflect.DeepEqual(got, templateAsJSON(t, "codex")) {
		t.Fatalf("codex hooks: %v", got)
	}
	// OpenCode: skill and both plugins, with no MCP entry or hooks edited for it.
	openCodeDir := filepath.Join(home, ".config", "opencode")
	if agents[2].Name != "opencode" || agents[2].MCP != "" || agents[2].Hooks != "" {
		t.Fatalf("opencode destinations: %+v", agents[2])
	}
	for _, p := range []string{filepath.Join(openCodeDir, "plugins", "rhizome.js"), filepath.Join(openCodeDir, "plugins", "rhizome-tui", "tui.js")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("opencode plugin missing: %v", err)
		}
	}
	for _, want := range []string{"plugin: installed", "tui:    installed", "docs/opencode.md"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	for _, p := range []string{claudeJSON, claudeSettings, codexTOML, codexHooks} {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", p, st, err)
		}
	}
	for _, want := range []string{"skill: installed", "mcp:   created rhizome server in " + claudeJSON, "hooks: created 5 hook groups in " + claudeSettings, "created 4 hook groups in " + codexHooks, "dangerously-load-development-channels", "/hooks"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "token") {
		t.Fatalf("output mentions tokens:\n%s", out.String())
	}
	// Repeat: nothing changes and no backups appear.
	out.Reset()
	for _, a := range agents {
		if err := in.install(a); err != nil {
			t.Fatal(err)
		}
	}
	// Claude and Codex report the skill, MCP entry and hooks; OpenCode reports the
	// skill and its two plugins.
	if strings.Count(out.String(), "unchanged") != 9 || strings.Contains(out.String(), "backup") {
		t.Fatalf("repeat run not idempotent:\n%s", out.String())
	}
	if matches, _ := filepath.Glob(filepath.Join(home, "*", "*.bak-*")); len(matches) != 0 {
		t.Fatalf("backups on unchanged run: %v", matches)
	}
	// Stale registrations and hooks are repaired, with backups, and unrelated
	// content survives.
	if err := os.WriteFile(claudeJSON, []byte(`{"oauthAccount":{"e":"x"},"mcpServers":{"rhizome":{"type":"stdio","command":"/old/rhizome","args":["mcp"]},"other":{"command":"o"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexTOML, []byte("model = \"m\"\n[mcp_servers.rhizome]\ncommand = \"/old\"\nargs = [\"mcp\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	for _, a := range agents {
		if err := in.install(a); err != nil {
			t.Fatal(err)
		}
	}
	doc := readJSON(t, claudeJSON)
	servers = doc["mcpServers"].(map[string]any)
	if !reflect.DeepEqual(servers["rhizome"], wantClaude) || !reflect.DeepEqual(servers["other"], map[string]any{"command": "o"}) || doc["oauthAccount"] == nil {
		t.Fatalf("stale claude entry: %v", doc)
	}
	codex := readTOML(t, codexTOML)
	if !reflect.DeepEqual(tomlLookup(codex, "mcp_servers", "rhizome"), wantCodex) || codex["model"] != "m" {
		t.Fatalf("stale codex entry: %v", codex)
	}
	for _, want := range []string{"updated rhizome server in " + claudeJSON + " (backup: " + claudeJSON + ".bak-20261021T180000Z)", "updated rhizome server in " + codexTOML + " (backup: " + codexTOML + ".bak-20261021T180000Z)"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	if b, _ := os.ReadFile(claudeJSON + ".bak-20261021T180000Z"); !bytes.Contains(b, []byte("/old/rhizome")) {
		t.Fatalf("backup content: %s", b)
	}
}

func TestInstallAgentsCustomHomesAndExplicitConfig(t *testing.T) {
	root := t.TempDir()
	claudeDir, codexHome := filepath.Join(root, "claude personal"), filepath.Join(root, "codex personal")
	cfg := filepath.Join(root, "rhizome conf", "config.toml")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("[codex]\nbin = \"/custom/codex\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, home := installEnv(t, map[string]string{"CLAUDE_CONFIG_DIR": claudeDir, "CODEX_HOME": codexHome, "RHIZOME_CONFIG": cfg})
	exe := filepath.Join(home, "rhizome")
	var out bytes.Buffer
	in := agentInstaller{exe: exe, home: home, c: c, explicit: true, lookPath: noPath, now: fixedNow, out: &out}
	agents := detectAgents(c, noPath)
	for _, a := range agents {
		if err := in.install(a); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("default ~/.claude.json touched for a custom config directory")
	}
	servers := readJSON(t, filepath.Join(claudeDir, ".claude.json"))["mcpServers"].(map[string]any)
	want := map[string]any{"type": "stdio", "command": exe, "args": []any{"mcp", "--agent", "claude", "--config", cfg}, "env": map[string]any{"CLAUDE_CONFIG_DIR": claudeDir}}
	if !reflect.DeepEqual(servers["rhizome"], want) {
		t.Fatalf("claude: %v", servers["rhizome"])
	}
	if _, err := os.Stat(filepath.Join(claudeDir, "settings.json")); err != nil {
		t.Fatal(err)
	}
	codex := tomlLookup(readTOML(t, filepath.Join(codexHome, "config.toml")), "mcp_servers", "rhizome")
	wantCodex := map[string]any{"command": exe, "args": []any{"mcp", "--agent", "codex", "--config", cfg, "--codex-bin", "/custom/codex"}, "env": map[string]any{"CODEX_HOME": codexHome}}
	if !reflect.DeepEqual(codex, wantCodex) {
		t.Fatalf("codex: %v", codex)
	}
	if _, err := os.Stat(filepath.Join(codexHome, "skills", skillName, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	// Moving back to the defaults drops the pinned home from the entry.
	in.c.Claude.ConfigDir = filepath.Join(home, ".claude")
	if err := os.Rename(filepath.Join(claudeDir, ".claude.json"), filepath.Join(home, ".claude.json")); err != nil {
		t.Fatal(err)
	}
	if err := in.install(detectAgents(in.c, noPath)[0]); err != nil {
		t.Fatal(err)
	}
	servers = readJSON(t, filepath.Join(home, ".claude.json"))["mcpServers"].(map[string]any)
	if entry := servers["rhizome"].(map[string]any); len(entry["env"].(map[string]any)) != 0 {
		t.Fatalf("env not cleared: %v", entry)
	}
}

func TestInstallAgentsFailuresAreActionable(t *testing.T) {
	c, home := installEnv(t, nil)
	var out bytes.Buffer
	in := agentInstaller{exe: filepath.Join(home, "rhizome"), home: home, c: c, lookPath: noPath, now: fixedNow, out: &out}
	agents := detectAgents(c, noPath)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := in.install(agents[0])
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") || !strings.Contains(err.Error(), ".claude.json") {
		t.Fatalf("malformed .claude.json: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".claude.json")); string(b) != "{not json" {
		t.Fatalf("malformed file replaced: %q", b)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("hooks installed after an MCP failure")
	}
	// An explicit configuration that does not exist is refused before any file changes.
	in.explicit, in.c.Path = true, filepath.Join(home, "missing.toml")
	if err := in.install(agents[1]); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing config: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "config.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("codex config written despite missing configuration")
	}
}

// The real entry point, in a subprocess with an isolated home, must register
// a command that works outside the invoking directory.
func TestSetupCLIInstallsGlobalRegistration(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home with space")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		cmd := configProcess(t, home, args...)
		cmd.Dir = t.TempDir()
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, b)
		}
		return string(b)
	}
	// Read-only listing touches nothing.
	out := run("setup", "--list")
	if !strings.Contains(out, "mcp "+filepath.Join(home, ".claude.json")) {
		t.Fatalf("list output:\n%s", out)
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatalf("--list wrote %v", entries)
	}
	// Configuration-only run installs no agent.
	run("setup", "--network", "local")
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("--network without agents registered MCP")
	}
	// Selecting one agent leaves the other alone.
	out = run("setup", "--agents", "codex")
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("claude registered without selection:\n%s", out)
	}
	codex := readTOML(t, filepath.Join(home, ".codex", "config.toml"))
	entry, _ := tomlLookup(codex, "mcp_servers", "rhizome").(map[string]any)
	command, _ := entry["command"].(string)
	if !filepath.IsAbs(command) {
		t.Fatalf("command not absolute: %v", entry)
	}
	if _, err := os.Stat(command); err != nil {
		t.Fatalf("registered executable missing: %v", err)
	}
	args, _ := entry["args"].([]any)
	if len(args) < 3 || args[0] != "mcp" || args[1] != "--agent" || args[2] != "codex" {
		t.Fatalf("args: %v", args)
	}
	for _, a := range args {
		if strings.Contains(a.(string), "--config") {
			t.Fatalf("default configuration path pinned: %v", args)
		}
	}
	// All agents, with the explicit configuration written just before.
	cfg := filepath.Join(home, "my conf", "rhizome.toml")
	out = run("setup", "--agents", "all", "--network", "local", "--config", cfg)
	if !strings.Contains(out, "wrote "+cfg) {
		t.Fatalf("configuration not written:\n%s", out)
	}
	servers := readJSON(t, filepath.Join(home, ".claude.json"))["mcpServers"].(map[string]any)
	claude := servers["rhizome"].(map[string]any)
	if got := claude["args"]; !reflect.DeepEqual(got, []any{"mcp", "--agent", "claude", "--config", cfg}) {
		t.Fatalf("claude args: %v", got)
	}
	hooks := readJSON(t, filepath.Join(home, ".claude", "settings.json"))["hooks"].(map[string]any)
	if len(hooks) != 5 {
		t.Fatalf("claude hooks: %v", hooks)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", skillName, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	// The registration must not depend on the working directory: the command
	// and configuration are absolute and the test ran from elsewhere.
	if !filepath.IsAbs(claude["command"].(string)) {
		t.Fatalf("claude command: %v", claude["command"])
	}
	// The registered command answers `config` with the pinned file.
	cmd := configProcess(t, home, append([]string{"config"}, "--config", cfg)...)
	cmd.Dir = t.TempDir()
	b, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(b), cfg) {
		t.Fatalf("config: %v\n%s", err, b)
	}
	// Repeat is idempotent: no new backups.
	run("setup", "--agents", "all", "--config", cfg)
	if matches, _ := filepath.Glob(filepath.Join(home, ".claude.json.bak-*")); len(matches) != 0 {
		t.Fatalf("backups after an unchanged run: %v", matches)
	}
}

// Cursor setup writes mcp.json and hooks.json at user scope like the other
// agents: fresh, repeated (unchanged), stale entries replaced, foreign hooks and
// servers preserved, --config threaded into the hook command, paths quoted.
func TestInstallCursor(t *testing.T) {
	c, home := installEnv(t, nil)
	exe := filepath.Join(home, "Rhizome App", "rhizome")
	var out bytes.Buffer
	in := agentInstaller{exe: exe, home: home, c: c, lookPath: noPath, now: fixedNow, out: &out}
	cursor := detectAgents(c, noPath)[3]
	if cursor.Name != "cursor" || cursor.MCP != filepath.Join(home, ".cursor", "mcp.json") || cursor.Hooks != filepath.Join(home, ".cursor", "hooks.json") {
		t.Fatalf("cursor destinations: %+v", cursor)
	}
	if err := in.install(cursor); err != nil {
		t.Fatal(err)
	}
	servers := readJSON(t, cursor.MCP)["mcpServers"].(map[string]any)
	want := map[string]any{"type": "stdio", "command": exe, "args": []any{"mcp", "--agent", "cursor"}}
	if !reflect.DeepEqual(servers["rhizome"], want) {
		t.Fatalf("cursor server: %v", servers["rhizome"])
	}
	hooks := readJSON(t, cursor.Hooks)
	command := "'" + exe + "' hook --agent cursor"
	wantHooks, _ := decodeJSON(mustJSON(t, cursorHookTemplate(command)))
	if hooks["version"] != json.Number("1") || !reflect.DeepEqual(hooks["hooks"], wantHooks) {
		t.Fatalf("cursor hooks: %v", hooks)
	}
	for _, want := range []string{"skill: installed", "mcp:   created", "hooks: created", "approve the rhizome MCP server"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	// Repeating changes nothing and makes no backup.
	out.Reset()
	if err := in.install(cursor); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "mcp:   unchanged") || !strings.Contains(out.String(), "hooks: unchanged") {
		t.Fatalf("repeat run: %s", out.String())
	}
	// Foreign entries survive; stale Rhizome entries (old path, extra event) are replaced.
	stale := map[string]any{"version": json.Number("2"), "hooks": map[string]any{
		"stop":          []any{map[string]any{"command": "/old/rhizome hook --agent cursor"}, map[string]any{"command": "./mine.sh"}},
		"afterFileEdit": []any{map[string]any{"command": "/old/rhizome hook --agent cursor"}},
		"preToolUse":    []any{map[string]any{"command": "./audit.sh", "matcher": "Shell"}},
	}}
	if err := os.WriteFile(cursor.Hooks, mustJSON(t, stale), 0o600); err != nil {
		t.Fatal(err)
	}
	mcpDoc := map[string]any{"mcpServers": map[string]any{"github": map[string]any{"url": "https://example.invalid"}, "rhizome": map[string]any{"command": "/old/rhizome", "args": []any{"mcp"}, "env": map[string]any{"KEEP": "1"}}}}
	if err := os.WriteFile(cursor.MCP, mustJSON(t, mcpDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := in.install(cursor); err != nil {
		t.Fatal(err)
	}
	hooks = readJSON(t, cursor.Hooks)
	got := hooks["hooks"].(map[string]any)
	if hooks["version"] != json.Number("2") || got["afterFileEdit"] != nil || len(got["preToolUse"].([]any)) != 2 || got["preToolUse"].([]any)[0].(map[string]any)["command"] != "./audit.sh" || len(got["stop"].([]any)) != 2 || got["stop"].([]any)[0].(map[string]any)["command"] != "./mine.sh" {
		t.Fatalf("stale merge: %v", hooks)
	}
	for _, event := range cursorHookEvents {
		entries := got[event].([]any)
		if last := entries[len(entries)-1].(map[string]any); last["command"] != command {
			t.Fatalf("%s: %v", event, entries)
		}
	}
	servers = readJSON(t, cursor.MCP)["mcpServers"].(map[string]any)
	if servers["github"] == nil || !reflect.DeepEqual(servers["rhizome"], map[string]any{"type": "stdio", "command": exe, "args": []any{"mcp", "--agent", "cursor"}, "env": map[string]any{"KEEP": "1"}}) {
		t.Fatalf("stale server merge: %v", servers)
	}
	if backups, _ := filepath.Glob(cursor.Hooks + ".bak-*"); len(backups) != 1 {
		t.Fatalf("hooks backup: %v", backups)
	}
	// An explicit configuration file reaches both the server entry and the hook command.
	cfg := filepath.Join(home, "rz dir", "config.toml")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("[network]\nmode = \"local\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ec := c
	ec.Path = cfg
	explicit := agentInstaller{exe: exe, home: home, c: ec, explicit: true, lookPath: noPath, now: fixedNow, out: &out}
	if err := explicit.install(cursor); err != nil {
		t.Fatal(err)
	}
	args := readJSON(t, cursor.MCP)["mcpServers"].(map[string]any)["rhizome"].(map[string]any)["args"]
	if !reflect.DeepEqual(args, []any{"mcp", "--agent", "cursor", "--config", cfg}) {
		t.Fatalf("explicit args: %v", args)
	}
	stop := readJSON(t, cursor.Hooks)["hooks"].(map[string]any)["stop"].([]any)
	if stop[len(stop)-1].(map[string]any)["command"] != command+" --config '"+cfg+"'" {
		t.Fatalf("explicit hook command: %v", stop)
	}
	// A malformed hooks file is reported, not replaced.
	if err := os.WriteFile(cursor.Hooks, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := in.install(cursor); err == nil || !strings.Contains(err.Error(), "hooks") {
		t.Fatalf("malformed hooks: %v", err)
	}
	if b, _ := os.ReadFile(cursor.Hooks); string(b) != "{" {
		t.Fatal("malformed hooks file was replaced")
	}
}

// The Cursor template must not drift from the checked-in example; the example
// uses a placeholder path and a shell string per entry.
func TestCursorHookTemplateMatchesExample(t *testing.T) {
	want := readJSON(t, "examples/cursor/hooks.json")
	got, err := decodeJSON(mustJSON(t, cursorHookTemplate("/absolute/path/to/rhizome/rhizome hook --agent cursor")))
	if err != nil || want["version"] != json.Number("1") || !reflect.DeepEqual(got, want["hooks"]) {
		t.Fatalf("cursor template differs from examples/cursor/hooks.json:\n got %v\nwant %v (%v)", got, want["hooks"], err)
	}
	for _, in := range []string{"/plain/path", "/with space/x", "it's", "a$b"} {
		if q := shellQuote(in); (strings.ContainsAny(in, " '$") && !strings.HasPrefix(q, "'")) || (!strings.ContainsAny(in, " '$") && q != in) {
			t.Fatalf("shellQuote(%q) = %q", in, q)
		}
	}
	if shellQuote("it's") != `'it'\''s'` {
		t.Fatalf("single quote escaping: %q", shellQuote("it's"))
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
