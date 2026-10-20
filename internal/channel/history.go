package channel

import (
	"crypto/sha256"
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

	"rhizome/internal/localstate"
)

// The session-history catalog is a rebuildable metadata cache written only by
// the observer service and read by MCP processes. Native logs stay the content
// source of truth; the catalog stores identity, bounded checkpoints, timestamps
// and titles, never transcript text.
const (
	catalogVersion     = 1
	catalogDir         = "history"
	catalogName        = "catalog.json"
	maxCatalogFiles    = 4096             // indexed source files (one per session in practice)
	maxCatalogBytes    = 32 * 1024 * 1024 // catalog file read bound
	maxCatalogWarnings = 8
	maxTombstones      = 512
	tombstoneTTL       = 30 * 24 * time.Hour
)

// historyScope binds a catalog and every cursor to the effective runtime and
// configured log roots. Entries from other roots are never served.
type historyScope struct{ Runtime, ClaudeRoot, CodexRoot string }

func (s historyScope) hash() string {
	sum := sha256.Sum256([]byte(s.Runtime + "\x00" + s.ClaudeRoot + "\x00" + s.CodexRoot))
	return hex.EncodeToString(sum[:])
}
func (s historyScope) path() string { return filepath.Join(s.Runtime, catalogDir, catalogName) }

// root returns the configured root containing path and its agent, or "" when
// the path lies outside every root. Prefix checks are lexical: callers must
// also refuse symlinks when opening.
func (s historyScope) root(path string) (agent, root string) {
	for _, item := range []struct{ root, agent string }{{s.ClaudeRoot, "claude"}, {s.CodexRoot, "codex"}} {
		if item.root != "" && strings.HasPrefix(path, item.root+string(os.PathSeparator)) {
			return item.agent, item.root
		}
	}
	return "", ""
}

type catalogWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// indexedFile is one native log inside a configured root. Path is private to
// the catalog file and never leaves the process in a tool response.
type indexedFile struct {
	Path        string     `json:"path"`
	Agent       string     `json:"agent"`
	Session     string     `json:"session,omitempty"`
	Cwd         string     `json:"cwd,omitempty"`
	Child       bool       `json:"child,omitempty"`
	Unsupported string     `json:"unsupported,omitempty"`
	Indexed     int64      `json:"indexed"`
	Anchor      string     `json:"anchor,omitempty"`
	Partial     []string   `json:"partial,omitempty"`
	FirstAt     *time.Time `json:"first_at,omitempty"`
	LastAt      *time.Time `json:"last_at,omitempty"`
	CustomTitle string     `json:"custom_title,omitempty"`
	AutoTitle   string     `json:"auto_title,omitempty"`
	MissingAt   *time.Time `json:"missing_at,omitempty"`
}

func (f *indexedFile) partial(reason string) {
	for _, r := range f.Partial {
		if r == reason {
			return
		}
	}
	f.Partial = append(f.Partial, reason)
}

type catalogFile struct {
	Version    int               `json:"version"`
	Scope      string            `json:"scope"`
	Roots      map[string]string `json:"roots"`
	Generation uint64            `json:"generation"`
	IndexedAt  *time.Time        `json:"indexed_at"`
	Inventory  bool              `json:"inventory_complete"`
	Pending    int               `json:"pending"`
	Degraded   bool              `json:"degraded"`
	Warnings   []catalogWarning  `json:"warnings"`
	Files      []*indexedFile    `json:"files"`
}

// historySession is the catalog view of one (agent_type, session_id) identity.
type historySession struct {
	SessionID, AgentType, ProjectPath, DisplayName string
	Custom, Auto                                   string
	StartedAt, LastActivityAt                      *time.Time
	Availability                                   string
	Reasons                                        []string
	sources                                        []*indexedFile
}

type sessionKey struct{ Agent, Session string }

// sessions groups indexed files into catalog identities. Files without a
// verified identity or excluded child transcripts are not sessions.
func (c *catalogFile) sessions() map[sessionKey]*historySession {
	out := map[sessionKey]*historySession{}
	for _, f := range c.Files {
		if f.Session == "" || f.Child {
			continue
		}
		key := sessionKey{f.Agent, f.Session}
		s := out[key]
		if s == nil {
			s = &historySession{SessionID: f.Session, AgentType: f.Agent}
			out[key] = s
		}
		s.sources = append(s.sources, f)
	}
	for _, s := range out {
		var present []*indexedFile
		for _, f := range s.sources {
			if f.MissingAt == nil {
				present = append(present, f)
			}
		}
		considered := present
		switch {
		case len(present) == 0:
			s.Availability = "missing"
			s.Reasons = []string{"source_missing"}
			considered = s.sources
		case len(present) > 1:
			s.Availability = "ambiguous"
			s.Reasons = []string{"duplicate_sources"}
		case present[0].Unsupported != "":
			s.Availability = "unsupported"
			s.Reasons = []string{present[0].Unsupported}
		case len(present[0].Partial) > 0:
			s.Availability = "partial"
			s.Reasons = append([]string(nil), present[0].Partial...)
		default:
			s.Availability = "available"
		}
		sort.Slice(considered, func(i, j int) bool { return considered[i].Path < considered[j].Path })
		for _, f := range considered {
			if f.Cwd != "" && s.ProjectPath == "" {
				s.ProjectPath = f.Cwd
			}
			if f.FirstAt != nil && (s.StartedAt == nil || f.FirstAt.Before(*s.StartedAt)) {
				t := *f.FirstAt
				s.StartedAt = &t
			}
			if f.LastAt != nil && (s.LastActivityAt == nil || f.LastAt.After(*s.LastActivityAt)) {
				t := *f.LastAt
				s.LastActivityAt = &t
			}
			if f.CustomTitle != "" {
				s.Custom = f.CustomTitle
			}
			if f.AutoTitle != "" {
				s.Auto = f.AutoTitle
			}
		}
		if s.ProjectPath != "" {
			s.DisplayName = clip(filepath.Base(s.ProjectPath), 40)
		}
	}
	return out
}

