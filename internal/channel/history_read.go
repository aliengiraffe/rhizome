package channel

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	pb "rhizome/proto"
)

// History reads use fixed EOF snapshots with their own budgets, separate from
// live retention and queue limits.
const (
	maxSnapshots         = 32
	snapshotTTL          = 15 * time.Minute
	maxHistoryScan       = 8 * 1024 * 1024
	maxHistoryResponse   = 1024 * 1024
	historyReserve       = 16 * 1024 // session, catalog, coverage and framing overhead
	historyDeadline      = 10 * time.Second
	maxHistoryConcurrent = 4
	maxCursorLength      = 512
	maxPeerSnapshots     = 8 // per paired host, so one peer cannot evict another's pagination
)

type historyEvent struct {
	ID        string  `json:"event_id"`
	Timestamp *string `json:"timestamp"`
	Kind      string  `json:"kind"`
	Text      *string `json:"text"`
	at        *time.Time
}

// historySink collects the live normalizer's output for one record without
// clipping, publishing, or touching any node state.
type historySink struct {
	identity *pb.Identity
	ts       *string
	at       *time.Time
	offset   int64
	events   []historyEvent
	skips    uint64
}

func (s *historySink) activity(_ uint64, kind, text string) {
	var k string
	switch kind {
	case "activity":
		k = "assistant_text"
	case "tool":
		k = "tool_summary"
	case "turn":
		k = "turn_completed"
	default:
		return
	}
	ev := historyEvent{ID: fmt.Sprintf("%x-%d", s.offset, len(s.events)), Timestamp: s.ts, Kind: k, at: s.at}
	if kind != "turn" {
		t := text
		ev.Text = &t
	}
	s.events = append(s.events, ev)
}
func (s *historySink) setNativeTitle(uint64, bool, string) {}
func (s *historySink) local() *pb.Identity                 { return s.identity }
func (s *historySink) countSkip()                          { s.skips++ }

// snapshot is bounded per-process pagination state. Search snapshots fix the
// ordered row set; read snapshots fix a source EOF boundary and parser state.
type snapshot struct {
	id, kind, query  string
	owner            string // authenticated paired host; "" for local MCP use
	access           *historyAccess
	created, expires time.Time
	page             int
	busy             bool
	keys             []sessionKey
	agent, session   string
	path             string
	info             os.FileInfo
	eof              int64
	anchor           []byte
	c                *collector
	sink             *historySink
	offset           int64
	carry            []historyEvent
	gaps             map[string]int
	skips            uint64
	oc               *openCodeCursor // OpenCode sessions page by message seq, not byte offset
}

type snapshotStore struct {
	mu      sync.Mutex
	key     []byte
	process string
	items   map[string]*snapshot
}

func newSnapshotStore() *snapshotStore {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return &snapshotStore{key: key, process: id()[:16], items: map[string]*snapshot{}}
}

// put registers s, evicting expired snapshots first and then the oldest one
// when the store is full. The caller holds s busy until release.
func (st *snapshotStore) put(s *snapshot) {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	for key, other := range st.items {
		if now.After(other.expires) && !other.busy {
			delete(st.items, key)
		}
	}
	evict := func(owned bool) bool {
		var oldest *snapshot
		count := 0
		for _, other := range st.items {
			if owned && other.owner != s.owner {
				continue
			}
			count++
			if !other.busy && (oldest == nil || other.created.Before(oldest.created)) {
				oldest = other
			}
		}
		if oldest == nil || owned && count < maxPeerSnapshots {
			return false
		}
		delete(st.items, oldest.id)
		return true
	}
	for s.owner != "" && evict(true) {
	}
	for len(st.items) >= maxSnapshots && evict(false) {
	}
	s.id = id()
	s.created = now
	s.expires = now.Add(snapshotTTL)
	s.busy = true
	st.items[s.id] = s
}

// take claims a snapshot for one page. Pagination is forward-only.
func (st *snapshotStore) take(id, kind, query string, page int) (*snapshot, *historyError) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.items[id]
	if s == nil || time.Now().After(s.expires) {
		return nil, herr("CURSOR_EXPIRED", "the pagination snapshot behind this cursor is no longer retained; run a fresh initial query", false)
	}
	if s.kind != kind || s.query != query {
		return nil, herr("INVALID_CURSOR", "cursor was issued for different query parameters or another tool", false)
	}
	if page != s.page {
		return nil, herr("INVALID_CURSOR", "cursor was already consumed; pagination is forward-only and each cursor is valid once", false)
	}
	if s.busy {
		return nil, herr("RESOURCE_LIMIT", "a page for this cursor is already being produced; retry", true)
	}
	s.busy = true
	return s, nil
}
func (st *snapshotStore) release(s *snapshot, drop bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s.busy = false
	if drop {
		delete(st.items, s.id)
	}
}

