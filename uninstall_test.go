package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"rhizome/internal/service"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

func TestSkillNamesMatchEmbedded(t *testing.T) {
	if got := skillNames(); !reflect.DeepEqual(got, []string{skillName}) {
		t.Fatalf("skill names %v", got)
	}
}

func TestParseComponents(t *testing.T) {
	all := map[string]bool{"skills": true, "mcp": true, "hooks": true, "plugins": true}
	cases := map[string]map[string]bool{"": all, " ": all, "skills": {"skills": true}, "mcp, hooks,plugins": {"mcp": true, "hooks": true, "plugins": true}}
	for in, want := range cases {
		if got, err := parseComponents(in); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%q: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"skill", "mcp,tui", ","} {
		if _, err := parseComponents(in); err == nil {
			t.Fatalf("%q accepted", in)
		}
	}
}

func TestStripHooks(t *testing.T) {
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
	hooks, err := stripHooks(doc)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"PreToolUse":  []any{map[string]any{"matcher": "Bash", "hooks": []any{other}}},
		"PostToolUse": []any{map[string]any{"hooks": []any{other}}},
	}
	if !reflect.DeepEqual(hooks, want) || !reflect.DeepEqual(doc["hooks"], want) || doc["theme"] != "dark" {
		t.Fatalf("stripped: %v", doc)
	}
	before, _ := encodeJSON(doc)
	if _, err := stripHooks(doc); err != nil {
		t.Fatal(err)
	}
	if after, _ := encodeJSON(doc); !bytes.Equal(before, after) {
		t.Fatalf("second strip changed the document:\n%s\n%s", before, after)
	}
	// A document without hooks gains no key; an object emptied by the strip stays.
	none := map[string]any{"theme": "dark"}
	if _, err := stripHooks(none); err != nil {
		t.Fatal(err)
	}
	if _, ok := none["hooks"]; ok {
		t.Fatalf("hooks key added: %v", none)
	}
	emptied := map[string]any{"hooks": map[string]any{"Stop": []any{map[string]any{"hooks": []any{stale}}}}}
	if _, err := stripHooks(emptied); err != nil {
		t.Fatal(err)
	}
	if got, ok := emptied["hooks"].(map[string]any); !ok || len(got) != 0 {
		t.Fatalf("emptied hooks: %v", emptied)
	}
	if _, err := stripHooks(map[string]any{"hooks": "nope"}); err == nil {
		t.Fatal("non-object hooks accepted")
	}
	if _, err := stripHooks(map[string]any{"hooks": map[string]any{"Stop": "nope"}}); err == nil {
		t.Fatal("non-array event accepted")
	}
}

func TestStripJSONServer(t *testing.T) {
	doc := map[string]any{
		"oauthAccount": map[string]any{"email": "x"},
		"mcpServers":   map[string]any{"other": map[string]any{"command": "keep"}, "rhizome": map[string]any{"command": "/old"}},
	}
	if err := stripJSONServer(doc); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"oauthAccount": map[string]any{"email": "x"}, "mcpServers": map[string]any{"other": map[string]any{"command": "keep"}}}
	if !reflect.DeepEqual(doc, want) {
		t.Fatalf("%v", doc)
	}
	none := map[string]any{"theme": "dark"}
	if err := stripJSONServer(none); err != nil {
		t.Fatal(err)
	}
	if _, ok := none["mcpServers"]; ok {
		t.Fatalf("mcpServers added: %v", none)
	}
	if err := stripJSONServer(map[string]any{"mcpServers": []any{}}); err == nil {
		t.Fatal("non-object mcpServers accepted")
	}
}

