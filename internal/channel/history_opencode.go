package channel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite" // pure-Go driver; opened read-only, never written
)

// OpenCode 2.x keeps every session in one SQLite database instead of one log
// file per session. The history catalog still models each session as one
// indexedFile whose private Path is "<root>/opencode.db#<session id>", so
// grouping, availability, tombstones, bounds, policy and pagination stay shared
// with the file-based agents. Only the metadata refresh and the page reader
// differ. The database is only ever opened read-only; the reader never writes,
// checkpoints or contacts OpenCode.
const (
	openCodeDBName       = "opencode.db"
	openCodeQueryTimeout = 5 * time.Second
	openCodeRefreshEvery = 2 * time.Second // minimum gap between metadata refreshes
	openCodeBatch        = 64              // session_message rows per query
	openCodeSchemaReason = "opencode_schema_unsupported"
)

// openCodeColumns are the columns of the 2.x schema this reader depends on.
// A database without all of them is reported, never read through a fallback.
var openCodeColumns = map[string][]string{
	"session_v2":      {"id", "directory", "title", "parent_id", "time_created", "time_updated"},
	"session_message": {"id", "session_id", "type", "seq", "time_created", "data"},
}

// openCodeSourcePath is the private catalog path for one session.
func openCodeSourcePath(root, session string) string {
	return filepath.Join(root, openCodeDBName) + "#" + session
}

// splitOpenCodeSource inverts openCodeSourcePath.
func splitOpenCodeSource(path string) (db, session string, ok bool) {
	i := strings.LastIndex(path, "#")
	if i <= 0 || i == len(path)-1 {
		return "", "", false
	}
	return path[:i], path[i+1:], true
}

// validOpenCodeSessionID bounds identity strings before they key the catalog.
func validOpenCodeSessionID(id string) bool {
	return id != "" && len(id) <= 128 && utf8.ValidString(id) && !strings.ContainsAny(id, "#\x00/\\")
}

// openOpenCodeDB opens the database read-only with a bounded busy timeout. The
// connection is single, short-lived and query_only, so it holds no WAL snapshot
// between calls and cannot modify OpenCode's data.
func openOpenCodeDB(dbPath string) (*sql.DB, error) {
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(2000)")
	q.Add("_pragma", "query_only(true)")
	dsn := (&url.URL{Scheme: "file", Path: dbPath, RawQuery: q.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// openCodeSchemaOK reports whether every column the reader needs exists.
func openCodeSchemaOK(ctx context.Context, db *sql.DB) (bool, error) {
	for table, want := range openCodeColumns {
		rows, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info('"+table+"')")
		if err != nil {
			return false, err
		}
		have := map[string]bool{}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return false, err
			}
			have[name] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return false, err
		}
		rows.Close()
		for _, col := range want {
			if !have[col] {
				return false, nil
			}
		}
	}
	return true, nil
}

// confineOpenCode re-checks, before every open, that the database is a regular
// file directly inside the configured data directory and that neither is a
// symlink. Catalog paths are never trusted after the fact.
func (s historyScope) confineOpenCode(path string) (db string, session string, e *historyError) {
	bad := func(reason, msg string, retry bool) *historyError {
		return &historyError{Code: "HISTORY_UNAVAILABLE", Message: msg, Retryable: retry, Extra: map[string]any{"reason": reason}}
	}
	db, session, ok := splitOpenCodeSource(path)
	if !ok || s.OpenCodeRoot == "" || db != filepath.Join(s.OpenCodeRoot, openCodeDBName) || !validOpenCodeSessionID(session) {
		return "", "", bad("source_outside_roots", "the recorded source lies outside the configured log roots", false)
	}
	for _, p := range []string{s.OpenCodeRoot, db} {
		st, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			return "", "", bad("source_missing", "the OpenCode database for this session no longer exists", false)
		}
		if err != nil {
			return "", "", bad("source_unreadable", "the OpenCode database for this session is not readable", true)
		}
		if st.Mode()&os.ModeSymlink != 0 || (p == db && !st.Mode().IsRegular()) || (p != db && !st.IsDir()) {
			return "", "", bad("source_unsupported", "the OpenCode database path is not a regular file inside the configured data directory", false)
		}
	}
	return db, session, nil
}