type cursorPayload struct {
	Process  string `json:"p"`
	Kind     string `json:"k"`
	Snapshot string `json:"s"`
	Page     int    `json:"n"`
	Query    string `json:"q"`
}

func (st *snapshotStore) sign(c cursorPayload) string {
	c.Process = st.process
	raw, _ := json.Marshal(c)
	body := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, st.key)
	mac.Write([]byte(body))
	return "v1." + body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
}

// verify authenticates a cursor and binds it to the expected tool and query.
func (st *snapshotStore) verify(cursor, kind, query string) (cursorPayload, *historyError) {
	var c cursorPayload
	invalid := herr("INVALID_CURSOR", "cursor is malformed or was not issued by this query; run a fresh initial query", false)
	if len(cursor) > maxCursorLength {
		return c, invalid
	}
	parts := strings.Split(cursor, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return c, invalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(raw, &c) != nil {
		return c, invalid
	}
	if c.Process != st.process {
		return c, herr("CURSOR_EXPIRED", "cursor was issued by another Rhizome process or before a restart; run a fresh initial query", false)
	}
	mac := hmac.New(sha256.New, st.key)
	mac.Write([]byte(parts[1]))
	sum, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sum, mac.Sum(nil)[:16]) {
		return c, invalid
	}
	if c.Kind != kind || c.Query != query {
		return c, invalid
	}
	return c, nil
}

func queryHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:16])
}

// confine re-checks that path is a regular, non-symlinked file inside the
// configured root for agent before every open. Catalog paths are never trusted
// after the fact.
func (s historyScope) confine(agent, path string) *historyError {
	owner, root := s.root(path)
	if owner != agent || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return &historyError{Code: "HISTORY_UNAVAILABLE", Message: "the recorded source lies outside the configured log roots", Extra: map[string]any{"reason": "source_outside_roots"}}
	}
	for p := path; p != root; p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			return &historyError{Code: "HISTORY_UNAVAILABLE", Message: "the native log for this session no longer exists", Extra: map[string]any{"reason": "source_missing"}}
		}
		if err != nil {
			return &historyError{Code: "HISTORY_UNAVAILABLE", Message: "the native log for this session is not readable", Retryable: true, Extra: map[string]any{"reason": "source_unreadable"}}
		}
		if st.Mode()&os.ModeSymlink != 0 || (p != path && !st.IsDir()) || (p == path && !st.Mode().IsRegular()) {
			return &historyError{Code: "HISTORY_UNAVAILABLE", Message: "the native log path is not a regular file inside the configured roots", Extra: map[string]any{"reason": "source_unsupported"}}
		}
	}
	return nil
}

// openSnapshot fixes the EOF boundary of a session's native log.
func (h *historyService) openSnapshot(ctx context.Context, agent, session, query string, src *indexedFile) (*snapshot, *historyError) {
	if agent == "opencode" {
		return h.openOpenCodeSnapshot(ctx, session, query, src)
	}
	if e := h.scope.confine(agent, src.Path); e != nil {
		return nil, e
	}
	f, err := os.Open(src.Path)
	if err != nil {
		return nil, &historyError{Code: "HISTORY_UNAVAILABLE", Message: "the native log for this session is not readable", Retryable: true, Extra: map[string]any{"reason": "source_unreadable"}}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, &historyError{Code: "HISTORY_UNAVAILABLE", Message: "the native log path is not a regular file", Extra: map[string]any{"reason": "source_unsupported"}}
	}
	s := &snapshot{kind: "read", query: query, agent: agent, session: session, path: src.Path, info: st, eof: st.Size(), gaps: map[string]int{}}
	size := int64(128)
	if s.eof < size {
		size = s.eof
	}
	s.anchor = make([]byte, size)
	if size > 0 {
		if _, err := f.ReadAt(s.anchor, s.eof-size); err != nil {
			return nil, &historyError{Code: "HISTORY_UNAVAILABLE", Message: "the native log for this session is not readable", Retryable: true, Extra: map[string]any{"reason": "source_unreadable"}}
		}
	}
	s.c = &collector{path: src.Path, agent: agent, seen: map[string]bool{}, tools: map[string]string{}}
	s.sink = &historySink{identity: &pb.Identity{SessionId: session, AgentType: agent, Generation: 1}}
	return s, nil
}

// pager places events into one response page under the count and byte budgets
// and the serving host's policy, for every agent's reader.
type pager struct {
	s      *snapshot
	limit  int
	used   int
	events []historyEvent
}

