package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"rhizome/internal/config"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// This file installs Rhizome into an agent's user-scope configuration: the MCP
// server registration and the binding hooks. Every write is a format-aware
// merge that touches only Rhizome-owned entries, keeps a timestamped backup of
// a changed file, and leaves malformed files untouched.

// serverName is the MCP server key the hooks and the Claude channel flag refer to.
const serverName = "rhizome"

// maxConfigFile bounds how much of an agent configuration file setup reads.
const maxConfigFile = 64 << 20

// ownedEnv lists environment keys setup manages inside the server entry. Any
// other key in an existing entry's env table is preserved.
var ownedEnv = []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME"}

// mcpServer is the Rhizome stdio server entry written into an agent's global
// configuration. It never carries a token.
type mcpServer struct {
	Command string
	Args    []string
	Env     map[string]string
}

// rhizomeServer builds the server entry for agentName. exe is the absolute
// Rhizome executable. The Rhizome configuration file is passed explicitly only
// when it was selected with --config/RHIZOME_CONFIG, and it must exist: the
// default path resolves at launch. The agent home is pinned through the
// environment only when it differs from the agent's default.
func rhizomeServer(exe, agentName string, c config.Config, explicitConfig bool, home string, lookPath func(string) (string, error)) (mcpServer, error) {
	s := mcpServer{Command: exe, Args: []string{"mcp", "--agent", agentName}}
	if explicitConfig {
		if st, err := os.Stat(c.Path); err != nil || !st.Mode().IsRegular() {
			return s, fmt.Errorf("configuration %s does not exist; run setup with --network to create it before registering MCP", c.Path)
		}
		s.Args = append(s.Args, "--config", c.Path)
	}
	switch agentName {
	case "claude":
		if c.Claude.ConfigDir != filepath.Join(home, ".claude") {
			s.Env = map[string]string{"CLAUDE_CONFIG_DIR": c.Claude.ConfigDir}
		}
	case "codex":
		if c.Codex.Home != filepath.Join(home, ".codex") {
			s.Env = map[string]string{"CODEX_HOME": c.Codex.Home}
		}
		bin := c.Codex.Bin
		if filepath.Base(bin) == bin {
			// A bare name is resolved now so the registration does not depend
			// on the PATH the agent launches MCP servers with.
			if p, err := lookPath(bin); err == nil {
				bin = p
			} else {
				bin = ""
			}
		}
		if bin != "" {
			s.Args = append(s.Args, "--codex-bin", bin)
		}
	default:
		return s, fmt.Errorf("unsupported agent %q", agentName)
	}
	return s, nil
}

// claudeGlobalConfig is the file holding Claude's user-scope MCP servers:
// ~/.claude.json, or <CLAUDE_CONFIG_DIR>/.claude.json for a non-default
// configuration directory.
func claudeGlobalConfig(configDir, home string) string {
	if configDir == filepath.Join(home, ".claude") {
		return filepath.Join(home, ".claude.json")
	}
	return filepath.Join(configDir, ".claude.json")
}

// rhizomeHook is one observe_session hook entry for event. Claude receives the
// agent placeholders; Codex 0.163.0 rejects placeholders it cannot fill.
func rhizomeHook(agentName, event string) map[string]any {
	input := map[string]any{
		"session_id":      "${session_id}",
		"transcript_path": "${transcript_path}",
		"cwd":             "${cwd}",
		"hook_event_name": "${hook_event_name}",
	}
	timeout := json.Number("10")
	if agentName == "claude" {
		input["agent_id"], input["agent_type"] = "${agent_id}", "${agent_type}"
		timeout = json.Number("5")
	}
	switch event {
	case "SessionStart":
		input["source"] = "${source}"
	case "PreToolUse":
		input["tool_name"] = "${tool_name}"
	}
	return map[string]any{"type": "mcp_tool", "server": serverName, "tool": "observe_session", "input": input, "timeout": timeout}
}

