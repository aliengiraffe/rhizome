package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"rhizome/internal/localstate"
)

// historyHarness pairs an observer (catalog writer) with a history service
// (catalog reader) over one isolated runtime and log roots.
type historyHarness struct {
	t       *testing.T
	o       *observer
	h       *historyService
	n       *Node
	scope   historyScope
	started bool
}

func newHistoryHarness(t *testing.T) *historyHarness {
	t.Helper()
	o := observerHarness(t)
	o.opts.OpenCodeRoot = filepath.Join(filepath.Dir(o.opts.Runtime), "opencode")
	if err := os.MkdirAll(o.opts.OpenCodeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	scope := historyScope{Runtime: o.opts.Runtime, ClaudeRoot: o.opts.ClaudeRoot, CodexRoot: o.opts.CodexRoot, OpenCodeRoot: o.opts.OpenCodeRoot}
	o.index = newHistoryIndex(scope, func() bool { return o.retry })
	n := newNode(context.Background())
	t.Cleanup(n.close)
	return &historyHarness{t: t, o: o, h: newHistoryService(scope), n: n, scope: scope}
}

// index runs a full inventory plus bounded passes until the queue drains.
func (hh *historyHarness) index() {
	hh.t.Helper()
	hh.o.inventory(!hh.started)
	hh.started = true
	hh.o.index.walkDone()
	for i := 0; i < 64 && len(hh.o.index.queue) > 0; i++ {
		hh.o.index.work(context.Background())
	}
	if len(hh.o.index.queue) > 0 {
		hh.t.Fatal("index queue did not drain")
	}
	hh.o.index.ocNext = time.Time{} // tests do not wait out the refresh debounce
	hh.o.index.work(context.Background())
	hh.o.index.flush()
}
func (hh *historyHarness) search(args string) (map[string]any, *historyError) {
	hh.t.Helper()
	out, err := hh.h.search(context.Background(), hh.n, json.RawMessage(args))
	return roundTrip(hh.t, out), err
}
func (hh *historyHarness) read(args string) (map[string]any, *historyError) {
	hh.t.Helper()
	out, err := hh.h.read(context.Background(), hh.n, json.RawMessage(args))
	return roundTrip(hh.t, out), err
}
func roundTrip(t *testing.T, v any) map[string]any {
	t.Helper()
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
func sessionsOf(m map[string]any) []map[string]any {
	var out []map[string]any
	for _, s := range m["sessions"].([]any) {
		out = append(out, s.(map[string]any))
	}
	return out
}
func eventsOf(m map[string]any) []map[string]any {
	var out []map[string]any
	for _, s := range m["events"].([]any) {
		out = append(out, s.(map[string]any))
	}
	return out
}
func cursorOf(m map[string]any) string {
	if c, ok := m["next_cursor"].(string); ok {
		return c
	}
	return ""
}

type claudeLine struct {
	typ, uuid, session, cwd, ts string
	content                     any
	stop                        string
	meta                        bool
	sidechain                   bool
}

func (l claudeLine) String() string {
	r := map[string]any{"type": l.typ, "uuid": l.uuid, "sessionId": l.session, "cwd": l.cwd, "isSidechain": l.sidechain}
	if l.ts != "" {
		r["timestamp"] = l.ts
	}
	if l.meta {
		r["isMeta"] = true
	}
	msg := map[string]any{"role": l.typ, "content": l.content}
	if l.stop != "" {
		msg["stop_reason"] = l.stop
	}
	r["message"] = msg
	b, _ := json.Marshal(r)
	return string(b) + "\n"
}
func assistantText(session, cwd, uuid, ts, text string) string {
	return claudeLine{typ: "assistant", uuid: uuid, session: session, cwd: cwd, ts: ts, content: []map[string]any{{"type": "text", "text": text}}}.String()
}
func userText(session, cwd, uuid, ts, text string) string {
	return claudeLine{typ: "user", uuid: uuid, session: session, cwd: cwd, ts: ts, content: text}.String()
}
func codexMeta(id, cwd, ts string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":%q,"cwd":%q,"source":"cli"}}`+"\n", ts, id, cwd)
}
func codexAssistant(id, ts, text string) string {
	b, _ := json.Marshal(map[string]any{"timestamp": ts, "type": "response_item", "payload": map[string]any{"type": "message", "id": id, "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": text}}}})
	return string(b) + "\n"
}
func codexUser(id, ts, text string) string {
	b, _ := json.Marshal(map[string]any{"timestamp": ts, "type": "response_item", "payload": map[string]any{"type": "message", "id": id, "role": "user", "content": []map[string]string{{"type": "input_text", "text": text}}}})
	return string(b) + "\n"
}

