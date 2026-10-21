package channel

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rhizome/internal/config"

	_ "modernc.org/sqlite"
)

// openCodeFixture builds an OpenCode 2.x database containing only the tables
// and columns the history reader depends on, in WAL mode like the real one.
type openCodeFixture struct {
	t    *testing.T
	path string
	db   *sql.DB
}

func newOpenCodeFixture(t *testing.T, root string) *openCodeFixture {
	t.Helper()
	path := filepath.Join(root, openCodeDBName)
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(wal)&_pragma=busy_timeout(2000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	f := &openCodeFixture{t: t, path: path, db: db}
	f.exec(`CREATE TABLE session_v2 (id text PRIMARY KEY, project_id text NOT NULL DEFAULT 'p', parent_id text, directory text NOT NULL, title text, time_created integer NOT NULL, time_updated integer NOT NULL)`)
	f.exec(`CREATE TABLE session_message (id text PRIMARY KEY, session_id text NOT NULL, type text NOT NULL, seq integer NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL DEFAULT 0, data text NOT NULL)`)
	f.exec(`CREATE UNIQUE INDEX session_message_session_seq_idx ON session_message (session_id, seq)`)
	return f
}

func (f *openCodeFixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(q, args...); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
}

func ms(ts string) int64 {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		panic(err)
	}
	return t.UnixMilli()
}

func (f *openCodeFixture) session(id, dir, title, parent, created, updated string) {
	f.t.Helper()
	var p any
	if parent != "" {
		p = parent
	}
	f.exec(`INSERT INTO session_v2 (id, directory, title, parent_id, time_created, time_updated) VALUES (?, ?, ?, ?, ?, ?)`, id, dir, title, p, ms(created), ms(updated))
}

func (f *openCodeFixture) message(session string, seq int, typ, ts string, data any) {
	f.t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		f.t.Fatal(err)
	}
	f.exec(`INSERT INTO session_message (id, session_id, type, seq, time_created, data) VALUES (?, ?, ?, ?, ?, ?)`, fmt.Sprintf("%s-%d", session, seq), session, typ, seq, ms(ts), string(raw))
}

type ocPart = map[string]any

func ocText(text string) ocPart { return ocPart{"type": "text", "text": text} }
func ocTool(name, status, created, completed string) ocPart {
	return ocPart{"type": "tool", "id": "c-" + name, "name": name, "state": ocPart{"status": status, "input": ocPart{"secret": "TOOL-INPUT"}, "content": "TOOL-OUTPUT"}, "time": ocPart{"created": ms(created), "completed": ms(completed)}}
}
func ocAssistant(parts ...ocPart) ocPart { return ocPart{"agent": "build", "content": parts} }

const (
	ocSession = "ses_hist0001"
	ocChild   = "ses_child0001"
)

