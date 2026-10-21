package channel

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	maxIndexQueue   = 8192
	indexPassBudget = 4 * 1024 * 1024 // bytes parsed per observer tick
	indexFlushEvery = time.Second
)

type historyCounters struct {
	FilesIndexed, BytesIndexed, ReadFailures, OversizedRecords, Removed, Flushes, FlushFailures uint64
}

// historyIndex is the observer's catalog writer. It runs on the observer's
// goroutine, parses only identity, timestamp and title fields, and never
// publishes activity, creates nodes, or touches collection leases.
type historyIndex struct {
	scope      historyScope
	files      map[string]*indexedFile
	queue      []string
	queued     map[string]bool
	seen       map[string]bool // paths enumerated by the initial inventory
	generation uint64
	indexedAt  *time.Time
	walked     bool // initial inventory walk finished
	reconciled bool
	dirty      bool
	lastFlush  time.Time
	limitHit   bool
	overflow   bool
	degraded   func() bool
	counters   historyCounters

	// OpenCode keeps sessions in one database, so it is refreshed by a metadata
	// query when the observer reports a change, not by walking files.
	ocDirty     bool
	ocNext      time.Time
	ocSchemaBad bool
}

func newHistoryIndex(scope historyScope, degraded func() bool) *historyIndex {
	h := &historyIndex{scope: scope, files: map[string]*indexedFile{}, queued: map[string]bool{}, seen: map[string]bool{}, degraded: degraded}
	if raw, err := os.ReadFile(scope.path()); err == nil && len(raw) <= maxCatalogBytes {
		var c catalogFile
		if json.Unmarshal(raw, &c) == nil && c.Version == catalogVersion && c.Scope == scope.hash() {
			h.generation = c.Generation
			for _, f := range c.Files {
				if agent, _ := scope.root(f.Path); agent == f.Agent && len(h.files) < maxCatalogFiles {
					h.files[f.Path] = f
				}
			}
		} else {
			slog.Warn("history_catalog_rebuild", "reason", "incompatible or corrupt catalog")
		}
	}
	return h
}

// enqueue schedules a regular .jsonl file inside a root for (re)indexing.
func (h *historyIndex) enqueue(path string) {
	if agent, _ := h.scope.root(path); agent == "" {
		return
	}
	if !h.walked {
		h.seen[path] = true
	}
	if h.queued[path] {
		return
	}
	if len(h.queue) >= maxIndexQueue {
		if !h.overflow {
			slog.Warn("history_index_queue_overflow")
		}
		h.overflow = true
		h.dirty = true
		return
	}
	h.queued[path] = true
	h.queue = append(h.queue, path)
}

// removed marks path (or every indexed file below it) missing. Metadata is
// retained as a bounded tombstone; content is never served from it.
func (h *historyIndex) removed(path string) {
	now := time.Now().UTC()
	for p, f := range h.files {
		if (p == path || strings.HasPrefix(p, path+string(os.PathSeparator))) && f.MissingAt == nil {
			f.MissingAt = &now
			h.counters.Removed++
			h.dirty = true
		}
	}
}

// walkDone records the end of the initial inventory walk. Files known from a
// previous run that were not enumerated are checked once and tombstoned.
func (h *historyIndex) walkDone() {
	if h.walked {
		return
	}
	h.walked = true
	for p, f := range h.files {
		if h.seen[p] || f.MissingAt != nil {
			continue
		}
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			h.removed(p)
		} else {
			h.enqueue(p)
		}
	}
	h.seen = nil
	h.dirty = true
}

// work indexes queued files within a byte budget, then flushes when due.
func (h *historyIndex) work(ctx context.Context) {
	budget := int64(indexPassBudget)
	for budget > 0 && len(h.queue) > 0 && ctx.Err() == nil {
		path := h.queue[0]
		used, more := h.indexFile(path, budget)
		budget -= used
		if more {
			break
		}
		h.queue = h.queue[1:]
		delete(h.queued, path)
	}
	if h.overflow && len(h.queue) == 0 {
		h.overflow = false // the next reconciliation inventory re-enumerates
	}
	h.indexOpenCode(ctx)
	if h.dirty && (len(h.queue) == 0 || time.Since(h.lastFlush) >= indexFlushEvery) {
		h.flush()
	}
}