// hookTemplate returns the hook groups setup installs for agentName, keyed by
// event, in the shape both agents decode from their hooks JSON. The PreToolUse
// matcher excludes Rhizome's own tools so hooks never feed back into collection.
func hookTemplate(agentName string) map[string][]any {
	events := []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "Stop"}
	matcher := "^(Bash|Edit|Write|apply_patch|exec_command|write_stdin)$"
	if agentName == "claude" {
		events = append(events, "SessionEnd")
		matcher = "^(?!mcp__rhizome__).*$"
	}
	out := map[string][]any{}
	for _, event := range events {
		group := map[string]any{"hooks": []any{rhizomeHook(agentName, event)}}
		if event == "PreToolUse" {
			group["matcher"] = matcher
		}
		out[event] = []any{group}
	}
	return out
}

// isRhizomeHook reports whether a decoded hook entry targets the Rhizome server.
func isRhizomeHook(v any) bool {
	h, ok := v.(map[string]any)
	return ok && h["type"] == "mcp_tool" && h["server"] == serverName
}

// mergeHooks removes every Rhizome hook entry from doc["hooks"], dropping groups
// that held nothing else, then appends the template groups. Other hooks,
// matchers and top-level keys are untouched.
func mergeHooks(doc map[string]any, template map[string][]any) error {
	hooks := map[string]any{}
	if v, ok := doc["hooks"]; ok && v != nil {
		if hooks, ok = v.(map[string]any); !ok {
			return errors.New(`"hooks" is not an object`)
		}
	}
	for event, v := range hooks {
		groups, ok := v.([]any)
		if !ok {
			return fmt.Errorf("hooks.%s is not an array", event)
		}
		kept := make([]any, 0, len(groups))
		for _, g := range groups {
			group, ok := g.(map[string]any)
			if !ok {
				kept = append(kept, g)
				continue
			}
			if entries, ok := group["hooks"].([]any); ok {
				rest := make([]any, 0, len(entries))
				for _, h := range entries {
					if !isRhizomeHook(h) {
						rest = append(rest, h)
					}
				}
				if len(rest) == 0 && len(entries) > 0 {
					continue // Rhizome-only group.
				}
				group["hooks"] = rest
			}
			kept = append(kept, group)
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	for event, groups := range template {
		existing, _ := hooks[event].([]any)
		hooks[event] = append(existing, groups...)
	}
	doc["hooks"] = hooks
	return nil
}

// applyServer writes s into entry, keeping keys and env variables setup does
// not own.
func applyServer(entry map[string]any, s mcpServer) {
	entry["command"] = s.Command
	args := make([]any, len(s.Args))
	for i, a := range s.Args {
		args[i] = a
	}
	entry["args"] = args
	env, hadEnv := entry["env"].(map[string]any)
	if env == nil {
		env = map[string]any{}
	}
	for _, k := range ownedEnv {
		delete(env, k)
	}
	for k, v := range s.Env {
		env[k] = v
	}
	if len(env) > 0 || hadEnv {
		entry["env"] = env
	} else {
		delete(entry, "env")
	}
}

// mergeJSONServer sets doc.mcpServers.rhizome to s as a stdio server,
// preserving every other server and the entry's unrelated keys.
func mergeJSONServer(doc map[string]any, s mcpServer) error {
	servers := map[string]any{}
	if v, ok := doc["mcpServers"]; ok && v != nil {
		if servers, ok = v.(map[string]any); !ok {
			return errors.New(`"mcpServers" is not an object`)
		}
	}
	entry, _ := servers[serverName].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
	}
	entry["type"] = "stdio"
	applyServer(entry, s)
	servers[serverName] = entry
	doc["mcpServers"] = servers
	return nil
}

var (
	tomlHeader        = regexp.MustCompile(`^\s*\[`)
	tomlRhizomeHeader = regexp.MustCompile(`^\s*\[\s*mcp_servers\s*\.\s*rhizome\s*(\.[^\]]*)?\]`)
)

// mergeCodexConfig returns existing with [mcp_servers.rhizome] (and its
// subtables) replaced by s. The rest of the file, including comments, is kept
// verbatim: Rhizome's tables are removed line-wise and re-appended at the end.
// The result is parsed again and must define exactly the intended entry, so a
// definition elsewhere (an inline table or dotted keys) is reported instead of
// being duplicated.
func mergeCodexConfig(existing []byte, s mcpServer) ([]byte, error) {
	var doc map[string]any
	if _, err := toml.Decode(string(existing), &doc); err != nil {
		return nil, fmt.Errorf("not valid TOML: %w", err)
	}
	want := map[string]any{}
	if cur, ok := tomlLookup(doc, "mcp_servers", serverName).(map[string]any); ok {
		for k, v := range cur {
			want[k] = v
		}
		if env, ok := cur["env"].(map[string]any); ok {
			copied := map[string]any{}
			for k, v := range env {
				copied[k] = v
			}
			want["env"] = copied
		}
	}
	applyServer(want, s)
	if reflect.DeepEqual(tomlLookup(doc, "mcp_servers", serverName), want) {
		return existing, nil
	}
	var b strings.Builder
	drop := false
	for _, line := range strings.SplitAfter(string(existing), "\n") {
		if tomlHeader.MatchString(line) {
			drop = tomlRhizomeHeader.MatchString(line)
		}
		if !drop {
			b.WriteString(line)
		}
	}
	body := strings.TrimRight(b.String(), "\n")
	if body != "" {
		body += "\n\n"
	}
	section, err := renderRhizomeTable(want)
	if err != nil {
		return nil, err
	}
	body += section
	var check map[string]any
	if _, err := toml.Decode(body, &check); err != nil {
		return nil, fmt.Errorf("mcp_servers.rhizome must be a [mcp_servers.rhizome] table; move or remove the existing definition and run setup again (%v)", err)
	}
	if got := tomlLookup(check, "mcp_servers", serverName); !reflect.DeepEqual(got, want) {
		return nil, errors.New("mcp_servers.rhizome must be a [mcp_servers.rhizome] table; move or remove the existing definition and run setup again")
	}
	return []byte(body), nil
}

// renderRhizomeTable encodes table as the [mcp_servers.rhizome] section text.
func renderRhizomeTable(table map[string]any) (string, error) {
	var b bytes.Buffer
	enc := toml.NewEncoder(&b)
	enc.Indent = ""
	if err := enc.Encode(map[string]any{"mcp_servers": map[string]any{serverName: table}}); err != nil {
		return "", err
	}
	const parent = "[mcp_servers]\n"
	if !strings.HasPrefix(b.String(), parent) {
		return "", errors.New("unexpected TOML encoding of mcp_servers.rhizome")
	}
	return strings.TrimPrefix(b.String(), parent), nil
}

// tomlLookup walks nested tables; it returns nil when any key is missing.
func tomlLookup(doc map[string]any, keys ...string) any {
	var cur any = doc
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		if cur, ok = m[k]; !ok {
			return nil
		}
	}
	return cur
}