// openCodeChanged schedules a metadata refresh. Filesystem events call it for
// the database and its write-ahead log; refreshes are rate limited.
func (h *historyIndex) openCodeChanged() {
	if h.scope.OpenCodeRoot != "" {
		h.ocDirty = true
	}
}

// openCodePending reports whether a refresh is outstanding, for coverage state.
func (h *historyIndex) openCodePending() int {
	if h.ocDirty {
		return 1
	}
	return 0
}

type openCodeRow struct {
	id, dir, title string
	parent         sql.NullString
	created, upd   int64
	lastMsg        sql.NullInt64
}

// indexOpenCode refreshes the catalog entries of every OpenCode session from
// one metadata query. It reads no message content.
func (h *historyIndex) indexOpenCode(ctx context.Context) {
	if h.scope.OpenCodeRoot == "" || !h.ocDirty || time.Now().Before(h.ocNext) {
		return
	}
	h.ocDirty = false
	h.ocNext = time.Now().Add(openCodeRefreshEvery)
	root := h.scope.OpenCodeRoot
	dbPath := filepath.Join(root, openCodeDBName)
	existing := func(yield func(*indexedFile)) {
		for _, f := range h.files {
			if f.Agent == "opencode" {
				yield(f)
			}
		}
	}
	if _, _, e := h.scope.confineOpenCode(openCodeSourcePath(root, "x")); e != nil {
		switch e.Extra["reason"] {
		case "source_missing":
			// OpenCode absent or its database removed: retain bounded tombstones.
			existing(func(f *indexedFile) { h.removed(f.Path) })
		default:
			h.counters.ReadFailures++
			existing(func(f *indexedFile) {
				if f.MissingAt == nil {
					f.partial("read_error")
					h.dirty = true
				}
			})
		}
		return
	}
	ctx, cancel := context.WithTimeout(ctx, openCodeQueryTimeout)
	defer cancel()
	db, err := openOpenCodeDB(dbPath)
	if err != nil {
		h.counters.ReadFailures++
		return
	}
	defer db.Close()
	ok, err := openCodeSchemaOK(ctx, db)
	if err != nil {
		h.counters.ReadFailures++
		existing(func(f *indexedFile) {
			if f.MissingAt == nil {
				f.partial("read_error")
				h.dirty = true
			}
		})
		return
	}
	if h.ocSchemaBad == ok {
		h.ocSchemaBad = !ok
		h.dirty = true
	}
	if !ok {
		existing(func(f *indexedFile) {
			if f.Unsupported != openCodeSchemaReason {
				f.Unsupported = openCodeSchemaReason
				h.dirty = true
			}
		})
		return
	}
	others := 0
	for _, f := range h.files {
		if f.Agent != "opencode" {
			others++
		}
	}
	room := maxCatalogFiles - others
	if room < 0 {
		room = 0
	}
	rows, err := db.QueryContext(ctx, `SELECT s.id, s.directory, COALESCE(s.title, ''), s.parent_id, s.time_created, s.time_updated,
		(SELECT MAX(m.time_created) FROM session_message m WHERE m.session_id = s.id)
		FROM session_v2 s ORDER BY s.time_updated DESC, s.id LIMIT ?`, room+1)
	if err != nil {
		h.counters.ReadFailures++
		existing(func(f *indexedFile) {
			if f.MissingAt == nil {
				f.partial("read_error")
				h.dirty = true
			}
		})
		return
	}
	defer rows.Close()
	var got []openCodeRow
	for rows.Next() {
		var r openCodeRow
		if err := rows.Scan(&r.id, &r.dir, &r.title, &r.parent, &r.created, &r.upd, &r.lastMsg); err != nil {
			h.counters.ReadFailures++
			return
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		h.counters.ReadFailures++
		return
	}
	truncated := len(got) > room
	if truncated {
		got = got[:room]
		if !h.limitHit {
			h.limitHit = true
		}
		h.dirty = true
	}
	seen := map[string]bool{}
	for _, r := range got {
		if !validOpenCodeSessionID(r.id) {
			continue
		}
		path := openCodeSourcePath(root, r.id)
		seen[path] = true
		f := h.files[path]
		if f == nil {
			f = &indexedFile{Path: path, Agent: "opencode", Session: r.id}
			h.files[path] = f
			h.counters.FilesIndexed++
		}
		next := *f
		next.Unsupported, next.Partial, next.MissingAt = "", nil, nil
		next.Child = r.parent.Valid && r.parent.String != ""
		next.Cwd = ""
		if filepath.IsAbs(r.dir) {
			next.Cwd = filepath.Clean(r.dir)
		}
		next.CustomTitle = cleanName(r.title)
		first, last := time.UnixMilli(r.created).UTC(), time.UnixMilli(r.upd).UTC()
		if r.lastMsg.Valid && r.lastMsg.Int64 > r.upd {
			last = time.UnixMilli(r.lastMsg.Int64).UTC()
		}
		next.FirstAt, next.LastAt = &first, &last
		if !sameIndexedFile(f, &next) {
			*f = next
			h.dirty = true
		}
	}
	if !truncated {
		existing(func(f *indexedFile) {
			if !seen[f.Path] && f.MissingAt == nil {
				h.removed(f.Path)
			}
		})
	}
}

// sameIndexedFile reports whether two catalog entries carry the same metadata.
func sameIndexedFile(a, b *indexedFile) bool {
	eq := func(x, y *time.Time) bool { return x == nil && y == nil || x != nil && y != nil && x.Equal(*y) }
	if len(a.Partial) != len(b.Partial) {
		return false
	}
	return a.Path == b.Path && a.Session == b.Session && a.Cwd == b.Cwd && a.Child == b.Child &&
		a.Unsupported == b.Unsupported && a.CustomTitle == b.CustomTitle && eq(a.FirstAt, b.FirstAt) &&
		eq(a.LastAt, b.LastAt) && eq(a.MissingAt, b.MissingAt)
}

// openCodeCursor is the snapshot state for one OpenCode session read. The
// boundary is the highest message seq at snapshot time; pages advance by seq.
type openCodeCursor struct {
	db, session  string
	maxSeq, next int64
	peer         bool // the current turn answers Rhizome deliveries only
}

func openCodeUnavailable(reason, msg string, retry bool) *historyError {
	return &historyError{Code: "HISTORY_UNAVAILABLE", Message: msg, Retryable: retry, Extra: map[string]any{"reason": reason}}
}

// openOpenCodeSnapshot fixes the seq boundary of one session. The session must
// still exist and must not be a child (subagent) session.
func (h *historyService) openOpenCodeSnapshot(ctx context.Context, session, query string, src *indexedFile) (*snapshot, *historyError) {
	dbPath, id, e := h.scope.confineOpenCode(src.Path)
	if e != nil {
		return nil, e
	}
	if id != session {
		return nil, openCodeUnavailable("source_unsupported", "the recorded source does not match the requested session", false)
	}
	ctx, cancel := context.WithTimeout(ctx, openCodeQueryTimeout)
	defer cancel()
	db, err := openOpenCodeDB(dbPath)
	if err != nil {
		return nil, openCodeUnavailable("source_unreadable", "the OpenCode database for this session is not readable", true)
	}
	defer db.Close()
	if ok, err := openCodeSchemaOK(ctx, db); err != nil {
		return nil, openCodeUnavailable("source_unreadable", "the OpenCode database for this session is not readable", true)
	} else if !ok {
		return nil, openCodeUnavailable(openCodeSchemaReason, "the OpenCode database does not have the 2.x session tables this build reads", false)
	}
	var parent sql.NullString
	var maxSeq sql.NullInt64
	err = db.QueryRowContext(ctx, `SELECT s.parent_id, (SELECT MAX(m.seq) FROM session_message m WHERE m.session_id = s.id) FROM session_v2 s WHERE s.id = ?`, id).Scan(&parent, &maxSeq)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, openCodeUnavailable("source_missing", "the session no longer exists in the OpenCode database; its metadata is retained but no content can be served", false)
	case err != nil:
		return nil, openCodeUnavailable("source_unreadable", "the OpenCode database for this session is not readable", true)
	case parent.Valid && parent.String != "":
		return nil, openCodeUnavailable("source_unsupported", "child (subagent) sessions are not served", false)
	}
	return &snapshot{
		kind: "read", query: query, agent: "opencode", session: session, path: src.Path, gaps: map[string]int{},
		sink: &historySink{identity: nil},
		oc:   &openCodeCursor{db: dbPath, session: id, maxSeq: maxSeq.Int64},
	}, nil
}