func TestHistoryCatalogIndexesPreexistingLogsWithoutPublishing(t *testing.T) {
	hh := newHistoryHarness(t)
	proj := filepath.Join(hh.o.opts.ClaudeRoot, "-work-app")
	if err := os.MkdirAll(proj, 0700); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(proj, "s-claude.jsonl")
	appendLog(t, claude, userText("s-claude", "/work/app", "u1", "2026-10-01T10:00:00Z", "hello")+
		`{"type":"ai-title","aiTitle":"  Auto   title ","sessionId":"s-claude"}`+"\n"+
		assistantText("s-claude", "/work/app", "a1", "2026-10-01T10:00:05Z", "first answer")+
		`{"type":"custom-title","customTitle":"Observer rewrite","sessionId":"s-claude"}`+"\n"+
		assistantText("s-claude", "/work/app", "a2", "2026-10-02T09:00:00Z", "second answer"))
	// Child transcripts never become sessions.
	appendLog(t, filepath.Join(proj, "agent-child.jsonl"), claudeLine{typ: "assistant", uuid: "c1", session: "child", cwd: "/work/app", ts: "2026-10-01T10:00:00Z", sidechain: true, content: "x"}.String())
	codexDir := filepath.Join(hh.o.opts.CodexRoot, "2026", "10", "03")
	if err := os.MkdirAll(codexDir, 0700); err != nil {
		t.Fatal(err)
	}
	appendLog(t, filepath.Join(codexDir, "rollout-x.jsonl"), codexMeta("s-codex", "/work/other", "2026-10-03T08:00:00Z")+codexUser("u1", "2026-10-03T08:00:01Z", "do it")+codexAssistant("m1", "2026-10-03T08:00:02Z", "done"))
	// No timestamps at all: sorted last, excluded by date filters.
	appendLog(t, filepath.Join(proj, "s-untimed.jsonl"), claudeLine{typ: "assistant", uuid: "n1", session: "s-untimed", cwd: "/work/app", content: "untimed"}.String())
	// No session identity: not a session.
	appendLog(t, filepath.Join(proj, "junk.jsonl"), `{"type":"summary","summary":"x"}`+"\n")
	hh.index()
	for path, f := range hh.o.files {
		if f.n != nil {
			t.Fatalf("indexing advertised %s as a publisher", path)
		}
	}
	lock, err := localstate.Lock(filepath.Join(hh.scope.Runtime, "observer.lock"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	out, herr := hh.search(`{}`)
	if herr != nil {
		t.Fatal(herr)
	}
	rows := sessionsOf(out)
	if len(rows) != 3 {
		t.Fatalf("rows %v", rows)
	}
	if rows[0]["session_id"] != "s-codex" || rows[1]["session_id"] != "s-claude" || rows[2]["session_id"] != "s-untimed" {
		t.Fatalf("order %v %v %v", rows[0]["session_id"], rows[1]["session_id"], rows[2]["session_id"])
	}
	c := rows[1]
	if c["agent_type"] != "claude" || c["project_path"] != "/work/app" || c["display_name"] != "app" || c["session_name"] != "Observer rewrite" || c["name_source"] != "native" || c["started_at"] != "2026-10-01T10:00:00Z" || c["last_activity_at"] != "2026-10-02T09:00:00Z" || c["history_availability"] != "available" || c["live_status"] != "not_discovered" || c["delivery_capable"] != false {
		t.Fatalf("claude row %v", c)
	}
	if x := rows[0]; x["session_name"] != "other · codex · s-codex" || x["name_source"] != "fallback" || x["project_path"] != "/work/other" {
		t.Fatalf("codex row %v", x)
	}
	if u := rows[2]; u["last_activity_at"] != nil || u["started_at"] != nil {
		t.Fatalf("untimed row %v", u)
	}
	cat := out["catalog"].(map[string]any)
	if cat["state"] != "ready" || cat["service_running"] != true || cat["coverage_complete"] != true || cat["indexed_at"] == nil {
		t.Fatalf("catalog %v", cat)
	}
	// Explicit overrides win and are looked up by exact ID.
	if err := storeOverride(hh.scope.Runtime, "s-codex", "Renamed thread"); err != nil {
		t.Fatal(err)
	}
	out, _ = hh.search(`{"name_contains":"renamed THREAD"}`)
	if rows := sessionsOf(out); len(rows) != 1 || rows[0]["name_source"] != "explicit" || rows[0]["session_name"] != "Renamed thread" {
		t.Fatalf("explicit %v", rows)
	}
	// Metadata persists across an observer restart without rescanning.
	again := newHistoryIndex(hh.scope, nil)
	if len(again.files) != len(hh.o.index.files) || again.files[claude].Indexed == 0 {
		t.Fatal("catalog not reloaded")
	}
}

func TestHistorySearchFiltersOrderingAndPagination(t *testing.T) {
	hh := newHistoryHarness(t)
	proj := filepath.Join(hh.o.opts.ClaudeRoot, "p")
	if err := os.MkdirAll(proj, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("s%d", i)
		appendLog(t, filepath.Join(proj, id+".jsonl"), assistantText(id, "/p", "a", fmt.Sprintf("2026-10-0%dT00:00:00Z", i+1), "x"))
	}
	appendLog(t, filepath.Join(proj, "tie.jsonl"), assistantText("a-tie", "/p", "a", "2026-10-03T00:00:00Z", "x"))
	hh.index()
	lock, _ := localstate.Lock(filepath.Join(hh.scope.Runtime, "observer.lock"), false)
	defer lock.Close()
	ids := func(m map[string]any) []string {
		var out []string
		for _, r := range sessionsOf(m) {
			out = append(out, r["session_id"].(string))
		}
		return out
	}
	for _, tc := range []struct {
		name, args string
		want       []string
	}{
		{"all", `{}`, []string{"s4", "s3", "a-tie", "s2", "s1", "s0"}}, // tie on 10-03 breaks by ID
		{"exact-id", `{"session_id":"s2"}`, []string{"s2"}},
		{"no-prefix", `{"session_id":"s"}`, nil},
		{"agent", `{"agent_type":"codex"}`, nil},
		{"project", `{"project_path":"/p/"}`, []string{"s4", "s3", "a-tie", "s2", "s1", "s0"}},
		{"project-subtree-not-matched", `{"project_path":"/"}`, nil},
		{"after-inclusive", `{"updated_after":"2026-10-03T00:00:00Z"}`, []string{"s4", "s3", "a-tie", "s2"}},
		{"before-exclusive", `{"updated_before":"2026-10-03T00:00:00Z"}`, []string{"s1", "s0"}},
		{"window", `{"updated_after":"2026-10-02T00:00:00+00:00","updated_before":"2026-10-04T00:00:00Z"}`, []string{"a-tie", "s2", "s1"}},
		{"and", `{"session_id":"s3","updated_before":"2026-10-04T00:00:00Z"}`, nil},
		{"name", `{"name_contains":"S3"}`, []string{"s3"}},
	} {
		out, herr := hh.search(tc.args)
		if herr != nil {
			t.Fatalf("%s: %v", tc.name, herr)
		}
		if got := ids(out); strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
	for _, args := range []string{`{"limit":0}`, `{"limit":101}`, `{"limit":1.5}`, `{"limit":"5"}`, `{"agent_type":"gpt"}`, `{"project_path":"relative"}`, `{"session_id":""}`, `{"name_contains":""}`, `{"updated_after":"yesterday"}`, `{"updated_after":"2026-10-03T00:00:00Z","updated_before":"2026-10-03T00:00:00Z"}`, `{"unknown":1}`, `[]`, `{"name_contains":"` + strings.Repeat("x", 257) + `"}`} {
		if _, herr := hh.search(args); herr == nil || herr.Code != "INVALID_ARGUMENT" {
			t.Fatalf("%s accepted: %v", args, herr)
		}
	}
	// Deterministic pagination over a fixed snapshot.
	page1, _ := hh.search(`{"limit":2}`)
	if got := ids(page1); strings.Join(got, ",") != "s4,s3" || cursorOf(page1) == "" {
		t.Fatalf("page1 %v", got)
	}
	// A catalog update between pages neither duplicates nor skips rows.
	appendLog(t, filepath.Join(proj, "s0.jsonl"), assistantText("s0", "/p", "b", "2026-10-09T00:00:00Z", "newest"))
	hh.o.visit(filepath.Join(proj, "s0.jsonl"), false)
	hh.index()
	page2, herr := hh.search(`{"limit":2,"cursor":"` + cursorOf(page1) + `"}`)
	if herr != nil || strings.Join(ids(page2), ",") != "a-tie,s2" || cursorOf(page2) == "" {
		t.Fatalf("page2 %v %v", page2, herr)
	}
	// Cursors are single-use and forward-only while the snapshot lives.
	if _, herr := hh.search(`{"limit":2,"cursor":"` + cursorOf(page1) + `"}`); herr == nil || herr.Code != "INVALID_CURSOR" {
		t.Fatalf("replayed cursor: %v", herr)
	}
	page3, herr := hh.search(`{"limit":2,"cursor":"` + cursorOf(page2) + `"}`)
	if herr != nil || strings.Join(ids(page3), ",") != "s1,s0" || cursorOf(page3) != "" {
		t.Fatalf("page3 %v %v", page3, herr)
	}
	if got := sessionsOf(page3)[1]["last_activity_at"]; got != "2026-10-09T00:00:00Z" {
		t.Fatalf("stale metadata on page 3: %v", got)
	}
	// The exhausted snapshot is released; its cursors expire explicitly.
	if _, herr := hh.search(`{"limit":2,"cursor":"` + cursorOf(page2) + `"}`); herr == nil || herr.Code != "CURSOR_EXPIRED" {
		t.Fatalf("released snapshot: %v", herr)
	}
	fresh, _ := hh.search(`{"limit":4}`)
	for _, tc := range []struct{ args, code string }{
		{`{"limit":3,"cursor":"` + cursorOf(fresh) + `"}`, "INVALID_CURSOR"},
		{`{"limit":4,"agent_type":"claude","cursor":"` + cursorOf(fresh) + `"}`, "INVALID_CURSOR"},
		{`{"limit":4,"cursor":"` + cursorOf(fresh)[:len(cursorOf(fresh))-2] + `zz"}`, "INVALID_CURSOR"},
		{`{"limit":4,"cursor":"garbage"}`, "INVALID_CURSOR"},
		{`{"limit":4,"cursor":"` + newSnapshotStore().sign(cursorPayload{Kind: "search", Snapshot: "x", Page: 1, Query: "q"}) + `"}`, "CURSOR_EXPIRED"},
		{`{"session_id":"s1","limit":1,"cursor":"` + cursorOf(fresh) + `"}`, "INVALID_CURSOR"},
	} {
		if _, herr := hh.search(tc.args); herr == nil || herr.Code != tc.code {
			t.Fatalf("%s: %v", tc.args, herr)
		}
	}
	// Eviction of the retained snapshot expires the cursor explicitly.
	hh.h.snaps.mu.Lock()
	hh.h.snaps.items = map[string]*snapshot{}
	hh.h.snaps.mu.Unlock()
	if _, herr := hh.search(`{"limit":4,"cursor":"` + cursorOf(fresh) + `"}`); herr == nil || herr.Code != "CURSOR_EXPIRED" {
		t.Fatalf("evicted: %v", herr)
	}
}

func TestHistoryCatalogStates(t *testing.T) {
	hh := newHistoryHarness(t)
	if _, herr := hh.search(`{}`); herr == nil || herr.Code != "CATALOG_UNAVAILABLE" || !strings.Contains(herr.Message, "rhizome service") {
		t.Fatalf("missing catalog: %v", herr)
	}
	if _, herr := hh.read(`{"session_id":"x","agent_type":"claude"}`); herr == nil || herr.Code != "CATALOG_UNAVAILABLE" {
		t.Fatalf("missing catalog read: %v", herr)
	}
	appendLog(t, filepath.Join(hh.o.opts.ClaudeRoot, "s.jsonl"), assistantText("s", "/p", "a", "2026-10-01T00:00:00Z", "x"))
	hh.index()
	out, herr := hh.search(`{}`)
	if herr != nil {
		t.Fatal(herr)
	}
	cat := out["catalog"].(map[string]any)
	if cat["state"] != "stale" || cat["service_running"] != false || cat["coverage_complete"] != false || len(sessionsOf(out)) != 1 {
		t.Fatalf("stopped service: %v", cat)
	}
	if _, herr := hh.read(`{"session_id":"nope","agent_type":"claude"}`); herr == nil || herr.Code != "SESSION_NOT_FOUND" || herr.Extra["catalog"] == nil {
		t.Fatalf("not found: %v", herr)
	}
	// Indexing in progress is visible while the service runs.
	lock, _ := localstate.Lock(filepath.Join(hh.scope.Runtime, "observer.lock"), false)
	hh.o.index.queue = append(hh.o.index.queue, "pending")
	hh.o.index.dirty = true
	hh.o.index.flush()
	hh.o.index.queue = nil
	out, _ = hh.search(`{}`)
	if cat := out["catalog"].(map[string]any); cat["state"] != "indexing" || cat["coverage_complete"] != false {
		t.Fatalf("indexing: %v", cat)
	}
	hh.o.retry = true
	hh.o.index.dirty = true
	hh.o.index.flush()
	out, _ = hh.search(`{}`)
	if cat := out["catalog"].(map[string]any); cat["state"] != "degraded" || cat["coverage_complete"] != false {
		t.Fatalf("degraded: %v", cat)
	}
	lock.Close()
	path := hh.scope.path()
	raw, _ := os.ReadFile(path)
	for _, tc := range []struct{ name, body, code string }{
		{"corrupt", "{not json", "CATALOG_UNAVAILABLE"},
		{"newer", strings.Replace(string(raw), `"version":1`, `"version":99`, 1), "CATALOG_INCOMPATIBLE"},
		{"foreign-scope", strings.Replace(string(raw), hh.scope.hash(), strings.Repeat("0", 64), 1), "CATALOG_INCOMPATIBLE"},
	} {
		if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, herr := hh.search(`{}`); herr == nil || herr.Code != tc.code {
			t.Fatalf("%s: %v", tc.name, herr)
		}
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, herr := hh.search(`{}`); herr == nil || herr.Code != "CATALOG_UNAVAILABLE" {
		t.Fatalf("world-readable catalog: %v", herr)
	}
	// A foreign-scope catalog is rebuilt by a fresh observer, never served.
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), hh.scope.hash(), strings.Repeat("0", 64), 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if len(newHistoryIndex(hh.scope, nil).files) != 0 {
		t.Fatal("foreign catalog adopted")
	}
}

func TestHistoryReadContentPolicyAndPagination(t *testing.T) {
	hh := newHistoryHarness(t)
	path := filepath.Join(hh.o.opts.ClaudeRoot, "s.jsonl")
	tool := func(uuid, id, name string) string {
		return claudeLine{typ: "assistant", uuid: uuid, session: "s", cwd: "/p", ts: "2026-10-01T00:00:03Z", content: []map[string]any{{"type": "tool_use", "id": id, "name": name}}}.String()
	}
	toolResult := func(uuid, id string, isErr bool) string {
		return claudeLine{typ: "user", uuid: uuid, session: "s", cwd: "/p", ts: "2026-10-01T00:00:04Z", content: []map[string]any{{"type": "tool_result", "tool_use_id": id, "is_error": isErr, "content": "secret output"}}}.String()
	}
	lines := []string{
		userText("s", "/p", "u1", "2026-10-01T00:00:00Z", "my prompt"),
		claudeLine{typ: "assistant", uuid: "th", session: "s", cwd: "/p", ts: "2026-10-01T00:00:01Z", content: []map[string]any{{"type": "thinking", "thinking": "secret"}}}.String(),
		assistantText("s", "/p", "a1", "2026-10-01T00:00:02Z", "answer one"),
		assistantText("s", "/p", "a1", "2026-10-01T00:00:02Z", "answer one"),  // duplicate uuid
		assistantText("s", "/p", "a1b", "2026-10-01T00:00:02Z", "answer one"), // distinct record, same text
		tool("t1", "call-1", "Read"),
		tool("t2", "call-2", "mcp__rhizome__read_session_history"),
		toolResult("r2", "call-2", false),
		toolResult("r1", "call-1", true),
		claudeLine{typ: "assistant", uuid: "end", session: "s", cwd: "/p", ts: "2026-10-01T00:00:05Z", content: []map[string]any{{"type": "text", "text": "wrapping up"}}, stop: "end_turn"}.String(),
		claudeLine{typ: "user", uuid: "peer", session: "s", cwd: "/p", ts: "2026-10-01T00:00:06Z", meta: true, content: `<channel source="rhizome" />peer envelope`}.String(),
		assistantText("s", "/p", "react", "2026-10-01T00:00:07Z", "reaction to peer"),
		assistantText("s", "/p", "react2", "2026-10-01T00:00:08Z", "second reaction"),
		userText("s", "/p", "u2", "2026-10-01T00:00:09Z", "human again"),
		assistantText("other", "/p", "foreign", "2026-10-01T00:00:10Z", "foreign session record"),
		claudeLine{typ: "assistant", uuid: "side", session: "s", cwd: "/p", ts: "2026-10-01T00:00:10Z", sidechain: true, content: "subagent"}.String(),
		assistantText("s", "/p", "a3", "2026-10-01T00:00:11Z", "final"),
	}
	appendLog(t, path, strings.Join(lines, ""))
	hh.index()
	want := []string{"assistant_text:answer one", "assistant_text:answer one", "tool_summary:Tool started: Read", "tool_summary:Tool failed: Read", "assistant_text:wrapping up", "turn_completed:", "assistant_text:final"}
	var got []string
	var ids []string
	cursor := ""
	var snapID any
	for page := 0; page < 10; page++ {
		args := `{"session_id":"s","agent_type":"claude","limit":1`
		if cursor != "" {
			args += `,"cursor":"` + cursor + `"`
		}
		out, herr := hh.read(args + "}")
		if herr != nil {
			t.Fatalf("page %d: %v", page, herr)
		}
		if snapID == nil {
			snapID = out["snapshot"].(map[string]any)["id"]
		} else if out["snapshot"].(map[string]any)["id"] != snapID {
			t.Fatal("snapshot changed across pages")
		}
		for _, ev := range eventsOf(out) {
			text, _ := ev["text"].(string)
			got = append(got, ev["kind"].(string)+":"+text)
			ids = append(ids, ev["event_id"].(string))
			if ev["timestamp"] == nil {
				t.Fatalf("missing timestamp %v", ev)
			}
		}
		if cov := out["coverage"].(map[string]any); cov["state"] != "complete" || cov["content_policy"] != "assistant_activity_only" {
			t.Fatalf("coverage %v", cov)
		}
		if out["session"].(map[string]any)["session_id"] != "s" {
			t.Fatal("session metadata missing")
		}
		cursor = cursorOf(out)
		if cursor == "" {
			break
		}
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("events:\n got %v\nwant %v", got, want)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate event id %s", id)
		}
		seen[id] = true
	}
	// A second snapshot of the unchanged file yields identical IDs in one page.
	out, _ := hh.read(`{"session_id":"s","agent_type":"claude","limit":200}`)
	var again []string
	for _, ev := range eventsOf(out) {
		again = append(again, ev["event_id"].(string))
	}
	if strings.Join(again, ",") != strings.Join(ids, ",") || cursorOf(out) != "" {
		t.Fatalf("ids differ: %v vs %v", again, ids)
	}
	for _, forbidden := range []string{"my prompt", "secret", "peer envelope", "reaction", "foreign", "subagent", "human again", "read_session_history"} {
		b, _ := json.Marshal(out)
		if strings.Contains(string(b), forbidden) {
			t.Fatalf("leaked %q", forbidden)
		}
	}
	// A user-only session is valid, empty and complete.
	appendLog(t, filepath.Join(hh.o.opts.ClaudeRoot, "u.jsonl"), userText("u", "/p", "x", "2026-10-01T00:00:00Z", "only me"))
	hh.index()
	out, herr := hh.read(`{"session_id":"u","agent_type":"claude"}`)
	if herr != nil || len(eventsOf(out)) != 0 || cursorOf(out) != "" || out["coverage"].(map[string]any)["state"] != "complete" {
		t.Fatalf("user-only %v %v", out, herr)
	}
	// Codex: mirrored event_msg text, reasoning, Rhizome calls and peer reactions are excluded.
	codex := filepath.Join(hh.o.opts.CodexRoot, "r.jsonl")
	appendLog(t, codex, codexMeta("c", "/p", "2026-10-02T00:00:00Z")+
		codexUser("cu1", "2026-10-02T00:00:01Z", "go")+
		codexAssistant("ca1", "2026-10-02T00:00:02Z", "codex answer")+
		`{"timestamp":"2026-10-02T00:00:02Z","type":"event_msg","payload":{"type":"agent_message","message":"codex answer"}}`+"\n"+
		`{"timestamp":"2026-10-02T00:00:03Z","type":"response_item","payload":{"type":"reasoning","id":"rr","text":"hidden"}}`+"\n"+
		`{"timestamp":"2026-10-02T00:00:03Z","type":"response_item","payload":{"type":"function_call","id":"f1","call_id":"k1","name":"shell","arguments":"rm -rf"}}`+"\n"+
		`{"timestamp":"2026-10-02T00:00:04Z","type":"response_item","payload":{"type":"function_call_output","call_id":"k1","output":"tool output"}}`+"\n"+
		`{"timestamp":"2026-10-02T00:00:04Z","type":"response_item","payload":{"type":"function_call","id":"f2","call_id":"k2","name":"mcp__rhizome__search_sessions"}}`+"\n"+
		`{"timestamp":"2026-10-02T00:00:05Z","type":"response_item","payload":{"type":"function_call_output","call_id":"k2","output":"history payload"}}`+"\n"+
		`{"timestamp":"2026-10-02T00:00:06Z","type":"event_msg","payload":{"type":"task_complete"}}`+"\n"+
		codexUser("cu2", "2026-10-02T00:00:07Z", "<rhizome-peer-event>peer</rhizome-peer-event>")+
		codexAssistant("ca2", "2026-10-02T00:00:08Z", "reacting"))
	hh.index()
	out, herr = hh.read(`{"session_id":"c","agent_type":"codex"}`)
	if herr != nil {
		t.Fatal(herr)
	}
	var cg []string
	for _, ev := range eventsOf(out) {
		text, _ := ev["text"].(string)
		cg = append(cg, ev["kind"].(string)+":"+text)
	}
	if strings.Join(cg, "|") != "assistant_text:codex answer|tool_summary:Tool started: shell|tool_summary:Tool completed: shell|turn_completed:" {
		t.Fatalf("codex events %v", cg)
	}
	b, _ := json.Marshal(out)
	for _, forbidden := range []string{"hidden", "rm -rf", "tool output", "history payload", "reacting"} {
		if strings.Contains(string(b), forbidden) {
			t.Fatalf("codex leaked %q", forbidden)
		}
	}
	for _, args := range []string{`{}`, `{"session_id":"s"}`, `{"agent_type":"claude"}`, `{"session_id":"s","agent_type":"claude","limit":201}`, `{"session_id":"s","agent_type":"claude","limit":0}`, `{"session_id":"s","agent_type":"gpt"}`, `{"session_id":"s","agent_type":"claude","path":"/etc/passwd"}`, `{"session_id":"","agent_type":"claude"}`} {
		if _, herr := hh.read(args); herr == nil || herr.Code != "INVALID_ARGUMENT" {
			t.Fatalf("%s accepted: %v", args, herr)
		}
	}
}

func TestHistoryReadSourceChangesAndBudgets(t *testing.T) {
	hh := newHistoryHarness(t)
	path := filepath.Join(hh.o.opts.ClaudeRoot, "s.jsonl")
	var lines []string
	for i := 0; i < 6; i++ {
		lines = append(lines, assistantText("s", "/p", fmt.Sprintf("a%d", i), "2026-10-01T00:00:00Z", fmt.Sprintf("answer %d", i)))
	}
	appendLog(t, path, strings.Join(lines, ""))
	hh.index()
	first, herr := hh.read(`{"session_id":"s","agent_type":"claude","limit":2}`)
	if herr != nil || len(eventsOf(first)) != 2 || cursorOf(first) == "" {
		t.Fatalf("first %v %v", first, herr)
	}
	// Growth does not invalidate the fixed EOF snapshot; the append is not folded in.
	appendLog(t, path, assistantText("s", "/p", "late", "2026-10-01T00:00:01Z", "appended later"))
	second, herr := hh.read(`{"session_id":"s","agent_type":"claude","limit":2,"cursor":"` + cursorOf(first) + `"}`)
	if herr != nil || len(eventsOf(second)) != 2 {
		t.Fatalf("second %v %v", second, herr)
	}
	third, herr := hh.read(`{"session_id":"s","agent_type":"claude","limit":2,"cursor":"` + cursorOf(second) + `"}`)
	if herr != nil || len(eventsOf(third)) != 2 || cursorOf(third) != "" {
		t.Fatalf("third %v %v", third, herr)
	}
	b, _ := json.Marshal(third)
	if strings.Contains(string(b), "appended later") {
		t.Fatal("append folded into an existing snapshot")
	}
	// Mismatched target/limit cursors fail before any read.
	fresh, _ := hh.read(`{"session_id":"s","agent_type":"claude","limit":2}`)
	for _, args := range []string{`{"session_id":"s","agent_type":"claude","limit":3,"cursor":"` + cursorOf(fresh) + `"}`, `{"session_id":"s","agent_type":"codex","limit":2,"cursor":"` + cursorOf(fresh) + `"}`} {
		if _, herr := hh.read(args); herr == nil || herr.Code != "INVALID_CURSOR" {
			t.Fatalf("%s: %v", args, herr)
		}
	}
	// Truncation between pages is reported, never spliced.
	if err := os.WriteFile(path, []byte(lines[0]), 0600); err != nil {
		t.Fatal(err)
	}
	if _, herr := hh.read(`{"session_id":"s","agent_type":"claude","limit":2,"cursor":"` + cursorOf(fresh) + `"}`); herr == nil || herr.Code != "SOURCE_CHANGED" {
		t.Fatalf("truncated: %v", herr)
	}
	// Replacement by rename (new inode) between pages.
	appendLog(t, path, strings.Join(lines[1:], ""))
	fresh, _ = hh.read(`{"session_id":"s","agent_type":"claude","limit":2}`)
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "")), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if _, herr := hh.read(`{"session_id":"s","agent_type":"claude","limit":2,"cursor":"` + cursorOf(fresh) + `"}`); herr == nil || herr.Code != "SOURCE_CHANGED" {
		t.Fatalf("replaced: %v", herr)
	}
	// A symlink in place of the log is never followed.
	fresh, _ = hh.read(`{"session_id":"s","agent_type":"claude","limit":2}`)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	appendLog(t, outside, assistantText("s", "/p", "evil", "2026-10-01T00:00:00Z", "outside root"))
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, herr := hh.read(`{"session_id":"s","agent_type":"claude","limit":2,"cursor":"` + cursorOf(fresh) + `"}`); herr == nil || herr.Code != "HISTORY_UNAVAILABLE" || herr.Extra["reason"] != "source_unsupported" {
		t.Fatalf("symlink: %v", herr)
	}
	// Deletion detected by the observer tombstones the session.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	hh.o.index.removed(path)
	hh.o.index.flush()
	if _, herr := hh.read(`{"session_id":"s","agent_type":"claude"}`); herr == nil || herr.Code != "HISTORY_UNAVAILABLE" || herr.Extra["reason"] != "source_missing" {
		t.Fatalf("missing: %v", herr)
	}
	out, _ := hh.search(`{"session_id":"s"}`)
	if rows := sessionsOf(out); len(rows) != 1 || rows[0]["history_availability"] != "missing" {
		t.Fatalf("tombstone %v", rows)
	}
	// Duplicate identities stay ambiguous.
	appendLog(t, filepath.Join(hh.o.opts.ClaudeRoot, "d1.jsonl"), assistantText("dup", "/p", "a", "2026-10-01T00:00:00Z", "one"))
	appendLog(t, filepath.Join(hh.o.opts.ClaudeRoot, "d2.jsonl"), assistantText("dup", "/p", "a", "2026-10-01T00:00:00Z", "two"))
	hh.index()
	if _, herr := hh.read(`{"session_id":"dup","agent_type":"claude"}`); herr == nil || herr.Code != "SESSION_AMBIGUOUS" {
		t.Fatalf("ambiguous: %v", herr)
	}
	out, _ = hh.search(`{"session_id":"dup"}`)
	if rows := sessionsOf(out); len(rows) != 1 || rows[0]["history_availability"] != "ambiguous" {
		t.Fatalf("ambiguous row %v", rows)
	}
	// Budgets: oversized records, incomplete tails and events beyond the response budget.
	big := filepath.Join(hh.o.opts.ClaudeRoot, "big.jsonl")
	huge := strings.Repeat("h", maxHistoryResponse) // one event larger than the whole response
	large := strings.Repeat("l", 400*1024)          // three of these exceed one page
	appendLog(t, big, assistantText("big", "/p", "x0", "2026-10-01T00:00:00Z", "before")+
		assistantText("big", "/p", "x1", "2026-10-01T00:00:00Z", huge)+
		strings.Repeat("o", maxRecord+10)+"\n"+ // oversized record
		assistantText("big", "/p", "x2", "2026-10-01T00:00:00Z", large)+
		assistantText("big", "/p", "x3", "2026-10-01T00:00:00Z", large)+
		assistantText("big", "/p", "x4", "2026-10-01T00:00:00Z", large)+
		assistantText("big", "/p", "x5", "2026-10-01T00:00:00Z", "after")+
		`{"type":"assistant","uuid":"tail","sessionId":"big","message":{"content":"incomplete`)
	hh.index()
	var texts []string
	cursor := ""
	var gaps map[string]any
	for page := 0; page < 10; page++ {
		args := `{"session_id":"big","agent_type":"claude","limit":10`
		if cursor != "" {
			args += `,"cursor":"` + cursor + `"`
		}
		out, herr := hh.read(args + "}")
		if herr != nil {
			t.Fatalf("budget page %d: %v", page, herr)
		}
		raw, _ := json.Marshal(out)
		if len(raw) > maxHistoryResponse {
			t.Fatalf("response %d bytes exceeds budget", len(raw))
		}
		for _, ev := range eventsOf(out) {
			text := ev["text"].(string)
			texts = append(texts, text[:min(6, len(text))])
		}
		gaps = out["coverage"].(map[string]any)
		cursor = cursorOf(out)
		if cursor == "" {
			break
		}
	}
	if strings.Join(texts, ",") != "before,llllll,llllll,llllll,after" {
		t.Fatalf("budget events %v", texts)
	}
	if gaps["state"] != "partial" {
		t.Fatalf("coverage %v", gaps)
	}
	codes := map[string]bool{}
	for _, g := range gaps["gaps"].([]any) {
		codes[g.(map[string]any)["code"].(string)] = true
	}
	if !codes["event_too_large"] || !codes["oversized_record"] || !codes["incomplete_tail"] {
		t.Fatalf("gap codes %v", codes)
	}
	out, _ = hh.search(`{"session_id":"big"}`)
	if rows := sessionsOf(out); rows[0]["history_availability"] != "partial" {
		t.Fatalf("partial availability %v", rows)
	}
}