// fileResult reports what editFile did to one configuration file.
type fileResult struct {
	Path   string // the file after symlink resolution
	Status string // created, updated or unchanged
	Backup string // copy of the previous content, when it was changed
}

// editFile applies edit to the current content of path (empty when absent) and
// installs the result atomically when it differs. A symlink is followed so the
// link itself survives; the resolved file must be regular. A changed file is
// first copied to <file>.bak-<timestamp> with the same mode, so a failed write
// never loses the original. New files are created 0600.
func editFile(path string, edit func(existing []byte) ([]byte, error), now time.Time) (fileResult, error) {
	res := fileResult{Path: path, Status: "unchanged"}
	st, err := os.Lstat(path)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, err
	}
	if exists && st.Mode()&os.ModeSymlink != 0 {
		if res.Path, err = filepath.EvalSymlinks(path); err != nil {
			return res, fmt.Errorf("%s: cannot resolve symlink: %w", path, err)
		}
		if st, err = os.Stat(res.Path); err != nil {
			return res, err
		}
	}
	if exists && !st.Mode().IsRegular() {
		return res, fmt.Errorf("%s is not a regular file", res.Path)
	}
	var existing []byte
	mode := fs.FileMode(0o600)
	if exists {
		if st.Size() > maxConfigFile {
			return res, fmt.Errorf("%s is larger than %d bytes", res.Path, maxConfigFile)
		}
		if existing, err = os.ReadFile(res.Path); err != nil {
			return res, err
		}
		mode = st.Mode().Perm()
	}
	out, err := edit(existing)
	if err != nil {
		return res, fmt.Errorf("%s: %w", res.Path, err)
	}
	if exists && bytes.Equal(out, existing) {
		return res, nil
	}
	dir := filepath.Dir(res.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return res, err
	}
	if exists {
		if res.Backup, err = writeBackup(res.Path, existing, mode, now); err != nil {
			return res, fmt.Errorf("%s: backup: %w", res.Path, err)
		}
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(res.Path)+".*")
	if err != nil {
		return res, err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return res, err
	}
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return res, fmt.Errorf("%s: write: %w", res.Path, err)
	}
	if err := tmp.Close(); err != nil {
		return res, fmt.Errorf("%s: write: %w", res.Path, err)
	}
	if err := os.Rename(tmp.Name(), res.Path); err != nil {
		return res, fmt.Errorf("%s: install: %w", res.Path, err)
	}
	res.Status = "updated"
	if !exists {
		res.Status = "created"
	}
	return res, nil
}