// seedOpenCode writes one session with a full human turn, a tool turn, and a
// peer-only turn, plus a child session that must never be cataloged.
func seedOpenCode(f *openCodeFixture, dir string) {
	f.session(ocSession, dir, "Add history", "", "2026-10-20T10:00:00Z", "2026-10-20T10:04:00Z")
	f.session(ocChild, dir, "subagent", ocSession, "2026-10-20T10:01:00Z", "2026-10-20T10:02:00Z")
	f.message(ocSession, 1, "user", "2026-10-20T10:00:01Z", ocPart{"text": "PROMPT-SECRET"})
	f.message(ocSession, 2, "assistant", "2026-10-20T10:00:05Z", ocAssistant(
		ocPart{"type": "reasoning", "text": "THINKING-SECRET"},
		ocText("I will read the file."),
		ocTool("read", "completed", "2026-10-20T10:00:06Z", "2026-10-20T10:00:07Z"),
		ocTool("execute", "completed", "2026-10-20T10:00:08Z", "2026-10-20T10:00:09Z"),
		ocTool("rhizome_send", "completed", "2026-10-20T10:00:09Z", "2026-10-20T10:00:10Z"),
		ocTool("bash", "error", "2026-10-20T10:00:11Z", "2026-10-20T10:00:12Z"),
		ocText("   "),
	))
	f.message(ocSession, 3, "system", "2026-10-20T10:00:13Z", ocPart{"text": "SYSTEM-SECRET"})
	f.message(ocSession, 4, "idle", "2026-10-20T10:00:14Z", ocPart{"outcome": "succeeded"})
	// A peer delivery starts a peer-only turn that ends with idle.
	f.message(ocSession, 5, "synthetic", "2026-10-20T10:01:00Z", ocPart{"text": "<rhizome-peer-event>PEER-SECRET</rhizome-peer-event>"})
	f.message(ocSession, 6, "assistant", "2026-10-20T10:01:05Z", ocAssistant(ocText("PEER-REACTION"), ocTool("read", "completed", "2026-10-20T10:01:06Z", "2026-10-20T10:01:07Z")))
	f.message(ocSession, 7, "idle", "2026-10-20T10:01:08Z", ocPart{"outcome": "succeeded"})
	// A later human prompt ends the peer turn.
	f.message(ocSession, 8, "user", "2026-10-20T10:03:00Z", ocPart{"text": "second prompt"})
	f.message(ocSession, 9, "assistant", "2026-10-20T10:03:05Z", ocAssistant(ocText("All done.")))
	f.message(ocSession, 10, "idle", "2026-10-20T10:04:00Z", ocPart{"outcome": "succeeded"})
	f.message(ocChild, 1, "assistant", "2026-10-20T10:01:30Z", ocAssistant(ocText("CHILD-SECRET")))
}

func openCodeHarness(t *testing.T) (*historyHarness, *openCodeFixture, string) {
	t.Helper()
	hh := newHistoryHarness(t)
	f := newOpenCodeFixture(t, hh.scope.OpenCodeRoot)
	dir := t.TempDir()
	seedOpenCode(f, dir)
	hh.index()
	return hh, f, dir
}

func TestHistoryOpenCodeCatalogRows(t *testing.T) {
	hh, _, dir := openCodeHarness(t)
	out, err := hh.search(`{"agent_type":"opencode"}`)
	if err != nil {
		t.Fatal(err)
	}
	rows := sessionsOf(out)
	if len(rows) != 1 {
		t.Fatalf("want exactly the parent session (child excluded), got %v", rows)
	}
	r := rows[0]
	for k, want := range map[string]any{"session_id": ocSession, "agent_type": "opencode", "project_path": filepath.Clean(dir), "display_name": filepath.Base(dir), "session_name": "Add history", "name_source": "native", "history_availability": "available", "started_at": "2026-10-20T10:00:00Z", "last_activity_at": "2026-10-20T10:04:00Z"} {
		if r[k] != want {
			t.Errorf("%s: got %v, want %v", k, r[k], want)
		}
	}
	// The harness has no running observer service, so the catalog is only "stale";
	// nothing else (pending work, schema trouble) may be reported.
	if w := fmt.Sprint(out["catalog"].(map[string]any)["warnings"]); strings.Count(w, "code:") != 1 || !strings.Contains(w, "service_stopped") {
		t.Fatalf("catalog warnings: %v", w)
	}
	// The default (omitted) filter covers OpenCode too, and names are searchable.
	all, _ := hh.search(`{"name_contains":"history"}`)
	if len(sessionsOf(all)) != 1 {
		t.Fatalf("name search: %v", all)
	}
	if none, _ := hh.search(`{"agent_type":"claude"}`); len(sessionsOf(none)) != 0 {
		t.Fatalf("claude filter leaked an OpenCode session: %v", none)
	}
}

