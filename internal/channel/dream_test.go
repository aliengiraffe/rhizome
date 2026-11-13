package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"rhizome/internal/config"
	"strings"
	"testing"
	"time"
)

func TestDreamHistoryPolicyAndSelection(t *testing.T) {
	hh := newHistoryHarness(t)
	stamp := "2026-11-11T12:00:00Z"
	now, _ := time.Parse(time.RFC3339, "2026-11-12T12:00:00Z")
	records := userText("dream-session", "/p", "u1", stamp, "remember PROMPT_BODY "+testGitHubPAT) +
		assistantText("dream-session", "/p", "a1", stamp, "ASSISTANT_BODY") +
		claudeLine{typ: "assistant", uuid: "a2", session: "dream-session", cwd: "/p", ts: stamp, content: []map[string]any{{"type": "tool_use", "id": "t1", "name": "Bash", "input": "TOOL_INPUT"}}}.String() +
		claudeLine{typ: "user", uuid: "u2", session: "dream-session", cwd: "/p", ts: stamp, content: []map[string]any{{"type": "tool_result", "tool_use_id": "t1", "content": "TOOL_OUTPUT"}}}.String() +
		claudeLine{typ: "user", uuid: "meta", session: "dream-session", cwd: "/p", ts: stamp, meta: true, content: "META_FORBIDDEN"}.String() +
		claudeLine{typ: "assistant", uuid: "r1", session: "dream-session", cwd: "/p", ts: stamp, content: []map[string]any{{"type": "tool_use", "id": "rh", "name": "mcp__rhizome__send", "input": "RHIZOME_FORBIDDEN"}}}.String() +
		userText("dream-session", "/p", "peer", stamp, peerEnvelope("PEER_FORBIDDEN", nil))

	appendLog(t, filepath.Join(hh.scope.ClaudeRoot, "dream-session.jsonl"), records)
	appendLog(t, filepath.Join(hh.scope.ClaudeRoot, "old.jsonl"), assistantText("old", "/p", "old", "2026-10-01T00:00:00Z", "OLD_FORBIDDEN"))
	appendLog(t, filepath.Join(hh.scope.CodexRoot, "rollout-dream.jsonl"), codexMeta("codex-dream", "/p", stamp)+fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"user_message","message":"CODEX_PROMPT"}}`+"\n", stamp)+codexAssistant("a", stamp, "CODEX_ASSISTANT"))
	hh.index()
	c := config.Config{RuntimeDir: hh.scope.Runtime, Dream: config.Dream{Enabled: true, Lookback: "7d", MaxInputBytes: 256 << 10}}
	c.Claude.ProjectsDir = hh.scope.ClaudeRoot
	c.Codex.SessionsDir = hh.scope.CodexRoot
	c.OpenCode.DataDir = hh.scope.OpenCodeRoot
	c.Pi.SessionsDir = hh.scope.PiRoot
	before, _ := os.ReadFile(hh.scope.path())
	_, lockBefore := os.Stat(filepath.Join(c.RuntimeDir, "observer.lock"))
	for _, prompts := range []bool{false, true} {
		for _, tools := range []bool{false, true} {
			c.Dream.IncludePrompts = prompts
			c.Dream.IncludeToolBodies = tools
			events, err := DreamHistory(context.Background(), c, time.Time{}, now, c.Dream.MaxInputBytes)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(events)
			text := string(raw)
			for _, bad := range []string{testGitHubPAT, "META_FORBIDDEN", "RHIZOME_FORBIDDEN", "PEER_FORBIDDEN", "OLD_FORBIDDEN"} {
				if strings.Contains(text, bad) {
					t.Fatalf("leaked %s", bad)
				}
			}
			for _, marker := range []string{"PROMPT_BODY", "CODEX_PROMPT"} {
				if strings.Contains(text, marker) != prompts {
					t.Errorf("prompt policy %t: %s", prompts, text)
				}
			}
			for _, marker := range []string{"TOOL_INPUT", "TOOL_OUTPUT"} {
				if strings.Contains(text, marker) != tools {
					t.Errorf("tool policy %t: %s", tools, text)
				}
			}
			if !strings.Contains(text, "ASSISTANT_BODY") || !strings.Contains(text, "CODEX_ASSISTANT") {
				t.Fatal("missing assistant history")
			}
		}
	}
	events, err := DreamHistory(context.Background(), c, now, now, c.Dream.MaxInputBytes)
	if err != nil || len(events) != 0 {
		t.Fatal("since bound", err, events)
	}
	events, err = DreamHistory(context.Background(), c, time.Time{}, now, 2)
	if err != nil || len(events) != 0 {
		t.Fatal("budget", err)
	}
	_, lockAfter := os.Stat(filepath.Join(c.RuntimeDir, "observer.lock"))
	if os.IsNotExist(lockBefore) && !os.IsNotExist(lockAfter) {
		t.Fatal("dream created observer lock")
	}
	after, _ := os.ReadFile(hh.scope.path())
	if string(before) != string(after) {
		t.Fatal("catalog changed")
	}
	c.Dream.Enabled = false
	if _, err := DreamHistory(context.Background(), c, time.Time{}, now, 100); err == nil {
		t.Fatal("disabled history accepted")
	}
}

func TestDreamHistoryOpenCodePiAndConfinement(t *testing.T) {
	hh, f, dir := openCodeHarness(t)
	stamp := "2026-11-11T12:00:00Z"
	now, _ := time.Parse(time.RFC3339, "2026-11-12T12:00:00Z")
	f.session("oc-dream", dir, "not sent", "", stamp, stamp)
	f.message("oc-dream", 1, "user", stamp, ocPart{"text": "OC_PROMPT"})
	f.message("oc-dream", 2, "assistant", stamp, ocAssistant(ocText("OC_ANSWER")))
	piDir := filepath.Join(hh.scope.PiRoot, "project")
	if err := os.MkdirAll(piDir, 0700); err != nil {
		t.Fatal(err)
	}
	appendLog(t, filepath.Join(piDir, "pi.jsonl"), piHeader("pi-dream", "/p", stamp)+piMessage("u", stamp, map[string]any{"role": "user", "content": "PI_PROMPT"})+piAssistant("a", stamp, "stop", map[string]any{"type": "text", "text": "PI_ANSWER"}))
	hh.index()
	c := config.Config{RuntimeDir: hh.scope.Runtime, Dream: config.Dream{Enabled: true, IncludePrompts: true, Lookback: "7d", MaxInputBytes: 256 << 10}}
	c.Claude.ProjectsDir = hh.scope.ClaudeRoot
	c.Codex.SessionsDir = hh.scope.CodexRoot
	c.OpenCode.DataDir = hh.scope.OpenCodeRoot
	c.Pi.SessionsDir = hh.scope.PiRoot
	events, err := DreamHistory(context.Background(), c, time.Time{}, now, c.Dream.MaxInputBytes)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(events)
	for _, marker := range []string{"OC_PROMPT", "OC_ANSWER", "PI_PROMPT", "PI_ANSWER"} {
		if !strings.Contains(string(raw), marker) {
			t.Errorf("missing %s: %s", marker, raw)
		}
	}
	// A catalog path replaced with a symlink must not be opened by the dream reader.
	piPath := filepath.Join(piDir, "pi.jsonl")
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.Rename(piPath, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, piPath); err != nil {
		t.Fatal(err)
	}
	if _, err := DreamHistory(context.Background(), c, time.Time{}, now, c.Dream.MaxInputBytes); err == nil {
		t.Fatal("followed a source symlink")
	}
}
