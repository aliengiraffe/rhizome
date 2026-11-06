package channel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func piEntry(typ, id, ts string, fields map[string]any) string {
	r := map[string]any{"type": typ, "id": id, "parentId": nil, "timestamp": ts}
	for k, v := range fields {
		r[k] = v
	}
	b, _ := json.Marshal(r)
	return string(b) + "\n"
}

func piHeader(id, cwd, ts string) string {
	b, _ := json.Marshal(map[string]any{"type": "session", "version": 3, "id": id, "timestamp": ts, "cwd": cwd})
	return string(b) + "\n"
}

func piMessage(id, ts string, msg map[string]any) string {
	return piEntry("message", id, ts, map[string]any{"message": msg})
}

func piAssistant(id, ts, stop string, blocks ...map[string]any) string {
	return piMessage(id, ts, map[string]any{"role": "assistant", "content": blocks, "stopReason": stop})
}

func TestHistoryPiSessions(t *testing.T) {
	hh := newHistoryHarness(t)
	dir := filepath.Join(hh.o.opts.PiRoot, "--work-app--")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	text := func(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
	path := filepath.Join(dir, "2026-10-05T10-00-00-000Z_pi-1.jsonl")
	appendLog(t, path, piHeader("pi-1", "/work/app", "2026-10-05T10:00:00.000Z")+
		piMessage("m0", "2026-10-05T10:00:00.500Z", map[string]any{"role": "system", "content": ""})+
		piMessage("m1", "2026-10-05T10:00:01.000Z", map[string]any{"role": "user", "content": "fix the login"})+
		piAssistant("m2", "2026-10-05T10:00:02.000Z", "toolUse", text("looking"), map[string]any{"type": "thinking", "thinking": "hmm"},
			map[string]any{"type": "toolCall", "id": "t1", "name": "read", "arguments": map[string]any{"path": "a.go"}},
			map[string]any{"type": "toolCall", "id": "t2", "name": "mcp__rhizome__list_sessions", "arguments": map[string]any{}})+
		piMessage("m3", "2026-10-05T10:00:03.000Z", map[string]any{"role": "toolResult", "toolCallId": "t1", "toolName": "read", "content": []map[string]any{text("package a")}, "isError": false})+
		piMessage("m3b", "2026-10-05T10:00:03.500Z", map[string]any{"role": "toolResult", "toolCallId": "t2", "toolName": "mcp__rhizome__list_sessions", "content": []map[string]any{text("[]")}})+
		piAssistant("m4", "2026-10-05T10:00:04.000Z", "stop", text("fixed"))+
		piEntry("session_info", "n1", "2026-10-05T10:00:05.000Z", map[string]any{"name": "  Fix   login "})+
		// A peer delivery is a custom message: the reaction to it is not published.
		piEntry("custom_message", "c1", "2026-10-05T10:01:00.000Z", map[string]any{"customType": "rhizome", "content": "<rhizome-peer-event>x</rhizome-peer-event>", "display": true})+
		piAssistant("m5", "2026-10-05T10:01:01.000Z", "stop", text("reacting to a peer"))+
		piMessage("m6", "2026-10-05T10:02:00.000Z", map[string]any{"role": "user", "content": []map[string]any{text("thanks")}})+
		piAssistant("m7", "2026-10-05T10:02:01.000Z", "stop", text("welcome")))
	// A header without an ID is not a session.
	appendLog(t, filepath.Join(dir, "junk.jsonl"), `{"type":"session","cwd":"/work/app"}`+"\n")
	hh.index()
	for p, f := range hh.o.files {
		if f.agent == "pi" {
			t.Fatalf("Pi log %s was observed passively", p)
		}
	}
	out, herr := hh.search(`{"agent_type":"pi"}`)
	if herr != nil {
		t.Fatal(herr)
	}
	rows := sessionsOf(out)
	if len(rows) != 1 {
		t.Fatalf("rows %v", rows)
	}
	r := rows[0]
	if r["session_id"] != "pi-1" || r["agent_type"] != "pi" || r["project_path"] != "/work/app" || r["session_name"] != "Fix login" || r["name_source"] != "native" ||
		r["started_at"] != "2026-10-05T10:00:00.5Z" || r["last_activity_at"] != "2026-10-05T10:02:01Z" || r["history_availability"] != "available" || r["delivery_capable"] != false {
		t.Fatalf("row %v", r)
	}
	// Local history only: there is no peer-history policy key for Pi yet.
	if _, herr := hh.search(`{"agent_type":"nope"}`); herr == nil {
		t.Fatal("unknown agent accepted")
	}
	read, herr := hh.read(`{"session_id":"pi-1","agent_type":"pi"}`)
	if herr != nil {
		t.Fatal(herr)
	}
	var got []string
	for _, e := range eventsOf(read) {
		txt, _ := e["text"].(string)
		got = append(got, e["kind"].(string)+":"+txt)
	}
	want := []string{"assistant_text:looking", "tool_summary:Tool started: read", "tool_summary:Tool completed: read", "assistant_text:fixed", "turn_completed:", "assistant_text:welcome", "turn_completed:"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("events\n got %v\nwant %v", got, want)
	}
	if cov := read["coverage"].(map[string]any); cov["content_policy"] != "assistant_activity_only" {
		t.Fatalf("coverage %v", cov)
	}
	// The root is confined: a catalog row for another agent's path is never read.
	if e := hh.scope.confine("claude", path); e == nil {
		t.Fatal("Pi log accepted as a Claude source")
	}
}