func TestStripCodexServer(t *testing.T) {
	existing := "# my codex config\nmodel = \"gpt-5\"\n\n[mcp_servers.other]\ncommand = \"keep\" # mine\n\n[mcp_servers.rhizome]\ncommand = \"/old\"\nargs = [\"mcp\"]\n\n[mcp_servers.rhizome.env]\nCODEX_HOME = \"/old\"\n\n[tui]\ntheme = \"x\"\n"
	want := "# my codex config\nmodel = \"gpt-5\"\n\n[mcp_servers.other]\ncommand = \"keep\" # mine\n\n[tui]\ntheme = \"x\"\n"
	out, err := stripCodexServer([]byte(existing))
	if err != nil || string(out) != want {
		t.Fatalf("%v\n%s", err, out)
	}
	if again, err := stripCodexServer(out); err != nil || string(again) != want {
		t.Fatalf("not idempotent: %v\n%s", err, again)
	}
	// A table at the end leaves one trailing newline; a file that was only the
	// table becomes empty; a file without the entry is returned as is.
	cases := map[string]string{
		"model = \"m\"\n\n[mcp_servers.rhizome]\ncommand = \"/old\"\n": "model = \"m\"\n",
		"[mcp_servers.rhizome]\ncommand = \"/old\"\n":                  "",
		"a = 1\n\n\n": "a = 1\n\n\n",
		"":            "",
	}
	for in, want := range cases {
		if out, err := stripCodexServer([]byte(in)); err != nil || string(out) != want {
			t.Fatalf("%q: %q %v", in, out, err)
		}
	}
	if _, err := stripCodexServer([]byte("model = \n")); err == nil || !strings.Contains(err.Error(), "not valid TOML") {
		t.Fatalf("malformed accepted: %v", err)
	}
	for _, in := range []string{"[mcp_servers]\nrhizome = { command = \"/old\" }\n", "mcp_servers.rhizome.command = \"/old\"\n"} {
		if _, err := stripCodexServer([]byte(in)); err == nil || !strings.Contains(err.Error(), "[mcp_servers.rhizome] table") {
			t.Fatalf("%q accepted: %v", in, err)
		}
	}
}