func TestHistoryOpenCodeReadContentPolicyAndPagination(t *testing.T) {
	hh, _, _ := openCodeHarness(t)
	out, err := hh.read(fmt.Sprintf(`{"session_id":%q,"agent_type":"opencode","limit":100}`, ocSession))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, ev := range eventsOf(out) {
		text := ""
		if s, ok := ev["text"].(string); ok {
			text = ": " + s
		}
		got = append(got, fmt.Sprint(ev["kind"], text))
	}
	want := []string{
		"assistant_text: I will read the file.",
		"tool_summary: Tool started: read", "tool_summary: Tool completed: read",
		"tool_summary: Tool started: bash", "tool_summary: Tool failed: bash",
		"turn_completed",
		"assistant_text: All done.",
		"turn_completed",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	raw, _ := json.Marshal(out)
	for _, secret := range []string{"PROMPT-SECRET", "THINKING-SECRET", "SYSTEM-SECRET", "PEER-SECRET", "PEER-REACTION", "TOOL-INPUT", "TOOL-OUTPUT", "CHILD-SECRET", "rhizome_send", "execute"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("response leaked %q: %s", secret, raw)
		}
	}
	if cov := out["coverage"].(map[string]any); cov["state"] != "complete" || cov["content_policy"] != "assistant_activity_only" {
		t.Fatalf("coverage: %v", cov)
	}
	if out["next_cursor"] != nil {
		t.Fatalf("a complete read must not return a cursor: %v", out["next_cursor"])
	}
	ids := map[string]bool{}
	for _, ev := range eventsOf(out) {
		id := ev["event_id"].(string)
		if ids[id] {
			t.Fatalf("duplicate event id %s", id)
		}
		ids[id] = true
		if ev["timestamp"] == nil {
			t.Fatalf("event without timestamp: %v", ev)
		}
	}
	// Tool events carry the part's own times; text carries the row time.
	evs := eventsOf(out)
	if evs[1]["timestamp"] != "2026-10-20T10:00:06Z" || evs[2]["timestamp"] != "2026-10-20T10:00:07Z" || evs[0]["timestamp"] != "2026-10-20T10:00:05Z" {
		t.Fatalf("timestamps: %v %v %v", evs[0]["timestamp"], evs[1]["timestamp"], evs[2]["timestamp"])
	}

	// Pagination: one event per page reproduces the same sequence exactly once.
	var paged []string
	args := fmt.Sprintf(`{"session_id":%q,"agent_type":"opencode","limit":1}`, ocSession)
	for page := 0; page < 40; page++ {
		pg, err := hh.read(args)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, ev := range eventsOf(pg) {
			text := ""
			if s, ok := ev["text"].(string); ok {
				text = ": " + s
			}
			paged = append(paged, fmt.Sprint(ev["kind"], text))
		}
		c := cursorOf(pg)
		if c == "" {
			break
		}
		args = fmt.Sprintf(`{"session_id":%q,"agent_type":"opencode","limit":1,"cursor":%q}`, ocSession, c)
	}
	if strings.Join(paged, "\n") != strings.Join(want, "\n") {
		t.Fatalf("paged events:\n%s", strings.Join(paged, "\n"))
	}
}

