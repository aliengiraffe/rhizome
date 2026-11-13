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
			batch, err := DreamHistory(context.Background(), c, DreamProgress{}, now, c.Dream.MaxInputBytes)
			events := batch.Excerpts
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
	// Project identities are derived only when digest sharing is enabled.
	batch, err := DreamHistory(context.Background(), c, DreamProgress{}, now, c.Dream.MaxInputBytes)
	if err != nil || batch.Projects != nil {
		t.Fatal("projects without [gateway.dream]", err, batch.Projects)
	}
	c.Gateway.Dream = config.DreamSharing{Mode: "review", Lookback: "7d"}
	batch, err = DreamHistory(context.Background(), c, DreamProgress{}, now, c.Dream.MaxInputBytes)
	if err != nil || batch.Projects[DreamKey("claude", "dream-session")] != DigestProjectKey("/p") || batch.Projects[DreamKey("codex", "codex-dream")] != DigestProjectKey("/p") {
		t.Fatal("projects", err, batch.Projects)
	}
	c.Gateway.Dream = config.DreamSharing{}
	batch, err = DreamHistory(context.Background(), c, DreamProgress{Since: now}, now, c.Dream.MaxInputBytes)
	if err != nil || len(batch.Excerpts) != 0 {
		t.Fatal("since bound", err, batch.Excerpts)
	}
	batch, err = DreamHistory(context.Background(), c, DreamProgress{}, now, 2)
	if err != nil || len(batch.Excerpts) != 0 || batch.Partial == "" || batch.Remaining == 0 {
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
	if _, err := DreamHistory(context.Background(), c, DreamProgress{}, now, 100); err == nil {
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
	batch, err := DreamHistory(context.Background(), c, DreamProgress{}, now, c.Dream.MaxInputBytes)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(batch.Excerpts)
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
	if _, err := DreamHistory(context.Background(), c, DreamProgress{}, now, c.Dream.MaxInputBytes); err == nil {
		t.Fatal("followed a source symlink")
	}
}

func TestDreamHistoryResumesFromCursors(t *testing.T) {
	hh := newHistoryHarness(t)
	now, _ := time.Parse(time.RFC3339, "2026-11-12T12:00:00Z")
	appendLog(t, filepath.Join(hh.scope.ClaudeRoot, "older.jsonl"),
		assistantText("older", "/p", "o1", "2026-11-10T10:00:00Z", "OLDER_ONE")+assistantText("older", "/p", "o2", "2026-11-10T10:00:01Z", "OLDER_TWO"))
	appendLog(t, filepath.Join(hh.scope.ClaudeRoot, "newer.jsonl"),
		assistantText("newer", "/p", "n1", "2026-11-11T10:00:00Z", "NEWER_ONE")+assistantText("newer", "/p", "n2", "2026-11-11T10:00:01Z", "NEWER_TWO"))
	hh.index()
	c := config.Config{RuntimeDir: hh.scope.Runtime, Dream: config.Dream{Enabled: true, Lookback: "7d", MaxInputBytes: 256 << 10}}
	c.Claude.ProjectsDir = hh.scope.ClaudeRoot
	c.Codex.SessionsDir = hh.scope.CodexRoot
	c.OpenCode.DataDir = hh.scope.OpenCodeRoot
	c.Pi.SessionsDir = hh.scope.PiRoot
	full, err := DreamHistory(context.Background(), c, DreamProgress{}, now, c.Dream.MaxInputBytes)
	if err != nil || len(full.Excerpts) != 4 || full.Excerpts[0].Text != "OLDER_ONE" || full.Partial != "" || full.Remaining != 0 || len(full.Scanned) != 2 {
		t.Fatalf("oldest-first full scan: %+v %v", full, err)
	}
	// Budget for the older session and one newer event only.
	one, _ := json.Marshal(full.Excerpts[0])
	budget := 2 + 3*(len(one)+1)
	cut, err := DreamHistory(context.Background(), c, DreamProgress{}, now, budget)
	older, newer := DreamKey("claude", "older"), DreamKey("claude", "newer")
	if err != nil || len(cut.Excerpts) != 3 || cut.Partial != newer || cut.Remaining != 1 || len(cut.Scanned) != 1 || cut.Scanned[older].IsZero() {
		t.Fatalf("budget cut: %+v %v", cut, err)
	}
	resume := DreamProgress{Cursors: map[string]time.Time{older: cut.Scanned[older], newer: cut.Excerpts[2].Source.At}}
	rest, err := DreamHistory(context.Background(), c, resume, now, c.Dream.MaxInputBytes)
	if err != nil || len(rest.Excerpts) != 1 || rest.Excerpts[0].Text != "NEWER_TWO" || rest.Remaining != 0 {
		t.Fatalf("resume: %+v %v", rest, err)
	}
	done := DreamProgress{Cursors: map[string]time.Time{older: cut.Scanned[older], newer: rest.Scanned[newer]}}
	if again, err := DreamHistory(context.Background(), c, done, now, c.Dream.MaxInputBytes); err != nil || len(again.Excerpts) != 0 || again.Remaining != 0 {
		t.Fatalf("drained history reread: %+v %v", again, err)
	}
}