// writeBackup copies content next to path under a timestamped name that does
// not exist yet and returns it.
func writeBackup(path string, content []byte, mode fs.FileMode, now time.Time) (string, error) {
	base := path + ".bak-" + now.UTC().Format("20060102T150405Z")
	for i := 0; i < 100; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if _, err := f.Write(content); err != nil {
			f.Close()
			return "", err
		}
		return name, f.Close()
	}
	return "", fmt.Errorf("too many backups named %s", base)
}

// editJSON is editFile for a JSON object: a malformed file is rejected, numbers
// keep their spelling, and a file whose object is unchanged is not rewritten.
func editJSON(path string, edit func(doc map[string]any) error, now time.Time) (fileResult, error) {
	return editFile(path, func(existing []byte) ([]byte, error) {
		doc, err := decodeJSON(existing)
		if err != nil {
			return nil, err
		}
		before, err := encodeJSON(doc)
		if err != nil {
			return nil, err
		}
		if err := edit(doc); err != nil {
			return nil, err
		}
		after, err := encodeJSON(doc)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(before, after) {
			return existing, nil
		}
		return after, nil
	}, now)
}

func decodeJSON(data []byte) (map[string]any, error) {
	doc := map[string]any{}
	if len(bytes.TrimSpace(data)) == 0 {
		return doc, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("not valid JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("not valid JSON: trailing data after the object")
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

func encodeJSON(doc map[string]any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// agentInstaller installs the skill, the MCP registration and the hooks for
// selected agents and reports every file it touched.
type agentInstaller struct {
	exe      string
	home     string
	c        config.Config
	explicit bool // configuration selected with --config/RHIZOME_CONFIG
	lookPath func(string) (string, error)
	now      func() time.Time
	out      io.Writer
}

// install performs every step for a. Steps run in order and the first failure
// is returned after the completed steps were reported, so a partial result is
// visible and actionable.
func (in agentInstaller) install(a agent) error {
	fmt.Fprintf(in.out, "%s:\n", a.Label)
	status, err := installSkill(a.Skills)
	if err != nil {
		return fmt.Errorf("%s: skill: %w", a.Label, err)
	}
	fmt.Fprintf(in.out, "  skill: %s %s\n", status, filepath.Join(a.Skills, skillName))
	if a.Name == "opencode" {
		return in.installOpenCode(a)
	}
	if a.MCP == "" && a.Hooks == "" {
		// Setup never writes Cursor's mcp.json or hooks.json; the user merges examples/cursor/.
		for _, note := range agentNotes(a.Name, a) {
			fmt.Fprintf(in.out, "  next:  %s\n", note)
		}
		return nil
	}
	server, err := rhizomeServer(in.exe, a.Name, in.c, in.explicit, in.home, in.lookPath)
	if err != nil {
		return fmt.Errorf("%s: mcp: %w", a.Label, err)
	}
	var mcp, hooks fileResult
	switch a.Name {
	case "claude":
		mcp, err = editJSON(a.MCP, func(doc map[string]any) error { return mergeJSONServer(doc, server) }, in.now())
	case "codex":
		mcp, err = editFile(a.MCP, func(existing []byte) ([]byte, error) { return mergeCodexConfig(existing, server) }, in.now())
	}
	if err != nil {
		return fmt.Errorf("%s: mcp: %w", a.Label, err)
	}
	fmt.Fprintf(in.out, "  mcp:   %s\n", mcp.describe(serverName+" server in "))
	template := hookTemplate(a.Name)
	hooks, err = editJSON(a.Hooks, func(doc map[string]any) error { return mergeHooks(doc, template) }, in.now())
	if err != nil {
		return fmt.Errorf("%s: hooks: %w", a.Label, err)
	}
	fmt.Fprintf(in.out, "  hooks: %s\n", hooks.describe(fmt.Sprintf("%d hook groups in ", len(template))))
	for _, note := range agentNotes(a.Name, a) {
		fmt.Fprintf(in.out, "  next:  %s\n", note)
	}
	return nil
}

// installOpenCode installs the OpenCode relay and TUI toast plugins after the
// skill. OpenCode has no hooks, and its MCP entry stays a manual step (see
// docs/opencode.md), so nothing in its user configuration is edited.
func (in agentInstaller) installOpenCode(a agent) error {
	for _, p := range []struct {
		label, dest string
		content     []byte
	}{{"plugin", a.Plugin, openCodePlugin}, {"tui", a.TUI, openCodeTUIPlugin}} {
		status, err := installPlugin(p.dest, p.content)
		if err != nil {
			return fmt.Errorf("%s: %s: %w", a.Label, p.label, err)
		}
		fmt.Fprintf(in.out, "  %-7s %s %s\n", p.label+":", status, p.dest)
	}
	for _, note := range agentNotes(a.Name, a) {
		fmt.Fprintf(in.out, "  next:  %s\n", note)
	}
	return nil
}

// describe renders "<status> <what><path> (backup: ...)".
func (r fileResult) describe(what string) string {
	s := r.Status + " " + what + r.Path
	if r.Backup != "" {
		s += " (backup: " + r.Backup + ")"
	}
	return s
}

// agentNotes lists the steps that remain for the user after installation.
func agentNotes(agentName string, a agent) []string {
	switch agentName {
	case "claude":
		return []string{
			"restart Claude Code and start it with `claude --dangerously-load-development-channels server:rhizome`; accept the channel and hook prompts it shows.",
			"a project .mcp.json entry named rhizome takes precedence over this user-scope registration; a project .claude/settings.json that also carries the Rhizome hooks runs them twice, so keep them in one place.",
		}
	case "cursor":
		return []string{
			"merge examples/cursor/mcp.json and examples/cursor/hooks.json into " + filepath.Dir(a.Skills) + " with absolute paths (see docs/cursor.md); setup does not write them.",
		}
	case "codex":
		return []string{
			"start a fresh Codex session, review the Rhizome hooks in /hooks and trust them; do not keep the same hooks at project scope.",
		}
	case "opencode":
		return []string{
			"add the rhizome MCP entry to your OpenCode config as described in docs/opencode.md (setup does not edit it), then restart OpenCode so the plugins load; sessions bind on their first prompt.",
		}
	}
	return nil
}