func TestHistoryOpenCodeSnapshotBoundaryAndSourceChanges(t *testing.T) {
	hh, f, _ := openCodeHarness(t)
	args := fmt.Sprintf(`{"session_id":%q,"agent_type":"opencode","limit":1}`, ocSession)
	first, err := hh.read(args)
	if err != nil {
		t.Fatal(err)
	}
	// Rows appended after the snapshot are not served by its later pages.
	f.message(ocSession, 11, "assistant", "2026-10-20T10:05:00Z", ocAssistant(ocText("LATE-TEXT")))
	f.message(ocSession, 12, "idle", "2026-10-20T10:05:01Z", ocPart{"outcome": "succeeded"})
	var seen []string
	cur := cursorOf(first)
	for i := 0; i < 40 && cur != ""; i++ {
		pg, err := hh.read(fmt.Sprintf(`{"session_id":%q,"agent_type":"opencode","limit":1,"cursor":%q}`, ocSession, cur))
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range eventsOf(pg) {
			if s, ok := ev["text"].(string); ok {
				seen = append(seen, s)
			}
		}
		cur = cursorOf(pg)
	}
	if strings.Contains(strings.Join(seen, "|"), "LATE-TEXT") {
		t.Fatalf("snapshot served rows written after it was captured: %v", seen)
	}
	// A fresh read sees them.
	fresh, _ := hh.read(fmt.Sprintf(`{"session_id":%q,"agent_type":"opencode","limit":100}`, ocSession))
	if !strings.Contains(fmt.Sprint(eventsOf(fresh)), "LATE-TEXT") {
		t.Fatal("a fresh read must include later rows")
	}

	// Deleting the session mid-pagination fails the next page with SOURCE_CHANGED.
	pg1, _ := hh.read(args)
	f.exec(`DELETE FROM session_message WHERE session_id = ?`, ocSession)
	f.exec(`DELETE FROM session_v2 WHERE id = ?`, ocSession)
	_, e := hh.read(fmt.Sprintf(`{"session_id":%q,"agent_type":"opencode","limit":1,"cursor":%q}`, ocSession, cursorOf(pg1)))
	if e == nil || e.Code != "SOURCE_CHANGED" {
		t.Fatalf("want SOURCE_CHANGED, got %v", e)
	}
}

func TestHistoryOpenCodeRefreshAndTombstones(t *testing.T) {
	hh, f, dir := openCodeHarness(t)
	f.session("ses_new", dir, "Second", "", "2026-10-21T09:00:00Z", "2026-10-21T09:00:00Z")
	hh.o.index.openCodeChanged()
	hh.index()
	if out, _ := hh.search(`{"agent_type":"opencode"}`); len(sessionsOf(out)) != 2 {
		t.Fatalf("new session not cataloged: %v", out)
	}
	// A message newer than the session row moves last activity forward.
	f.message("ses_new", 1, "assistant", "2026-10-21T09:30:00Z", ocAssistant(ocText("hello")))
	hh.o.index.openCodeChanged()
	hh.index()
	out, _ := hh.search(`{"session_id":"ses_new","agent_type":"opencode"}`)
	if got := sessionsOf(out)[0]["last_activity_at"]; got != "2026-10-21T09:30:00Z" {
		t.Fatalf("last activity: %v", got)
	}
	// A renamed session updates its native title without a new identity.
	f.exec(`UPDATE session_v2 SET title = ? WHERE id = ?`, "Renamed", "ses_new")
	hh.o.index.openCodeChanged()
	hh.index()
	out, _ = hh.search(`{"session_id":"ses_new","agent_type":"opencode"}`)
	if sessionsOf(out)[0]["session_name"] != "Renamed" {
		t.Fatalf("title: %v", out)
	}
	// A deleted session keeps a tombstone but serves no content.
	f.exec(`DELETE FROM session_message WHERE session_id = ?`, "ses_new")
	f.exec(`DELETE FROM session_v2 WHERE id = ?`, "ses_new")
	hh.o.index.openCodeChanged()
	hh.index()
	out, _ = hh.search(`{"session_id":"ses_new","agent_type":"opencode"}`)
	if r := sessionsOf(out)[0]; r["history_availability"] != "missing" {
		t.Fatalf("tombstone: %v", r)
	}
	if _, e := hh.read(`{"session_id":"ses_new","agent_type":"opencode"}`); e == nil || e.Code != "HISTORY_UNAVAILABLE" {
		t.Fatalf("read of a removed session: %v", e)
	}
}

