package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"rhizome/internal/config"
	"strings"
	"testing"
)

func TestDetectAgents(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	c, err := config.Resolve(nil, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	missing := func(string) (string, error) { return "", errors.New("not found") }
	agents := detectAgents(c, missing)
	if len(agents) != 3 || agents[0].Name != "claude" || agents[1].Name != "codex" || agents[2].Name != "opencode" {
		t.Fatalf("unexpected agents %+v", agents)
	}
	if !agents[0].Detected || agents[0].Skills != filepath.Join(home, ".claude", "skills") {
		t.Fatalf("claude should be detected from ~/.claude: %+v", agents[0])
	}
	if agents[1].Detected {
		t.Fatalf("codex should not be detected: %+v", agents[1])
	}

	// PATH lookup detects an agent without a config directory.
	onPath := func(name string) (string, error) {
		if name == "codex" {
			return "/usr/local/bin/codex", nil
		}
		return "", errors.New("not found")
	}
	agents = detectAgents(c, onPath)
	if !agents[1].Detected || !strings.Contains(agents[1].Reason, "PATH") {
		t.Fatalf("codex should be detected from PATH: %+v", agents[1])
	}

	// Environment overrides relocate the skills directory.
	env := map[string]string{"CLAUDE_CONFIG_DIR": "/cfg/claude", "CODEX_HOME": "/cfg/codex"}
	c, err = config.Resolve(nil, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	agents = detectAgents(c, missing)
	if agents[0].Skills != filepath.Join("/cfg/claude", "skills") || agents[1].Skills != filepath.Join("/cfg/codex", "skills") {
		t.Fatalf("env overrides ignored: %+v", agents)
	}
}

func TestInstallSkill(t *testing.T) {
	skills := filepath.Join(t.TempDir(), "skills")
	status, err := installSkill(skills)
	if err != nil || status != "installed" {
		t.Fatalf("first install: %q %v", status, err)
	}
	path := filepath.Join(skills, skillName, "SKILL.md")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := skillFS.ReadFile("skills/rhizome-collab/SKILL.md")
	if !bytes.Equal(got, want) || !bytes.HasPrefix(got, []byte("---\nname: rhizome-collab\n")) {
		t.Fatalf("installed SKILL.md does not match embedded skill")
	}
	if status, err = installSkill(skills); err != nil || status != "unchanged" {
		t.Fatalf("second install: %q %v", status, err)
	}
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if status, err = installSkill(skills); err != nil || status != "updated" {
		t.Fatalf("install over stale copy: %q %v", status, err)
	}
	if got, _ = os.ReadFile(path); !bytes.Equal(got, want) {
		t.Fatalf("stale copy was not replaced")
	}
}

func TestInstallSkillLeavesSymlink(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	skills := filepath.Join(root, "skills")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(checkout, filepath.Join(skills, skillName)); err != nil {
		t.Fatal(err)
	}
	status, err := installSkill(skills)
	if err != nil || !strings.HasPrefix(status, "skipped") {
		t.Fatalf("symlink: %q %v", status, err)
	}
	if entries, _ := os.ReadDir(checkout); len(entries) != 0 {
		t.Fatalf("symlink target was written to: %v", entries)
	}
}

func TestInstallSkillRejectsFile(t *testing.T) {
	skills := t.TempDir()
	if err := os.WriteFile(filepath.Join(skills, skillName), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := installSkill(skills); err == nil {
		t.Fatal("expected error when destination is a regular file")
	}
}

func TestParseAgentFlag(t *testing.T) {
	agents := []agent{{Name: "claude"}, {Name: "codex"}}
	cases := map[string][]bool{"all": {true, true}, "claude": {true, false}, "codex, claude": {true, true}, "codex": {false, true}}
	for in, want := range cases {
		got, err := parseAgentFlag(in, agents)
		if err != nil || len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("%q: got %v err %v, want %v", in, got, err, want)
		}
	}
	if _, err := parseAgentFlag("cursor", agents); err == nil || !strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("expected unknown agent error, got %v", err)
	}
}

func TestPickAgents(t *testing.T) {
	agents := []agent{{Name: "claude", Label: "Claude Code", Detected: true}, {Name: "codex", Label: "Codex"}}
	run := func(keys string, start []bool) ([]bool, error) {
		var out bytes.Buffer
		checked := append([]bool(nil), start...)
		return pickAgents(strings.NewReader(keys), &out, agents, checked)
	}
	// Enter keeps the pre-selection.
	if got, err := run("\r", []bool{true, false}); err != nil || !got[0] || got[1] {
		t.Fatalf("enter: %v %v", got, err)
	}
	// Down then space toggles the second item.
	if got, err := run("\x1b[B \r", []bool{true, false}); err != nil || !got[0] || !got[1] {
		t.Fatalf("down+space: %v %v", got, err)
	}
	// j/k vim keys and space on the first item untoggles it.
	if got, err := run("jk \n", []bool{true, true}); err != nil || got[0] || !got[1] {
		t.Fatalf("jk+space: %v %v", got, err)
	}
	// 'a' selects all when any is unchecked, then none when all are checked.
	if got, err := run("a\r", []bool{true, false}); err != nil || !got[0] || !got[1] {
		t.Fatalf("a: %v %v", got, err)
	}
	if got, err := run("aa\r", []bool{true, false}); err != nil || got[0] || got[1] {
		t.Fatalf("aa: %v %v", got, err)
	}
	// q cancels; EOF without enter is an error.
	if _, err := run("q", []bool{true, false}); err == nil {
		t.Fatal("q should cancel")
	}
	if _, err := run("", []bool{true, false}); err == nil {
		t.Fatal("EOF should error")
	}
	// Unknown keys are ignored.
	if got, err := run("zz\r", []bool{false, true}); err != nil || got[0] || !got[1] {
		t.Fatalf("ignored keys: %v %v", got, err)
	}
}

func TestDetectOpenCode(t *testing.T) {
	home := t.TempDir()
	missing := func(string) (string, error) { return "", errors.New("missing") }
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	c, err := config.Resolve(nil, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	agents := detectAgents(c, missing)
	oc := agents[2]
	if oc.Name != "opencode" || oc.Detected || oc.Skills != filepath.Join(home, "xdg", "opencode", "skills") || oc.Plugin != filepath.Join(home, "xdg", "opencode", "plugins", "rhizome.js") {
		t.Fatalf("opencode %+v", oc)
	}
	if err := os.MkdirAll(filepath.Join(home, ".config", "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	agents = detectAgents(c, missing)
	if !agents[2].Detected || agents[0].Plugin != "" || agents[1].Plugin != "" {
		t.Fatalf("detection %+v", agents)
	}
}

func TestInstallPlugin(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "opencode", "plugins", "rhizome.js")
	for _, want := range []string{"installed", "unchanged"} {
		if status, err := installPlugin(dest); err != nil || status != want {
			t.Fatalf("status %q %v want %q", status, err, want)
		}
	}
	if err := os.WriteFile(dest, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if status, err := installPlugin(dest); err != nil || status != "updated" {
		t.Fatalf("update %q %v", status, err)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, openCodePlugin) {
		t.Fatal("plugin content mismatch")
	}
	link := filepath.Join(t.TempDir(), "rhizome.js")
	if err := os.Symlink(dest, link); err != nil {
		t.Fatal(err)
	}
	if status, err := installPlugin(link); err != nil || !strings.HasPrefix(status, "skipped (symlink") {
		t.Fatalf("symlink %q %v", status, err)
	}
}