func TestHistoryLiveStatusJoin(t *testing.T) {
	hh := newHistoryHarness(t)
	dir := t.TempDir()
	for _, id := range []string{"self", "peer", "watched", "cold"} {
		appendLog(t, filepath.Join(hh.o.opts.ClaudeRoot, id+".jsonl"), assistantText(id, dir, "a", "2026-10-01T00:00:00Z", "x"))
	}
	hh.index()
	a, b := pair(t)
	hh.n = a
	a.mu.Lock()
	a.identity.SessionId = "self"
	a.mu.Unlock()
	b.mu.Lock()
	b.identity.SessionId = "peer"
	b.mu.Unlock()
	_ = b.members.UpdateNode(time.Second)
	observed := node(t)
	if err := observed.start(Options{Runtime: a.runtime}); err != nil {
		t.Fatal(err)
	}
	observed.mu.Lock()
	observed.identity.SessionId = "watched"
	observed.identity.ObservationOnly = true
	observed.mu.Unlock()
	_ = observed.members.UpdateNode(time.Second)
	eventually(t, func() bool {
		live := liveLookup(a)
		return live.info["peer"].direct == 1 && live.info["watched"].observed == 1
	})
	out, herr := hh.search(`{}`)
	if herr != nil {
		t.Fatal(herr)
	}
	got := map[string]string{}
	for _, r := range sessionsOf(out) {
		got[r["session_id"].(string)] = fmt.Sprintf("%v/%v", r["live_status"], r["delivery_capable"])
	}
	want := map[string]string{"self": "live/false", "peer": "live/true", "watched": "recent/false", "cold": "not_discovered/false"}
	for id, w := range want {
		if got[id] != w {
			t.Fatalf("%s: got %s want %s (%v)", id, got[id], w, got)
		}
	}
	// --debug keeps the underlying distinction in a parenthesised substatus.
	a.debug = true
	if out, herr = hh.search(`{}`); herr != nil {
		t.Fatal(herr)
	}
	for _, r := range sessionsOf(out) {
		got[r["session_id"].(string)] = fmt.Sprintf("%v/%v", r["live_status"], r["delivery_capable"])
	}
	a.debug = false
	want = map[string]string{"self": "live (direct, self)/false", "peer": "live (direct)/true", "watched": "recent (observer)/false", "cold": "not_discovered/false"}
	for id, w := range want {
		if got[id] != w {
			t.Fatalf("debug %s: got %s want %s (%v)", id, got[id], w, got)
		}
	}
	// Reading history leaves live state untouched.
	before := len(a.history)
	if _, herr := hh.read(`{"session_id":"peer","agent_type":"claude"}`); herr != nil {
		t.Fatal(herr)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.history) != before || len(a.feeds) != 0 || len(a.incoming) != 0 {
		t.Fatal("history read changed live state")
	}
}

