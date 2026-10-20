package channel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// resolveLocation returns the symlink-resolved absolute directory and its
// SHA-256 hex digest. Both the bridge socket and the binding store use it.
func resolveLocation(dir string) (string, string, error) {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", "", err
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(abs))
	return abs, hex.EncodeToString(sum[:]), nil
}

// storedBinding is one previously bound top-level OpenCode session.
type storedBinding struct {
	SessionID string `json:"session_id"`
	Last      int64  `json:"last"` // unix seconds of the last bind, activity or tool call
}

type bindingRecord struct {
	Directory string          `json:"directory"`
	Sessions  []storedBinding `json:"sessions"`
}

// openCodeStore persists, per OpenCode location, the top-level sessions that a
// human prompt bound, so a fresh plugin instance (OpenCode reloads instances)
// can restore them. It lives at <runtime>/opencode-bindings/<hash>.json, mode
// 0600. Changes are buffered in memory and merged into the file by flush under
// an exclusive flock, because an exiting and a starting Rhizome process for the
// same location can overlap. It is bounded to maxOpenCodeNodes entries, newest
// first, and drops entries idle longer than openCodeIdleTTL.
type openCodeStore struct {
	dir, path, lock string
	now             func() time.Time
	mu              sync.Mutex // guards upsert and remove only; never held across I/O
	upsert          map[string]time.Time
	remove          map[string]bool
	ioMu            sync.Mutex // serializes flushes within this process
}

// newOpenCodeStore opens the store for one OpenCode process (pid, the same
// pairing key as the bridge socket) and location. An instance reload stays in
// the same OpenCode process, so its sessions restore; another OpenCode process
// in the same directory never sees them. Files of dead OpenCode processes are
// removed.
func newOpenCodeStore(runtime string, pid int, location string) (*openCodeStore, error) {
	abs, hash, err := resolveLocation(location)
	if err != nil {
		return nil, err
	}
	base := filepath.Join(runtime, "opencode-bindings")
	if err := secureDir(base); err != nil {
		return nil, err
	}
	removeDeadStores(base, pid)
	name := fmt.Sprintf("opencode-%d-%s", pid, hash[:32])
	return &openCodeStore{dir: abs, path: filepath.Join(base, name+".json"), lock: filepath.Join(base, name+".lock"),
		now: time.Now, upsert: map[string]time.Time{}, remove: map[string]bool{}}, nil
}

var storeName = regexp.MustCompile(`^opencode-([0-9]+)-[0-9a-f]{32}\.(json|lock|json\.tmp)$`)

// removeDeadStores deletes store files whose OpenCode process no longer exists.
// The current process's files are never touched.
func removeDeadStores(base string, self int) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	removed := 0
	for _, e := range entries {
		m := storeName.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		pid, err := strconv.Atoi(m[1])
		if err != nil || pid == self || pid <= 0 || processAlive(pid) {
			continue
		}
		if os.Remove(filepath.Join(base, e.Name())) == nil {
			removed++
		}
	}
	if removed > 0 {
		slog.Info("opencode_store_cleanup", "removed", removed)
	}
}

// processAlive reports whether pid exists; EPERM means it exists as another user.
func processAlive(pid int) bool {
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}

// touch records that sid is bound with the given last-activity time.
func (s *openCodeStore) touch(sid string, last time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.remove, sid)
	if last.After(s.upsert[sid]) {
		s.upsert[sid] = last
	}
	s.mu.Unlock()
}

// drop records that sid must not be restored again.
func (s *openCodeStore) drop(sid string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.upsert, sid)
	s.remove[sid] = true
	s.mu.Unlock()
}

func (s *openCodeStore) dirty() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.upsert)+len(s.remove) > 0
}

func (s *openCodeStore) locked(how int, f func() error) error {
	l, err := os.OpenFile(s.lock, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer l.Close()
	if err := unix.Flock(int(l.Fd()), how); err != nil {
		return err
	}
	defer unix.Flock(int(l.Fd()), unix.LOCK_UN)
	return f()
}

// read returns the stored entries keyed by session ID. A missing, corrupt or
// foreign file reads as empty.
func (s *openCodeStore) read() map[string]int64 {
	out := map[string]int64{}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return out
	}
	var r bindingRecord
	if json.Unmarshal(raw, &r) != nil || r.Directory != s.dir {
		return out
	}
	for _, b := range r.Sessions {
		if openCodeSession.MatchString(b.SessionID) && b.Last > out[b.SessionID] {
			out[b.SessionID] = b.Last
		}
	}
	return out
}

// bounded drops expired entries and keeps the newest maxOpenCodeNodes.
func (s *openCodeStore) bounded(m map[string]int64) []storedBinding {
	cutoff := s.now().Add(-openCodeIdleTTL).Unix()
	out := make([]storedBinding, 0, len(m))
	for sid, last := range m {
		if last >= cutoff {
			out = append(out, storedBinding{SessionID: sid, Last: last})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Last != out[j].Last {
			return out[i].Last > out[j].Last
		}
		return out[i].SessionID < out[j].SessionID
	})
	if len(out) > maxOpenCodeNodes {
		out = out[:maxOpenCodeNodes]
	}
	return out
}

// load returns the restorable sessions, newest first.
func (s *openCodeStore) load() ([]storedBinding, error) {
	if s == nil {
		return nil, nil
	}
	var out []storedBinding
	err := s.locked(unix.LOCK_SH, func() error { out = s.bounded(s.read()); return nil })
	return out, err
}

// flush merges buffered changes into the file atomically (temp file and
// rename). On failure the changes are kept for the next flush unless newer
// ones replaced them.
func (s *openCodeStore) flush() error {
	if s == nil {
		return nil
	}
	s.ioMu.Lock()
	defer s.ioMu.Unlock()
	s.mu.Lock()
	up, rm := s.upsert, s.remove
	s.upsert, s.remove = map[string]time.Time{}, map[string]bool{}
	s.mu.Unlock()
	if len(up)+len(rm) == 0 {
		return nil
	}
	err := s.locked(unix.LOCK_EX, func() error {
		m := s.read()
		for sid := range rm {
			delete(m, sid)
		}
		for sid, last := range up {
			if t := last.Unix(); t > m[sid] {
				m[sid] = t
			}
		}
		entries := s.bounded(m)
		if len(entries) == 0 {
			if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		raw, err := json.Marshal(bindingRecord{Directory: s.dir, Sessions: entries})
		if err != nil {
			return err
		}
		tmp := s.path + ".tmp"
		_ = os.Remove(tmp) // WriteFile keeps an existing file's mode
		if err := os.WriteFile(tmp, raw, 0600); err != nil {
			return err
		}
		if err := os.Rename(tmp, s.path); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		return nil
	})
	if err != nil {
		s.mu.Lock()
		for sid, last := range up {
			if _, newer := s.upsert[sid]; !newer && !s.remove[sid] {
				s.upsert[sid] = last
			}
		}
		for sid := range rm {
			if _, newer := s.upsert[sid]; !newer {
				s.remove[sid] = true
			}
		}
		s.mu.Unlock()
		return fmt.Errorf("persist OpenCode bindings: %w", err)
	}
	return nil
}