func (h *historyIndex) indexFile(path string, budget int64) (int64, bool) {
	f := h.files[path]
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if f != nil {
			h.removed(path)
		}
		return 0, false
	}
	if err != nil {
		h.counters.ReadFailures++
		if f != nil {
			f.partial("read_error")
			h.dirty = true
		}
		return 0, false
	}
	agent, _ := h.scope.root(path)
	if f == nil {
		if len(h.files) >= maxCatalogFiles {
			if !h.limitHit {
				slog.Warn("history_index_limit", "files", len(h.files))
			}
			h.limitHit = true
			h.dirty = true
			return 0, false
		}
		f = &indexedFile{Path: path, Agent: agent}
		h.files[path] = f
		h.counters.FilesIndexed++
		h.dirty = true
	}
	if f.MissingAt != nil {
		f.MissingAt = nil
		h.dirty = true
	}
	if !st.Mode().IsRegular() {
		if f.Unsupported != "source_not_regular" {
			f.Unsupported = "source_not_regular"
			h.dirty = true
		}
		return 0, false
	}
	if f.Unsupported == "source_not_regular" {
		f.Unsupported = ""
		h.dirty = true
	}
	src, err := os.Open(path)
	if err != nil {
		h.counters.ReadFailures++
		f.partial("read_error")
		h.dirty = true
		return 0, false
	}
	defer src.Close()
	opened, err := src.Stat()
	if err != nil || !os.SameFile(st, opened) {
		return 0, true // changed under us; retry next pass
	}
	size := opened.Size()
	if f.Indexed > size || (f.Indexed > 0 && anchorHash(src, f.Indexed) != f.Anchor) {
		// Replaced or truncated: metadata derived from old bytes is discarded.
		*f = indexedFile{Path: path, Agent: agent}
		h.dirty = true
	}
	if f.Indexed == size {
		return 0, false
	}
	limit := size - f.Indexed
	if limit > budget {
		limit = budget
	}
	r := bufio.NewReaderSize(io.NewSectionReader(src, f.Indexed, size-f.Indexed), 64*1024)
	var total, pending int64
	var record []byte
	dropping, tail := false, false
	for total < limit {
		chunk, err := r.ReadSlice('\n')
		pending += int64(len(chunk))
		complete := err == nil
		if err != nil && err != bufio.ErrBufferFull {
			if err != io.EOF {
				h.counters.ReadFailures++
				f.partial("read_error")
			}
			tail = true // an incomplete tail waits for more bytes
			break
		}
		if !dropping {
			if len(record)+len(chunk) > maxRecord {
				dropping = true
				record = nil
				h.counters.OversizedRecords++
				f.partial("oversized_record")
			} else {
				record = append(record, chunk...)
			}
		}
		if complete {
			if !dropping && len(record) > 0 {
				h.indexLine(f, bytes.TrimRight(record, "\r\n"))
			}
			record = nil
			dropping = false
			f.Indexed += pending
			total += pending
			h.counters.BytesIndexed += uint64(pending)
			pending = 0
			h.dirty = true
		}
	}
	f.Anchor = anchorHash(src, f.Indexed)
	return total, f.Indexed < size && !tail
}