func TestHistoryToolsRegisteredInBothModes(t *testing.T) {
	hh := newHistoryHarness(t)
	n := hh.n
	if err := n.start(Options{Runtime: filepath.Join(t.TempDir(), "runtime")}); err != nil {
		t.Fatal(err)
	}
	n.archive = hh.h
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := n.mcpServer().Connect(ctx, &channelTransport{base: st}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 13 {
		t.Fatalf("tools %v %v", tools, err)
	}
	schemas := map[string]*jsonschema.Schema{}
	for _, tool := range tools.Tools {
		if tool.Name == "search_sessions" || tool.Name == "read_session_history" {
			if !strings.Contains(tool.Description, "untrusted") {
				t.Fatalf("%s description lacks the untrusted-data warning", tool.Name)
			}
			raw, _ := json.Marshal(tool.InputSchema)
			var s jsonschema.Schema
			if err := json.Unmarshal(raw, &s); err != nil {
				t.Fatal(err)
			}
			schemas[tool.Name] = &s
		}
	}
	if !strings.Contains(cs.InitializeResult().Instructions, "read_session_history") {
		t.Fatal("server instructions do not mention history tools")
	}
	// Unbound callers may search; errors are JSON payloads with stable codes.
	r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "search_sessions", Arguments: map[string]any{}})
	if err != nil || !r.IsError {
		t.Fatalf("search %v %v", r, err)
	}
	var payload struct {
		Error struct {
			Code      string `json:"code"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(r.Content[0].(*mcp.TextContent).Text), &payload) != nil || payload.Error.Code != "CATALOG_UNAVAILABLE" || !payload.Error.Retryable {
		t.Fatalf("payload %s", r.Content[0].(*mcp.TextContent).Text)
	}
	// The published schemas agree with the decoder on acceptance.
	for _, tc := range []struct {
		tool, args string
		ok         bool
	}{
		{"search_sessions", `{}`, true},
		{"search_sessions", `{"limit":100,"agent_type":"codex","name_contains":"x"}`, true},
		{"search_sessions", `{"limit":101}`, false},
		{"search_sessions", `{"extra":true}`, false},
		{"search_sessions", `{"agent_type":"gpt"}`, false},
		{"search_sessions", `{"name_contains":""}`, false},
		{"read_session_history", `{"session_id":"s","agent_type":"claude"}`, true},
		{"read_session_history", `{"session_id":"s"}`, false},
		{"read_session_history", `{"session_id":"s","agent_type":"claude","limit":201}`, false},
		{"read_session_history", `{"session_id":"s","agent_type":"claude","cursor":5}`, false},
	} {
		resolved, err := schemas[tc.tool].Resolve(nil)
		if err != nil {
			t.Fatal(err)
		}
		var v any
		_ = json.Unmarshal([]byte(tc.args), &v)
		schemaOK := resolved.Validate(v) == nil
		var decodeErr *historyError
		if tc.tool == "search_sessions" {
			_, _, decodeErr = parseSearch(json.RawMessage(tc.args))
		} else {
			_, decodeErr = hh.h.read(ctx, n, json.RawMessage(tc.args))
			if decodeErr != nil && decodeErr.Code != "INVALID_ARGUMENT" {
				decodeErr = nil
			}
		}
		if schemaOK != tc.ok || (decodeErr == nil) != tc.ok {
			t.Fatalf("%s %s: schema=%v decoder=%v want %v", tc.tool, tc.args, schemaOK, decodeErr == nil, tc.ok)
		}
	}
	// Codex mode offers the tools and still rejects unverified callers.
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	manager := &codexNodes{ctx: cctx, nodes: map[string]*Node{}, opts: Options{Runtime: filepath.Join(t.TempDir(), "run")}, binary: "/bin/false", socket: "/nonexistent.sock"}
	defer manager.close()
	st2, ct2 := mcp.NewInMemoryTransports()
	ss2, err := makeServer(manager.resolve, agentCodex, func() {}, hh.h).Connect(cctx, st2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss2.Close()
	cs2, err := mcp.NewClient(&mcp.Implementation{Name: "codex-test", Version: "1"}, nil).Connect(cctx, ct2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs2.Close()
	tools, err = cs2.ListTools(cctx, nil)
	if err != nil || len(tools.Tools) != 13 {
		t.Fatalf("codex tools %v %v", tools, err)
	}
	r, err = cs2.CallTool(cctx, &mcp.CallToolParams{Name: "read_session_history", Arguments: map[string]any{"session_id": "s", "agent_type": "claude"}})
	if err != nil || !r.IsError || !strings.Contains(r.Content[0].(*mcp.TextContent).Text, `"code":"CALLER_UNVERIFIED"`) {
		t.Fatalf("codex unverified %v %v", r, err)
	}
	r, err = cs2.CallTool(cctx, &mcp.CallToolParams{Meta: mcp.Meta{"threadId": "11111111-2222-3333-4444-555555555555"}, Name: "search_sessions", Arguments: map[string]any{}})
	if err != nil || !r.IsError || !strings.Contains(r.Content[0].(*mcp.TextContent).Text, `"code":"CATALOG_UNAVAILABLE"`) {
		t.Fatalf("codex search %v %v", r, err)
	}
}

func TestHistoryIndexIncrementalAndReplacement(t *testing.T) {
	hh := newHistoryHarness(t)
	path := filepath.Join(hh.o.opts.ClaudeRoot, "s.jsonl")
	appendLog(t, path, assistantText("s", "/p", "a", "2026-10-01T00:00:00Z", "x"))
	hh.index()
	f := hh.o.index.files[path]
	indexed := f.Indexed
	// A partial tail is not indexed until it completes; appends extend the checkpoint.
	line := assistantText("s", "/p", "b", "2026-10-05T00:00:00Z", "y")
	appendLog(t, path, line[:10])
	hh.o.visit(path, false)
	hh.index()
	if f.Indexed != indexed || f.LastAt.Format(time.RFC3339) != "2026-10-01T00:00:00Z" {
		t.Fatalf("partial tail indexed: %d %v", f.Indexed, f.LastAt)
	}
	appendLog(t, path, line[10:])
	hh.o.visit(path, false)
	hh.index()
	if f.Indexed != indexed+int64(len(line)) || f.LastAt.Format(time.RFC3339) != "2026-10-05T00:00:00Z" {
		t.Fatalf("append not indexed: %d %v", f.Indexed, f.LastAt)
	}
	// Replacement with different earlier bytes discards stale metadata.
	if err := os.WriteFile(path, []byte(assistantText("replaced", "/q", "z", "2026-10-02T00:00:00Z", "z")), 0600); err != nil {
		t.Fatal(err)
	}
	hh.o.visit(path, false)
	hh.index()
	if f.Session != "replaced" || f.Cwd != "/q" || f.LastAt.Format(time.RFC3339) != "2026-10-02T00:00:00Z" {
		t.Fatalf("replacement kept stale metadata: %+v", f)
	}
	// Limits are visible, never silent.
	hh.o.index.limitHit = true
	hh.o.index.dirty = true
	hh.o.index.flush()
	lock, _ := localstate.Lock(filepath.Join(hh.scope.Runtime, "observer.lock"), false)
	defer lock.Close()
	out, _ := hh.search(`{}`)
	cat := out["catalog"].(map[string]any)
	if cat["state"] != "degraded" || cat["coverage_complete"] != false || !strings.Contains(fmt.Sprint(cat["warnings"]), "index_limit") {
		t.Fatalf("limit hidden: %v", cat)
	}
	health := hh.o.index.health()
	if health["state"] != "degraded" || health["coverage_complete"] != false {
		t.Fatalf("health %v", health)
	}
	raw, _ := json.Marshal(health)
	if strings.Contains(string(raw), "/p") || strings.Contains(string(raw), "replaced") {
		t.Fatal("health leaked paths or session IDs")
	}
}

// TestHistoryDocsMatchImplementation keeps docs/history.md honest: every JSON
// example parses, and every documented error code is one the code can emit.
func TestHistoryDocsMatchImplementation(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "history.md"))
	if err != nil {
		t.Fatal(err)
	}
	blocks := strings.Split(string(doc), "```json\n")
	if len(blocks) < 8 {
		t.Fatalf("expected documented JSON examples, found %d", len(blocks)-1)
	}
	for _, block := range blocks[1:] {
		body := block[:strings.Index(block, "```")]
		var v any
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatalf("invalid JSON example: %v\n%s", err, body)
		}
	}
	source := ""
	for _, name := range []string{"history.go", "history_read.go", "history_mcp.go", "history_peer.go"} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		source += string(b)
	}
	for _, code := range []string{"INVALID_ARGUMENT", "CATALOG_UNAVAILABLE", "CATALOG_INCOMPATIBLE", "SESSION_NOT_FOUND", "SESSION_AMBIGUOUS", "HISTORY_UNAVAILABLE", "INVALID_CURSOR", "CURSOR_EXPIRED", "SOURCE_CHANGED", "RESOURCE_LIMIT", "CALLER_UNVERIFIED", "REMOTE_HISTORY_DISABLED", "POLICY_DENIED", "REMOTE_HISTORY_UNSUPPORTED", "PEER_UNAVAILABLE", "GATEWAY_UNAVAILABLE"} {
		if !strings.Contains(string(doc), "`"+code+"`") || !strings.Contains(source, `"`+code+`"`) {
			t.Fatalf("error code %s is not both documented and emitted", code)
		}
	}
	for _, warning := range []string{"service_stopped", "initial_inventory", "pending_updates", "watch_degraded", "index_limit", "queue_overflow", "read_failures", "snapshot_rows_removed"} {
		if !strings.Contains(string(doc), "`"+warning+"`") {
			t.Fatalf("warning %s undocumented", warning)
		}
	}
	for _, path := range []string{"service.md", "../README.md"} {
		if _, err := os.Stat(filepath.Join("..", "..", "docs", path)); err != nil {
			t.Fatalf("documentation link target missing: %s", path)
		}
	}
}