// readOpenCodePage emits the next permitted events of an OpenCode session in
// seq order. Like the file reader it advances parser state with every consumed
// row, so events that do not fit are carried, never reparsed.
func (h *historyService) readOpenCodePage(ctx context.Context, s *snapshot, limit int) ([]historyEvent, bool, *historyError) {
	dbPath, id, e := h.scope.confineOpenCode(s.path)
	if e != nil {
		return nil, false, e
	}
	changed := herr("SOURCE_CHANGED", "the OpenCode session was removed or rewritten since this snapshot was captured; run a fresh initial read", false)
	ctx, cancel := context.WithTimeout(ctx, openCodeQueryTimeout*2)
	defer cancel()
	db, err := openOpenCodeDB(dbPath)
	if err != nil {
		return nil, false, openCodeUnavailable("source_unreadable", "the OpenCode database for this session is not readable", true)
	}
	defer db.Close()
	var maxSeq sql.NullInt64
	switch err := db.QueryRowContext(ctx, `SELECT (SELECT MAX(m.seq) FROM session_message m WHERE m.session_id = s.id) FROM session_v2 s WHERE s.id = ?`, id).Scan(&maxSeq); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, false, changed
	case err != nil:
		return nil, false, openCodeUnavailable("source_unreadable", "the OpenCode database for this session is not readable", true)
	case maxSeq.Int64 < s.oc.maxSeq:
		return nil, false, changed
	}
	pg := &pager{s: s, limit: limit, events: []historyEvent{}}
	for len(s.carry) > 0 && pg.place(s.carry[0]) {
		s.carry = s.carry[1:]
	}
	start, scanned := s.oc.next, 0
	for len(s.carry) == 0 && len(pg.events) < limit && s.oc.next < s.oc.maxSeq && scanned < maxHistoryScan && ctx.Err() == nil {
		// Oversized rows are measured in bytes and never loaded.
		rows, err := db.QueryContext(ctx, `SELECT seq, type, time_created, length(CAST(data AS BLOB)),
			CASE WHEN length(CAST(data AS BLOB)) <= ? THEN data END
			FROM session_message WHERE session_id = ? AND seq > ? AND seq <= ? ORDER BY seq LIMIT ?`,
			maxRecord, id, s.oc.next, s.oc.maxSeq, openCodeBatch)
		if err != nil {
			s.gaps["read_error"]++
			s.oc.next = s.oc.maxSeq
			break
		}
		batch, stop := 0, false
		for rows.Next() && !stop {
			var seq, created, size int64
			var typ string
			var data sql.NullString
			if err := rows.Scan(&seq, &typ, &created, &size, &data); err != nil {
				s.gaps["read_error"]++
				s.oc.next = s.oc.maxSeq
				stop = true
				break
			}
			batch++
			s.oc.next = seq
			scanned += int(size)
			if !data.Valid {
				s.gaps["oversized_record"]++
				continue
			}
			s.sink.ts, s.sink.at = nil, nil
			if created > 0 {
				t := time.UnixMilli(created).UTC()
				s.sink.ts, s.sink.at = rfc3339(&t), &t
			}
			s.sink.offset = seq
			s.sink.events = nil
			s.oc.normalize(typ, data.String, s.sink)
			for i, ev := range s.sink.events {
				if !pg.place(ev) {
					s.carry = append(s.carry, s.sink.events[i:]...)
					break
				}
			}
			stop = len(s.carry) > 0 || len(pg.events) >= limit || scanned >= maxHistoryScan
		}
		rerr := rows.Err()
		rows.Close()
		if rerr != nil && ctx.Err() == nil {
			s.gaps["read_error"]++
			s.oc.next = s.oc.maxSeq
		}
		if batch == 0 {
			s.oc.next = s.oc.maxSeq // rows between seq values were removed; nothing left to read
		}
	}
	s.skips = s.sink.skips
	if ctx.Err() != nil && len(pg.events) == 0 && s.oc.next == start {
		return nil, false, herr("RESOURCE_LIMIT", "the request deadline or cancellation arrived before a page could be produced; retry", true)
	}
	return pg.events, s.oc.next >= s.oc.maxSeq && len(s.carry) == 0, nil
}