// anchorHash fingerprints the bytes just before offset so a replaced file
// with a shorter prefix is not mistaken for an append.
func anchorHash(f *os.File, offset int64) string {
	size := int64(128)
	if offset < size {
		size = offset
	}
	if size <= 0 {
		return ""
	}
	b := make([]byte, size)
	if _, err := f.ReadAt(b, offset-size); err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

type indexRecord struct {
	Type        string `json:"type"`
	Timestamp   string `json:"timestamp"`
	SessionID   string `json:"sessionId"`
	Cwd         string `json:"cwd"`
	Sidechain   bool   `json:"isSidechain"`
	AITitle     string `json:"aiTitle"`
	CustomTitle string `json:"customTitle"`
	Payload     struct {
		ID           string          `json:"id"`
		Session      string          `json:"session_id"`
		Cwd          string          `json:"cwd"`
		Source       json.RawMessage `json:"source"`
		ThreadSource json.RawMessage `json:"thread_source"`
		Parent       string          `json:"parent_thread_id"`
	} `json:"payload"`
}

// indexLine extracts identity, activity timestamps and titles from one record.
// It mirrors logIdentity: the first identity record is authoritative and child
// transcripts are excluded entirely.
func (h *historyIndex) indexLine(f *indexedFile, line []byte) {
	if f.Child {
		return
	}
	var r indexRecord
	if json.Unmarshal(line, &r) != nil {
		return
	}
	timed := false
	switch f.Agent {
	case "codex":
		if r.Type == "session_meta" {
			if f.Session == "" {
				if r.Payload.Parent != "" || strings.Contains(string(r.Payload.Source), "subagent") || strings.Contains(string(r.Payload.ThreadSource), "subagent") {
					f.Child = true
					return
				}
				id := r.Payload.ID
				if id == "" {
					id = r.Payload.Session
				}
				if id != "" && len(id) <= 128 {
					f.Session = id
					if filepath.IsAbs(r.Payload.Cwd) {
						f.Cwd = filepath.Clean(r.Payload.Cwd)
					}
				}
			}
			timed = true
		}
		timed = timed || r.Type == "response_item" || r.Type == "event_msg" || r.Type == "turn_context"
	default:
		if r.Sidechain && f.Session == "" {
			f.Child = true
			return
		}
		if r.Sidechain || (r.SessionID != "" && f.Session != "" && r.SessionID != f.Session) {
			return
		}
		if f.Session == "" && r.SessionID != "" && len(r.SessionID) <= 128 {
			f.Session = r.SessionID
			if filepath.IsAbs(r.Cwd) {
				f.Cwd = filepath.Clean(r.Cwd)
			}
		}
		switch r.Type {
		case "user", "assistant":
			timed = true
		case "ai-title":
			f.AutoTitle = cleanName(r.AITitle)
		case "custom-title":
			f.CustomTitle = cleanName(r.CustomTitle)
		}
		if f.Cwd == "" && filepath.IsAbs(r.Cwd) && r.SessionID == f.Session {
			f.Cwd = filepath.Clean(r.Cwd)
		}
	}
	if !timed || r.Timestamp == "" {
		return
	}
	t, err := time.Parse(time.RFC3339Nano, r.Timestamp)
	if err != nil {
		return
	}
	t = t.UTC()
	if f.FirstAt == nil || t.Before(*f.FirstAt) {
		first := t
		f.FirstAt = &first
	}
	if f.LastAt == nil || t.After(*f.LastAt) {
		last := t
		f.LastAt = &last
	}
}

func (h *historyIndex) warnings() []catalogWarning {
	var out []catalogWarning
	if h.limitHit {
		out = append(out, catalogWarning{"index_limit", "The indexed-file limit was reached; some sessions are not cataloged. Narrow the configured log roots."})
	}
	if h.overflow {
		out = append(out, catalogWarning{"queue_overflow", "Pending index work overflowed; coverage is reconciled on the next inventory."})
	}
	if h.ocSchemaBad {
		out = append(out, catalogWarning{"opencode_schema", "The OpenCode database does not have the 2.x session tables this build reads; its sessions are marked unsupported."})
	}
	if h.counters.ReadFailures > 0 {
		out = append(out, catalogWarning{"read_failures", "Some log files could not be read completely; affected sessions are marked partial."})
	}
	return out
}

// flush writes the catalog atomically. Tombstones are bounded by count and age,
// and a tombstone whose identity has a present source is dropped.
func (h *historyIndex) flush() {
	if err := secureDir(filepath.Join(h.scope.Runtime, catalogDir)); err != nil {
		h.counters.FlushFailures++
		slog.Warn("history_catalog_dir", "error", err)
		return
	}
	present := map[sessionKey]bool{}
	for _, f := range h.files {
		if f.MissingAt == nil && f.Session != "" {
			present[sessionKey{f.Agent, f.Session}] = true
		}
	}
	var tombstones []*indexedFile
	for p, f := range h.files {
		if f.MissingAt == nil {
			continue
		}
		if time.Since(*f.MissingAt) > tombstoneTTL || (f.Session != "" && present[sessionKey{f.Agent, f.Session}]) {
			delete(h.files, p)
			continue
		}
		tombstones = append(tombstones, f)
	}
	if len(tombstones) > maxTombstones {
		sort.Slice(tombstones, func(i, j int) bool { return tombstones[i].MissingAt.Before(*tombstones[j].MissingAt) })
		for _, f := range tombstones[:len(tombstones)-maxTombstones] {
			delete(h.files, f.Path)
		}
	}
	files := make([]*indexedFile, 0, len(h.files))
	for _, f := range h.files {
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	now := time.Now().UTC()
	h.generation++
	h.indexedAt = &now
	c := catalogFile{Version: catalogVersion, Scope: h.scope.hash(), Roots: map[string]string{"claude": h.scope.ClaudeRoot, "codex": h.scope.CodexRoot, "opencode": h.scope.OpenCodeRoot}, Generation: h.generation, IndexedAt: &now, Inventory: h.walked && len(h.queue) == 0 && !h.ocDirty, Pending: len(h.queue) + h.openCodePending(), Degraded: h.degraded != nil && h.degraded(), Warnings: h.warnings(), Files: files}
	if c.Warnings == nil {
		c.Warnings = []catalogWarning{}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		h.counters.FlushFailures++
		return
	}
	path := h.scope.path()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		h.counters.FlushFailures++
		slog.Warn("history_catalog_write", "error", err)
		return
	}
	h.counters.Flushes++
	h.dirty = false
	h.lastFlush = time.Now()
}

// health reports safe counters for observer-health.json and service status.
func (h *historyIndex) health() map[string]any {
	sessions := 0
	for _, f := range h.files {
		if f.Session != "" && !f.Child && f.MissingAt == nil {
			sessions++
		}
	}
	state := "indexing"
	switch {
	case h.walked && len(h.queue) == 0 && !h.ocDirty && (h.limitHit || h.overflow || h.ocSchemaBad || (h.degraded != nil && h.degraded())):
		state = "degraded"
	case h.walked && len(h.queue) == 0 && !h.ocDirty:
		state = "ready"
	}
	return map[string]any{"state": state, "indexed_files": len(h.files), "sessions": sessions, "pending": len(h.queue) + h.openCodePending(), "indexed_at": h.indexedAt, "coverage_complete": state == "ready", "generation": h.generation, "warnings": h.warnings(), "counters": h.counters}
}