func TestHistoryRenameNotLiveSession(t *testing.T) {
	hh := newHistoryHarness(t)
	codexDir := filepath.Join(hh.o.opts.CodexRoot, "2026", "10", "03")
	if err := os.MkdirAll(codexDir, 0700); err != nil {
		t.Fatal(err)
	}
	appendLog(t, filepath.Join(codexDir, "rollout-x.jsonl"), codexMeta("s-codex", "/work/other", "2026-10-03T08:00:00Z")+codexAssistant("m1", "2026-10-03T08:00:02Z", "done"))
	hh.index()
	hh.n.runtime = hh.scope.Runtime
	out, err := hh.n.renameSession(hh.h, "s-codex", "Cursor integration")
	if err != nil || out["session_name"] != "Cursor integration" || out["source"] != "explicit" {
		t.Fatalf("rename %v %v", out, err)
	}
	row := func(h *historyService) map[string]any {
		t.Helper()
		res, herr := h.search(context.Background(), hh.n, json.RawMessage(`{"session_id":"s-codex"}`))
		if herr != nil {
			t.Fatal(herr)
		}
		rows := sessionsOf(roundTrip(t, res))
		if len(rows) != 1 {
			t.Fatalf("rows %v", rows)
		}
		return rows[0]
	}
	if r := row(hh.h); r["session_name"] != "Cursor integration" || r["name_source"] != "explicit" || r["live_status"] != "not_discovered" {
		t.Fatalf("renamed row %v", r)
	}
	// A restarted reader over the same runtime still sees the override.
	if r := row(newHistoryService(hh.scope)); r["session_name"] != "Cursor integration" {
		t.Fatalf("override lost on restart %v", r)
	}
	out, err = hh.n.renameSession(hh.h, "s-codex", "")
	if err != nil || out["session_name"] != "other · codex · s-codex" || out["source"] != "fallback" {
		t.Fatalf("clear %v %v", out, err)
	}
	if r := row(hh.h); r["name_source"] != "fallback" {
		t.Fatalf("cleared row %v", r)
	}
	if _, err := hh.n.renameSession(hh.h, "s-unknown", "x"); !errors.Is(err, errRenameNotFound) {
		t.Fatalf("unknown session %v", err)
	}
}