// openCodePeerMarker is the envelope Rhizome wraps around peer deliveries; the
// live collector refuses to republish text containing it.
const openCodePeerMarker = "<rhizome-peer-event>"

type openCodePart struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Text  string `json:"text"`
	State struct {
		Status string `json:"status"`
	} `json:"state"`
	Time struct {
		Created   int64 `json:"created"`
		Ran       int64 `json:"ran"`
		Completed int64 `json:"completed"`
	} `json:"time"`
}

type openCodeRecord struct {
	Text    string         `json:"text"`
	Content []openCodePart `json:"content"`
}

// normalize turns one session_message row into the same three event kinds the
// live collector publishes, under the same content policy: assistant text, tool
// names with a phase, and turn completion. Prompts, reasoning, tool inputs and
// outputs, and peer-only turns never appear.
func (c *openCodeCursor) normalize(typ, data string, sink *historySink) {
	switch typ {
	case "user":
		c.peer = false
		return
	case "synthetic":
		var r openCodeRecord
		if err := json.Unmarshal([]byte(data), &r); err != nil {
			sink.countSkip()
			return
		}
		if strings.Contains(r.Text, openCodePeerMarker) {
			c.peer = true
		}
		return
	case "idle":
		if !c.peer {
			sink.activity(1, "turn", "")
		}
		return
	case "assistant":
	default:
		return // system and unknown record types carry no assistant activity
	}
	var r openCodeRecord
	if err := json.Unmarshal([]byte(data), &r); err != nil {
		sink.countSkip()
		return
	}
	rowTS, rowAt := sink.ts, sink.at
	at := func(ms int64) {
		if ms > 0 {
			t := time.UnixMilli(ms).UTC()
			sink.ts, sink.at = rfc3339(&t), &t
		} else {
			sink.ts, sink.at = rowTS, rowAt
		}
	}
	for _, p := range r.Content {
		switch p.Type {
		case "text":
			if c.peer || strings.TrimSpace(p.Text) == "" || strings.Contains(p.Text, openCodePeerMarker) {
				continue
			}
			at(0)
			sink.activity(1, "activity", p.Text)
		case "tool":
			if c.peer || p.Name == "" || p.Name == "execute" || strings.HasPrefix(p.Name, "rhizome_") {
				continue
			}
			start := p.Time.Created
			if start == 0 {
				start = p.Time.Ran
			}
			at(start)
			sink.activity(1, "tool", "Tool started: "+p.Name)
			switch p.State.Status {
			case "completed":
				at(p.Time.Completed)
				sink.activity(1, "tool", "Tool completed: "+p.Name)
			case "error":
				at(p.Time.Completed)
				sink.activity(1, "tool", "Tool failed: "+p.Name)
			}
		}
	}
	sink.ts, sink.at = rowTS, rowAt
}