// place appends ev when it fits and reports whether it was consumed: withheld
// and oversized events are counted as gaps and consumed, so a page that is full
// returns false and the caller carries ev to the next page.
func (p *pager) place(ev historyEvent) bool {
	if p.s.access != nil {
		if code := p.s.access.event(p.s.agent, ev.at); code != "" {
			p.s.gaps[code]++
			return true
		}
	}
	raw, _ := json.Marshal(ev)
	size := len(raw) + 1
	budget := maxHistoryResponse - historyReserve
	if size > budget {
		p.s.gaps["event_too_large"]++
		return true
	}
	if len(p.events) >= p.limit || p.used+size > budget {
		return false
	}
	p.events = append(p.events, ev)
	p.used += size
	return true
}

// readPage emits the next permitted events from the snapshot in source order.
// It returns whether the snapshot is exhausted. Parser state advances with
// every consumed record, so events that do not fit are carried, never reparsed.
func (h *historyService) readPage(ctx context.Context, s *snapshot, limit int) ([]historyEvent, bool, *historyError) {
	if s.oc != nil {
		return h.readOpenCodePage(ctx, s, limit)
	}
	if e := h.scope.confine(s.agent, s.path); e != nil {
		return nil, false, e
	}
	f, err := os.Open(s.path)
	if err != nil {
		return nil, false, &historyError{Code: "HISTORY_UNAVAILABLE", Message: "the native log for this session is not readable", Retryable: true, Extra: map[string]any{"reason": "source_unreadable"}}
	}
	defer f.Close()
	changed := herr("SOURCE_CHANGED", "the native log was replaced, truncated or deleted since this snapshot was captured; run a fresh initial read", false)
	st, err := f.Stat()
	if err != nil || !os.SameFile(st, s.info) || st.Size() < s.eof {
		return nil, false, changed
	}
	if len(s.anchor) > 0 {
		b := make([]byte, len(s.anchor))
		if _, err := f.ReadAt(b, s.eof-int64(len(b))); err != nil || !bytes.Equal(b, s.anchor) {
			return nil, false, changed
		}
	}
	pg := &pager{s: s, limit: limit, events: []historyEvent{}}
	place := pg.place
	for len(s.carry) > 0 && place(s.carry[0]) {
		s.carry = s.carry[1:]
	}
	start := s.offset
	if len(s.carry) == 0 && s.offset < s.eof {
		r := bufio.NewReaderSize(io.NewSectionReader(f, s.offset, s.eof-s.offset), 64*1024)
		var record []byte
		var pending int64
		scanned := int64(0)
		dropping := false
		for scanned < maxHistoryScan && len(pg.events) < limit && len(s.carry) == 0 {
			if ctx.Err() != nil {
				break
			}
			chunk, err := r.ReadSlice('\n')
			pending += int64(len(chunk))
			if err != nil && err != bufio.ErrBufferFull {
				if err == io.EOF {
					if pending > 0 {
						s.gaps["incomplete_tail"]++
					}
					s.offset = s.eof
				} else {
					s.gaps["read_error"]++
					s.offset = s.eof
				}
				break
			}
			if !dropping {
				if len(record)+len(chunk) > maxRecord {
					dropping = true
					record = nil
					s.gaps["oversized_record"]++
				} else {
					record = append(record, chunk...)
				}
			}
			if err == bufio.ErrBufferFull {
				continue
			}
			lineStart := s.offset
			s.offset += pending
			scanned += pending
			pending = 0
			line := bytes.TrimRight(record, "\r\n")
			record = nil
			if dropping {
				dropping = false
				continue
			}
			if len(line) == 0 {
				continue
			}
			var meta struct {
				Timestamp string `json:"timestamp"`
			}
			s.sink.ts, s.sink.at = nil, nil
			if json.Unmarshal(line, &meta) == nil && meta.Timestamp != "" {
				if t, err := time.Parse(time.RFC3339Nano, meta.Timestamp); err == nil {
					t = t.UTC()
					s.sink.ts, s.sink.at = rfc3339(&t), &t
				}
			}
			s.sink.offset = lineStart
			s.sink.events = nil
			s.c.normalize(line, s.sink, 1)
			for i, ev := range s.sink.events {
				if !place(ev) {
					s.carry = append(s.carry, s.sink.events[i:]...)
					break
				}
			}
		}
	}
	s.skips = s.sink.skips
	if ctx.Err() != nil && len(pg.events) == 0 && s.offset == start {
		return nil, false, herr("RESOURCE_LIMIT", "the request deadline or cancellation arrived before a page could be produced; retry", true)
	}
	return pg.events, s.offset >= s.eof && len(s.carry) == 0, nil
}

func (s *snapshot) coverage() map[string]any {
	codes := make([]string, 0, len(s.gaps))
	for code := range s.gaps {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	gaps := make([]map[string]any, 0, len(codes))
	for _, code := range codes {
		gaps = append(gaps, map[string]any{"code": code, "count": s.gaps[code]})
	}
	state := "complete"
	if len(gaps) > 0 {
		state = "partial"
	}
	return map[string]any{"state": state, "gaps": gaps, "skipped_records": s.skips, "content_policy": "assistant_activity_only"}
}