// Installing and then stripping returns a user's file to its starting point.
func TestInstallThenStripRoundTrip(t *testing.T) {
	other := map[string]any{"type": "command", "command": "echo hi"}
	settings := map[string]any{"theme": "dark", "hooks": map[string]any{"PreToolUse": []any{map[string]any{"matcher": "Bash", "hooks": []any{other}}}}}
	for _, agentName := range []string{"claude", "codex"} {
		b, _ := encodeJSON(settings)
		doc, _ := decodeJSON(b)
		if err := mergeHooks(doc, hookTemplate(agentName)); err != nil {
			t.Fatal(err)
		}
		if _, err := stripHooks(doc); err != nil {
			t.Fatal(err)
		}
		if want, _ := decodeJSON(b); !reflect.DeepEqual(doc, want) {
			t.Fatalf("%s hooks round trip: %v", agentName, doc)
		}
	}
	claude := map[string]any{"oauthAccount": map[string]any{"email": "x"}, "mcpServers": map[string]any{"other": map[string]any{"command": "keep"}}}
	b, _ := encodeJSON(claude)
	doc, _ := decodeJSON(b)
	if err := mergeJSONServer(doc, mcpServer{Command: "/rhizome", Args: []string{"mcp", "--agent", "claude"}}); err != nil {
		t.Fatal(err)
	}
	if err := stripJSONServer(doc); err != nil {
		t.Fatal(err)
	}
	if want, _ := decodeJSON(b); !reflect.DeepEqual(doc, want) {
		t.Fatalf("server round trip: %v", doc)
	}
	fixture := []byte("# comment\nmodel = \"m\"\n\n[mcp_servers.other]\ncommand = \"keep\"\n")
	merged, err := mergeCodexConfig(fixture, mcpServer{Command: "/rhizome", Args: []string{"mcp", "--agent", "codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if stripped, err := stripCodexServer(merged); err != nil || !bytes.Equal(stripped, fixture) {
		t.Fatalf("codex round trip: %v\n%s", err, stripped)
	}
}

// installAll installs every agent into the isolated home and returns the
// agents, the installer and its captured output.
func installAll(t *testing.T) ([]agent, agentInstaller, string) {
	t.Helper()
	c, home := installEnv(t, nil)
	var out bytes.Buffer
	in := agentInstaller{exe: filepath.Join(home, "Rhizome App", "rhizome"), home: home, c: c, lookPath: noPath, now: fixedNow, out: &out}
	agents := detectAgents(c, noPath)
	for _, a := range agents {
		if err := in.install(a); err != nil {
			t.Fatal(err)
		}
	}
	return agents, in, home
}

func everything(agents []agent) uninstaller {
	checked := make([]bool, len(agents))
	for i := range checked {
		checked[i] = true
	}
	only, _ := parseComponents("")
	return uninstaller{agents: agents, checked: checked, only: only, now: fixedNow}
}

func backups(t *testing.T, home string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(home, func(p string, _ os.DirEntry, err error) error {
		if err == nil && strings.Contains(filepath.Base(p), ".bak-") {
			found = append(found, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func snapshot(t *testing.T, paths ...string) map[string][]byte {
	t.Helper()
	got := map[string][]byte{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		got[p] = b
	}
	return got
}

func TestUninstallAgentsFullAndRepeat(t *testing.T) {
	agents, in, home := installAll(t)
	claudeJSON, claudeSettings := filepath.Join(home, ".claude.json"), filepath.Join(home, ".claude", "settings.json")
	codexTOML, codexHooks := filepath.Join(home, ".codex", "config.toml"), filepath.Join(home, ".codex", "hooks.json")
	openCodeDir := filepath.Join(home, ".config", "opencode")
	// User content next to Rhizome's: a hook in a Rhizome group, another event,
	// another server, unrelated keys, and a Codex file with comments and tables.
	other := map[string]any{"type": "command", "command": "echo hi"}
	doc := readJSON(t, claudeSettings)
	hooks := doc["hooks"].(map[string]any)
	group := hooks["PreToolUse"].([]any)[0].(map[string]any)
	group["hooks"] = append(group["hooks"].([]any), other)
	hooks["PostToolUse"] = []any{map[string]any{"hooks": []any{other}}}
	doc["theme"] = "dark"
	b, _ := encodeJSON(doc)
	if err := os.WriteFile(claudeSettings, b, 0o600); err != nil {
		t.Fatal(err)
	}
	doc = readJSON(t, claudeJSON)
	doc["mcpServers"].(map[string]any)["other"] = map[string]any{"command": "keep"}
	doc["numStartups"] = 3
	b, _ = encodeJSON(doc)
	if err := os.WriteFile(claudeJSON, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(claudeJSON, 0o640); err != nil {
		t.Fatal(err)
	}
	codexFixture := "# mine\nmodel = \"m\"\n\n[mcp_servers.other]\ncommand = \"keep\" # keep me\n"
	if err := os.WriteFile(codexTOML, []byte(codexFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := in.install(agents[1]); err != nil {
		t.Fatal(err)
	}
	for _, p := range backups(t, home) {
		os.Remove(p)
	}
	before := snapshot(t, claudeJSON, claudeSettings, codexTOML, codexHooks)

	// The plan writes nothing.
	u := everything(agents)
	var out bytes.Buffer
	u.out = &out
	if err := u.run(false); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "would remove") != 9 || strings.Contains(out.String(), "backup") || strings.Contains(out.String(), "next:") {
		t.Fatalf("plan:\n%s", out.String())
	}
	if !reflect.DeepEqual(snapshot(t, claudeJSON, claudeSettings, codexTOML, codexHooks), before) || len(backups(t, home)) != 0 {
		t.Fatal("plan changed files")
	}

	out.Reset()
	if err := u.run(true); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	for _, want := range []string{
		"skill: removed " + filepath.Join(home, ".claude", "skills", skillName),
		"mcp:   removed rhizome server in " + claudeJSON + " (backup: " + claudeJSON + ".bak-20261021T180000Z)",
		"hooks: removed rhizome hooks in " + claudeSettings + " (backup: ",
		"mcp:   removed rhizome server in " + codexTOML,
		"hooks: removed rhizome hooks in " + codexHooks,
		"plugin: removed " + filepath.Join(openCodeDir, "plugins", "rhizome.js"),
		"tui:   removed " + filepath.Join(openCodeDir, "plugins", "rhizome-tui", "tui.js"),
		"next:  restart Claude Code", "project-scope", "fresh Codex session", "hand-added rhizome MCP entry", "~/.rhizome (configuration, identity, pairings, history) is kept",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "token") {
		t.Fatalf("output mentions tokens:\n%s", out.String())
	}
	for _, a := range agents {
		if _, err := os.Lstat(filepath.Join(a.Skills, skillName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s skill kept: %v", a.Name, err)
		}
	}
	doc = readJSON(t, claudeJSON)
	if !reflect.DeepEqual(doc["mcpServers"], map[string]any{"other": map[string]any{"command": "keep"}}) || doc["numStartups"] == nil {
		t.Fatalf("claude json: %v", doc)
	}
	doc = readJSON(t, claudeSettings)
	wantHooks := map[string]any{"PreToolUse": []any{map[string]any{"matcher": "^(?!mcp__rhizome__).*$", "hooks": []any{other}}}, "PostToolUse": []any{map[string]any{"hooks": []any{other}}}}
	if !reflect.DeepEqual(doc["hooks"], wantHooks) || doc["theme"] != "dark" {
		t.Fatalf("claude settings: %v", doc)
	}
	if b, _ := os.ReadFile(codexTOML); string(b) != codexFixture {
		t.Fatalf("codex config:\n%s", b)
	}
	if got, ok := readJSON(t, codexHooks)["hooks"].(map[string]any); !ok || len(got) != 0 {
		t.Fatalf("codex hooks: %v", got)
	}
	for _, p := range []string{filepath.Join(openCodeDir, "plugins", "rhizome.js"), filepath.Join(openCodeDir, "plugins", "rhizome-tui")} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s kept: %v", p, err)
		}
	}
	if st, _ := os.Stat(claudeJSON); st.Mode().Perm() != 0o640 {
		t.Fatalf("mode not preserved: %v", st.Mode())
	}
	got := backups(t, home)
	if len(got) != 4 {
		t.Fatalf("backups: %v", got)
	}
	if b, _ := os.ReadFile(claudeJSON + ".bak-20261021T180000Z"); !bytes.Equal(b, before[claudeJSON]) {
		t.Fatalf("backup content: %s", b)
	}

	// A second run finds nothing and makes no backup.
	out.Reset()
	if err := u.run(true); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "absent") != 5 || strings.Count(out.String(), "unchanged") != 4 || strings.Contains(out.String(), "backup") || strings.Contains(out.String(), "next:") {
		t.Fatalf("repeat:\n%s", out.String())
	}
	if again := backups(t, home); !reflect.DeepEqual(again, got) {
		t.Fatalf("backups after repeat: %v", again)
	}
	// Setup reinstalls every component.
	out.Reset()
	in.out = &out
	for _, a := range agents {
		if err := in.install(a); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Count(out.String(), "skill: installed") != 3 || !strings.Contains(out.String(), "plugin: installed") || !strings.Contains(out.String(), "updated rhizome server in "+claudeJSON) {
		t.Fatalf("reinstall:\n%s", out.String())
	}
}

func TestUninstallOnlyAndSymlinks(t *testing.T) {
	agents, _, home := installAll(t)
	files := []string{filepath.Join(home, ".claude.json"), filepath.Join(home, ".claude", "settings.json"), filepath.Join(home, ".codex", "config.toml"), filepath.Join(home, ".codex", "hooks.json"), agents[2].Plugin, agents[2].TUI}
	before := snapshot(t, files...)
	u := everything(agents)
	u.only, _ = parseComponents("skills")
	var out bytes.Buffer
	u.out = &out
	if err := u.run(true); err != nil {
		t.Fatal(err)
	}
	for _, a := range agents {
		if _, err := os.Lstat(filepath.Join(a.Skills, skillName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s skill kept", a.Name)
		}
	}
	if !reflect.DeepEqual(snapshot(t, files...), before) || len(backups(t, home)) != 0 {
		t.Fatalf("--only skills touched other components:\n%s", out.String())
	}
	if strings.Contains(out.String(), "mcp:") || strings.Contains(out.String(), "plugin:") {
		t.Fatalf("unselected components reported:\n%s", out.String())
	}
	// A symlinked skill or plugin is reported and neither the link nor its
	// target is touched.
	checkout := filepath.Join(home, "checkout")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "SKILL.md"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(agents[0].Skills, skillName)
	if err := os.Symlink(checkout, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(agents[2].Plugin); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(checkout, "SKILL.md"), agents[2].Plugin); err != nil {
		t.Fatal(err)
	}
	u.only, _ = parseComponents("skills,plugins")
	out.Reset()
	if err := u.run(true); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"skill: skipped (symlink to " + checkout + ") " + link, "plugin: skipped (symlink to " + filepath.Join(checkout, "SKILL.md") + ")"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("skill link removed: %v", err)
	}
	if st, err := os.Lstat(agents[2].Plugin); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("plugin link removed: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(checkout, "SKILL.md")); err != nil || string(b) != "mine" {
		t.Fatalf("symlink target touched: %s %v", b, err)
	}
	if _, err := os.Lstat(agents[2].TUI); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("regular plugin kept beside the symlinked one")
	}
}

func TestUninstallMalformedFilesContinue(t *testing.T) {
	agents, _, home := installAll(t)
	claudeJSON, codexTOML := filepath.Join(home, ".claude.json"), filepath.Join(home, ".codex", "config.toml")
	if err := os.WriteFile(claudeJSON, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexTOML, []byte("[mcp_servers]\nrhizome = { command = \"/old\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	u := everything(agents)
	var out bytes.Buffer
	u.out = &out
	err := u.run(true)
	if err == nil || !strings.Contains(err.Error(), "errors") {
		t.Fatalf("errors not reported: %v", err)
	}
	for _, want := range []string{"mcp:   error: " + claudeJSON + ": not valid JSON", "mcp:   error: " + codexTOML + ": mcp_servers.rhizome must be a [mcp_servers.rhizome] table", "hooks: removed rhizome hooks in " + filepath.Join(home, ".claude", "settings.json"), "tui:   removed"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	if b, _ := os.ReadFile(claudeJSON); string(b) != "{not json" {
		t.Fatalf("malformed file replaced: %q", b)
	}
	if b, _ := os.ReadFile(codexTOML); !strings.Contains(string(b), "{ command") {
		t.Fatalf("inline definition changed: %q", b)
	}
	for _, p := range backups(t, home) {
		if strings.HasPrefix(p, claudeJSON) || strings.HasPrefix(p, codexTOML) {
			t.Fatalf("backup of an untouched file: %s", p)
		}
	}
}

func TestUninstallService(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, f serviceFixture)
		apply   bool
		manage  func(t *testing.T, f serviceFixture) error
		want    serviceRemoval
		wantMsg string
	}{
		{"not installed", nil, true, nil, serviceRemoval{}, ""},
		{"not installed while an observer runs elsewhere", func(t *testing.T, f serviceFixture) { f.hold(t) }, true, nil, serviceRemoval{Running: true}, ""},
		{"plan leaves an installed definition", func(t *testing.T, f serviceFixture) { f.writeDefinition(t); f.hold(t) }, false, nil, serviceRemoval{Found: true, Running: true}, ""},
		{"installed service is stopped and removed", func(t *testing.T, f serviceFixture) { f.writeDefinition(t); f.hold(t) }, true, func(t *testing.T, f serviceFixture) error { return os.Remove(f.definition) }, serviceRemoval{Found: true, Running: true}, ""},
		{"manager error keeps the definition", func(t *testing.T, f serviceFixture) { f.writeDefinition(t) }, true, func(*testing.T, serviceFixture) error { return errors.New("launchctl: boom") }, serviceRemoval{Found: true}, "service not uninstalled: launchctl: boom"},
		{"success claim with the definition still present", func(t *testing.T, f serviceFixture) { f.writeDefinition(t) }, true, func(*testing.T, serviceFixture) error { return nil }, serviceRemoval{Found: true}, "still present"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newServiceFixture(t)
			if tc.prepare != nil {
				tc.prepare(t, f)
			}
			var actions []string
			manage := func(ctx context.Context, action string, c service.Config) error {
				actions = append(actions, action)
				if c.ConfigPath != f.path || c.Observer.Runtime != f.runtime {
					t.Errorf("service config %+v", c)
				}
				return tc.manage(t, f)
			}
			got, err := uninstallService(context.Background(), f.path, tc.apply, manage)
			if tc.manage == nil && len(actions) != 0 {
				t.Fatalf("service manager called: %v", actions)
			}
			if tc.manage != nil && !reflect.DeepEqual(actions, []string{"uninstall"}) {
				t.Fatalf("actions %v", actions)
			}
			if got.Runtime != f.runtime || got.Found != tc.want.Found || got.Running != tc.want.Running {
				t.Fatalf("outcome %+v", got)
			}
			if tc.wantMsg == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantMsg != "" && (err == nil || !strings.Contains(err.Error(), tc.wantMsg)) {
				t.Fatalf("got %v, want %q", err, tc.wantMsg)
			}
			if b, _ := os.ReadFile(f.path); !strings.Contains(string(b), f.runtime) {
				t.Fatalf("configuration changed: %s", b)
			}
		})
	}
	// A missing configuration without a definition is simply not installed;
	// with a definition the resolution error is reported. An invalid
	// configuration never reaches the service manager.
	f := newServiceFixture(t)
	missing := filepath.Join(filepath.Dir(f.path), "missing.toml")
	called := false
	manage := func(context.Context, string, service.Config) error { called = true; return nil }
	if got, err := uninstallService(context.Background(), missing, true, manage); err != nil || got.Found || called {
		t.Fatalf("missing configuration: %+v %v %v", got, err, called)
	}
	f.writeDefinition(t)
	if _, err := uninstallService(context.Background(), missing, true, manage); err == nil || called {
		t.Fatalf("missing configuration with a definition: %v %v", err, called)
	}
	if _, err := os.Stat(f.definition); err != nil {
		t.Fatal("definition removed without a configuration")
	}
	if err := os.WriteFile(f.path, []byte("runtime_dir = relative\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := uninstallService(context.Background(), f.path, true, manage); err == nil || called {
		t.Fatalf("invalid configuration reached the service manager: %v %v", err, called)
	}
}

// The real entry point, in a subprocess with an isolated home: setup then
// uninstall per the documented flags, without a terminal.
func TestUninstallCLI(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home with space")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(ok bool, args ...string) string {
		t.Helper()
		cmd := configProcess(t, home, args...)
		cmd.Dir = t.TempDir()
		b, err := cmd.CombinedOutput()
		if (err == nil) != ok {
			t.Fatalf("%v: %v\n%s", args, err, b)
		}
		return string(b)
	}
	claudeJSON, codexTOML := filepath.Join(home, ".claude.json"), filepath.Join(home, ".codex", "config.toml")
	rhizomeConfig := filepath.Join(home, ".rhizome", "config.toml")
	skills := func() []string {
		var present []string
		for _, dir := range []string{filepath.Join(home, ".claude"), filepath.Join(home, ".codex"), filepath.Join(home, ".config", "opencode")} {
			if _, err := os.Stat(filepath.Join(dir, "skills", skillName, "SKILL.md")); err == nil {
				present = append(present, filepath.Base(dir))
			}
		}
		return present
	}
	hasClaudeServer := func() bool {
		servers, _ := readJSON(t, claudeJSON)["mcpServers"].(map[string]any)
		return servers["rhizome"] != nil
	}
	hasCodexServer := func() bool {
		var doc map[string]any
		if _, err := toml.DecodeFile(codexTOML, &doc); err != nil {
			t.Fatal(err)
		}
		return tomlLookup(doc, "mcp_servers", "rhizome") != nil
	}
	run(true, "setup", "--agents", "all", "--network", "local")
	config, err := os.ReadFile(rhizomeConfig)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills()) != 3 || !hasClaudeServer() || !hasCodexServer() {
		t.Fatal("setup did not install everything")
	}
	// Without a terminal, a selection and --yes are required before anything happens.
	if out := run(false, "uninstall"); !strings.Contains(out, "--agents") {
		t.Fatalf("no selection:\n%s", out)
	}
	if out := run(false, "uninstall", "--agents", "all"); !strings.Contains(out, "--yes") {
		t.Fatalf("no confirmation:\n%s", out)
	}
	if out := run(false, "uninstall", "--all", "--agents", "codex", "--yes"); !strings.Contains(out, "--all") {
		t.Fatalf("conflicting selection:\n%s", out)
	}
	out := run(true, "uninstall", "--all", "--dry-run")
	if strings.Count(out, "would remove") != 9 || !strings.Contains(out, "service: not installed") || strings.Contains(out, "backup") {
		t.Fatalf("dry run:\n%s", out)
	}
	if len(skills()) != 3 || !hasClaudeServer() || !hasCodexServer() || len(backups(t, home)) != 0 {
		t.Fatal("dry run changed files")
	}
	// Service only: nothing installed in this home, agents untouched.
	if out := run(true, "uninstall", "--service", "--yes"); !strings.Contains(out, "service: not installed") || strings.Contains(out, "Claude Code:") {
		t.Fatalf("service only:\n%s", out)
	}
	if len(skills()) != 3 || !hasClaudeServer() {
		t.Fatal("--service touched agents")
	}
	// One agent only.
	out = run(true, "uninstall", "--agents", "codex", "--yes")
	if strings.Contains(out, "Claude Code:") || !reflect.DeepEqual(skills(), []string{".claude", "opencode"}) || !hasClaudeServer() || hasCodexServer() {
		t.Fatalf("codex only:\n%s", out)
	}
	// Skills only.
	run(true, "uninstall", "--agents", "all", "--only", "skills", "--yes")
	if len(skills()) != 0 || !hasClaudeServer() {
		t.Fatal("--only skills removed more than skills")
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "opencode", "plugins", "rhizome.js")); err != nil {
		t.Fatal("--only skills removed plugins")
	}
	// Everything.
	out = run(true, "uninstall", "--all", "--yes")
	if hasClaudeServer() || strings.Contains(out, "token") {
		t.Fatalf("full uninstall:\n%s", out)
	}
	if hooks, _ := readJSON(t, filepath.Join(home, ".claude", "settings.json"))["hooks"].(map[string]any); len(hooks) != 0 {
		t.Fatalf("claude hooks kept: %v", hooks)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "opencode", "plugins", "rhizome-tui")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("tui plugin directory kept")
	}
	if b, _ := os.ReadFile(rhizomeConfig); !bytes.Equal(b, config) {
		t.Fatalf("rhizome configuration changed:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(home, ".rhizome")); err != nil {
		t.Fatal("~/.rhizome removed")
	}
	count := len(backups(t, home))
	if count == 0 {
		t.Fatal("no backups written")
	}
	if out := run(true, "uninstall", "--list"); strings.Contains(out, "installed") || strings.Count(out, "absent") != 9 {
		t.Fatalf("list:\n%s", out)
	}
	// Idempotent: no new backups.
	if out := run(true, "uninstall", "--agents", "all", "--yes"); strings.Contains(out, "backup") || strings.Contains(out, "removed") {
		t.Fatalf("repeat:\n%s", out)
	}
	if len(backups(t, home)) != count {
		t.Fatal("repeat wrote backups")
	}
	// Setup reinstalls cleanly.
	out = run(true, "setup", "--agents", "all")
	if strings.Count(out, "skill: installed") != 3 || !hasClaudeServer() || !hasCodexServer() {
		t.Fatalf("reinstall:\n%s", out)
	}
}

var _ = time.Second