// source returns the single readable native file for a session, or nil.
func (s *historySession) source() *indexedFile {
	var present *indexedFile
	for _, f := range s.sources {
		if f.MissingAt == nil {
			if present != nil {
				return nil
			}
			present = f
		}
	}
	return present
}

// historyError is the stable JSON error payload of the history tools.
type historyError struct {
	Code      string
	Message   string
	Retryable bool
	Extra     map[string]any
}

func (e *historyError) Error() string { return e.Code + ": " + e.Message }
func herr(code, message string, retryable bool) *historyError {
	return &historyError{Code: code, Message: message, Retryable: retryable}
}

// catalogReader loads the observer's catalog for MCP queries. It never writes.
type catalogReader struct {
	scope  historyScope
	mu     sync.Mutex
	cached *catalogFile
	size   int64
	mod    time.Time
}

const startObserverHint = "No session-history catalog exists in this runtime directory. Start the observer service (rhizome service install, or rhizome service start) so it indexes local coding agent logs; MCP processes only read the catalog."

// load returns the current catalog and whether the observer service holds its
// runtime lock. Missing, corrupt, foreign-scope and newer catalogs are errors.
func (r *catalogReader) load() (*catalogFile, bool, *historyError) {
	running := observerRunning(r.scope.Runtime)
	path := r.scope.path()
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, running, herr("CATALOG_UNAVAILABLE", startObserverHint, true)
	}
	if err != nil {
		return nil, running, herr("CATALOG_UNAVAILABLE", "session-history catalog is not readable", true)
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return nil, running, herr("CATALOG_UNAVAILABLE", "session-history catalog must be a private regular file; remove it and restart the observer service", false)
	}
	if st.Size() > maxCatalogBytes {
		return nil, running, herr("CATALOG_INCOMPATIBLE", "session-history catalog exceeds the supported size; remove it and restart the observer service", false)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cached != nil && st.Size() == r.size && st.ModTime().Equal(r.mod) {
		return r.cached, running, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, running, herr("CATALOG_UNAVAILABLE", "session-history catalog is not readable", true)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(st, opened) {
		return nil, running, herr("CATALOG_UNAVAILABLE", "session-history catalog changed while opening; retry", true)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxCatalogBytes+1))
	if err != nil || len(raw) > maxCatalogBytes {
		return nil, running, herr("CATALOG_UNAVAILABLE", "session-history catalog is not readable", true)
	}
	var c catalogFile
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, running, herr("CATALOG_UNAVAILABLE", "session-history catalog is corrupt; the observer service rebuilds it on restart", true)
	}
	if c.Version != catalogVersion {
		return nil, running, herr("CATALOG_INCOMPATIBLE", fmt.Sprintf("session-history catalog schema %d is not supported by this binary (expected %d); upgrade or restart the observer service", c.Version, catalogVersion), false)
	}
	if c.Scope != r.scope.hash() {
		return nil, running, herr("CATALOG_INCOMPATIBLE", "session-history catalog was built for different runtime or log roots; restart the observer service with the current configuration", false)
	}
	r.cached, r.size, r.mod = &c, opened.Size(), opened.ModTime()
	return &c, running, nil
}

// observerRunning probes the observer's runtime lock without holding it.
func observerRunning(runtime string) bool {
	f, err := localstate.Lock(filepath.Join(runtime, "observer.lock"), false)
	if f != nil {
		f.Close()
	}
	return errors.Is(err, localstate.ErrBusy)
}

// catalogStatus is the `catalog` block of every successful history response.
type catalogStatus struct {
	State            string           `json:"state"`
	ServiceRunning   bool             `json:"service_running"`
	IndexedAt        *string          `json:"indexed_at"`
	CoverageComplete bool             `json:"coverage_complete"`
	Warnings         []catalogWarning `json:"warnings"`
}

func rfc3339(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}
func catalogState(c *catalogFile, running bool, extra ...catalogWarning) catalogStatus {
	s := catalogStatus{ServiceRunning: running, IndexedAt: rfc3339(c.IndexedAt), Warnings: []catalogWarning{}}
	add := func(code, message string) {
		if len(s.Warnings) < maxCatalogWarnings {
			s.Warnings = append(s.Warnings, catalogWarning{code, message})
		}
	}
	for _, w := range c.Warnings {
		add(w.Code, w.Message)
	}
	for _, w := range extra {
		add(w.Code, w.Message)
	}
	limited := len(c.Warnings) > 0
	switch {
	case !running:
		s.State = "stale"
		add("service_stopped", "The observer service is not running; results reflect the last indexed state and may be incomplete.")
	case !c.Inventory || c.Pending > 0:
		s.State = "indexing"
		if !c.Inventory {
			add("initial_inventory", "Initial log inventory is still running; sessions may be missing.")
		} else {
			add("pending_updates", "Recent log changes are still being indexed.")
		}
	case c.Degraded || limited:
		s.State = "degraded"
		if c.Degraded {
			add("watch_degraded", "Filesystem watch coverage is degraded; the observer reconciles periodically.")
		}
	default:
		s.State = "ready"
	}
	s.CoverageComplete = s.State == "ready"
	return s
}