func TestHistoryOpenCodeDatabaseRemovedAndSchemaUnsupported(t *testing.T) {
	hh, f, _ := openCodeHarness(t)
	// An incompatible schema is reported, never read through a fallback.
	f.exec(`ALTER TABLE session_message RENAME COLUMN seq TO sequence`)
	hh.o.index.openCodeChanged()
	hh.index()
	out, _ := hh.search(`{"agent_type":"opencode"}`)
	r := sessionsOf(out)[0]
	if r["history_availability"] != "unsupported" || fmt.Sprint(r["availability_reasons"]) != "["+openCodeSchemaReason+"]" {
		t.Fatalf("schema row: %v", r)
	}
	warned := false
	for _, w := range out["catalog"].(map[string]any)["warnings"].([]any) {
		warned = warned || w.(map[string]any)["code"] == "opencode_schema"
	}
	if !warned {
		t.Fatalf("catalog: %v", out["catalog"])
	}
	if _, e := hh.read(fmt.Sprintf(`{"session_id":%q,"agent_type":"opencode"}`, ocSession)); e == nil || e.Code != "HISTORY_UNAVAILABLE" {
		t.Fatalf("read with unsupported schema: %v", e)
	}
	// Removing the database tombstones every session.
	f.db.Close()
	for _, p := range []string{f.path, f.path + "-wal", f.path + "-shm"} {
		os.Remove(p)
	}
	hh.o.index.openCodeChanged()
	hh.index()
	out, _ = hh.search(`{"agent_type":"opencode"}`)
	if r := sessionsOf(out)[0]; r["history_availability"] != "missing" {
		t.Fatalf("after removal: %v", r)
	}
}

func TestHistoryOpenCodeRefusesSymlinkedDatabase(t *testing.T) {
	hh := newHistoryHarness(t)
	elsewhere := t.TempDir()
	f := newOpenCodeFixture(t, elsewhere)
	seedOpenCode(f, t.TempDir())
	if err := os.Symlink(f.path, filepath.Join(hh.scope.OpenCodeRoot, openCodeDBName)); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	hh.index()
	if out, _ := hh.search(`{"agent_type":"opencode"}`); len(sessionsOf(out)) != 0 {
		t.Fatalf("a symlinked database must not be indexed: %v", out)
	}
	if _, _, e := hh.scope.confineOpenCode(openCodeSourcePath(hh.scope.OpenCodeRoot, ocSession)); e == nil || e.Extra["reason"] != "source_unsupported" {
		t.Fatalf("confine: %v", e)
	}
}

func TestHistoryOpenCodeConfineRejectsForeignPaths(t *testing.T) {
	scope := historyScope{OpenCodeRoot: "/data/opencode"}
	for _, path := range []string{
		"/data/opencode/other.db#ses_a", "/etc/passwd#ses_a", "/data/opencode/opencode.db", "/data/opencode/opencode.db#",
		"/data/opencode/opencode.db#ses/a", "/data/opencode/sub/opencode.db#ses_a", "/data/opencode/../opencode.db#ses_a",
	} {
		if _, _, e := scope.confineOpenCode(path); e == nil || e.Extra["reason"] != "source_outside_roots" {
			t.Errorf("%s: %v", path, e)
		}
	}
	if (historyScope{}).hash() == (historyScope{OpenCodeRoot: "/x"}).hash() {
		t.Fatal("the catalog scope must bind the OpenCode root")
	}
}

func TestHistoryOpenCodeOversizedRowAndMalformedRecord(t *testing.T) {
	hh, f, _ := openCodeHarness(t)
	big := strings.Repeat("x", maxRecord+1)
	f.message(ocSession, 11, "assistant", "2026-10-20T10:06:00Z", ocAssistant(ocText(big)))
	f.exec(`INSERT INTO session_message (id, session_id, type, seq, time_created, data) VALUES ('bad', ?, 'assistant', 12, ?, 'not json')`, ocSession, ms("2026-10-20T10:06:01Z"))
	f.message(ocSession, 13, "assistant", "2026-10-20T10:06:02Z", ocAssistant(ocText("after the bad rows")))
	out, err := hh.read(fmt.Sprintf(`{"session_id":%q,"agent_type":"opencode","limit":100}`, ocSession))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "xxxxxxxx") || !strings.Contains(string(raw), "after the bad rows") {
		t.Fatalf("oversized row served or later rows lost: %.300s", raw)
	}
	cov := out["coverage"].(map[string]any)
	if cov["state"] != "partial" || cov["skipped_records"].(float64) != 1 || !strings.Contains(fmt.Sprint(cov["gaps"]), "oversized_record") {
		t.Fatalf("coverage: %v", cov)
	}
}

func TestHistoryOpenCodePeerPolicyAppliesToEvents(t *testing.T) {
	hh, _, _ := openCodeHarness(t)
	snap, e := hh.h.openSnapshot(context.Background(), "opencode", ocSession, "q", mustSource(t, hh))
	if e != nil {
		t.Fatal(e)
	}
	defer hh.h.snaps.release(snap, true)
	// Window opens after the first human turn: only later events are visible.
	policy := historyPolicyFor(t, "opencode", 1)
	snap.access = &historyAccess{host: "h", grant: "g", policy: policy, now: time.Date(2026, 10, 21, 10, 3, 30, 0, time.UTC)}
	events, _, e := hh.h.readPage(context.Background(), snap, 100)
	if e != nil {
		t.Fatal(e)
	}
	// 24 h before 10-21T10:03:30 is 10-20T10:03:30: only the final idle (10:04:00)
	// is inside the window; the other seven events are withheld and counted.
	if len(events) != 1 || events[0].Kind != "turn_completed" {
		t.Fatalf("lookback window kept %+v", events)
	}
	if snap.gaps["outside_lookback"] != 7 {
		t.Fatalf("withheld events must be counted: %v", snap.gaps)
	}
}

// historyPolicyFor shares one agent's history over a rolling window of days.
func historyPolicyFor(t *testing.T, agent string, days int64) config.HistoryPolicy {
	t.Helper()
	p := config.HistoryPolicy{Enabled: true}
	switch agent {
	case "opencode":
		p.OpenCode = config.AgentHistory{Enabled: true, Days: days}
	default:
		t.Fatalf("unsupported agent %s", agent)
	}
	return p
}

func mustSource(t *testing.T, hh *historyHarness) *indexedFile {
	t.Helper()
	c, _, e := hh.h.catalog.load()
	if e != nil {
		t.Fatal(e)
	}
	s := c.sessions()[sessionKey{"opencode", ocSession}]
	if s == nil || s.source() == nil {
		t.Fatalf("session not cataloged")
	}
	return s.source()
}

func TestObserverOpenCodeEvents(t *testing.T) {
	hh := newHistoryHarness(t)
	o := hh.o
	root := o.opts.OpenCodeRoot
	o.index.ocDirty = false
	for name, want := range map[string]bool{
		filepath.Join(root, "opencode.db"):         true,
		filepath.Join(root, "opencode.db-wal"):     true,
		filepath.Join(root, "opencode.db-shm"):     false,
		filepath.Join(root, "auth.json"):           false,
		filepath.Join(root, "log", "2026.log"):     false,
		filepath.Join(root, "snapshot", "x", "y"):  false,
		filepath.Join(filepath.Dir(root), "other"): false,
	} {
		o.index.ocDirty = false
		o.openCodeEvent(name)
		if o.index.ocDirty != want {
			t.Errorf("%s: dirty=%v, want %v", name, o.index.ocDirty, want)
		}
	}
	o.index.ocDirty = false
	o.openCodeEvent(root) // the directory itself (re)appearing refreshes and re-registers
	if !o.index.ocDirty || !o.dirs[root] {
		t.Fatalf("root event: dirty=%v watched=%v", o.index.ocDirty, o.dirs[root])
	}
}
