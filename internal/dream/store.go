package dream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"rhizome/internal/channel"
	"runtime"
	"sort"
	"strings"
	"time"

	"rhizome/internal/localstate"
)

const maxLogBytes = 32 << 20

// Revision is an append-only operation set and its validated materialized state.
// Rollback records the target revision instead of asking the model for operations.
type Revision struct {
	Version  int       `json:"version"`
	Revision int       `json:"revision"`
	At       time.Time `json:"at"`
	Since    time.Time `json:"since"`
	// Cursors resume partially read sessions; see channel.DreamProgress.
	Cursors    map[string]time.Time `json:"cursors,omitempty"`
	Rollback   int                  `json:"rollback,omitempty"`
	Operations []Operation          `json:"operations"`
	Items      []Item               `json:"items"`
	Added      int                  `json:"added"`
	Updated    int                  `json:"updated"`
	Removed    int                  `json:"removed"`
}

// Store addresses the private user store. The log is authoritative; Markdown is a cache.
type Store struct{ Runtime string }

func (s Store) root() string { return filepath.Join(s.Runtime, "memory") }
func (s Store) user() string { return filepath.Join(s.root(), "user") }
func checkDir(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() || runtime.GOOS != "windows" && st.Mode().Perm()&0077 != 0 {
		return errors.New("memory directory must be private and not a symlink")
	}
	return nil
}
func (s Store) check() error {
	for _, p := range []string{s.Runtime, s.root(), s.user()} {
		if err := checkDir(p); err != nil {
			return err
		}
	}
	return nil
}

// Load reads a bounded private log, without creating directories or lock files.
func (s Store) Load() ([]Revision, error) {
	if err := s.check(); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, errors.New("unsafe memory directory")
	}
	path := filepath.Join(s.user(), "log.jsonl")
	if err := localstate.PrivateFile(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, errors.New("unsafe memory log")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot read memory log")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxLogBytes+1))
	if err != nil || len(raw) > maxLogBytes {
		return nil, errors.New("memory log exceeds read limit")
	}
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		return nil, errors.New("incomplete memory revision")
	}
	var revisions []Revision
	for _, line := range bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var rev Revision
		if strict(line, &rev) != nil || rev.Version != 1 || rev.Revision != len(revisions)+1 || len(rev.Items) > 200 || len(rev.Operations) > 64 {
			return nil, errors.New("invalid memory revision")
		}
		seen := map[string]bool{}
		for _, it := range rev.Items {
			if !slug.MatchString(it.ID) || seen[it.ID] || it.Scope != "user" || !slug.MatchString(it.Topic) || !clean(it.Text, 1024) || len(it.Sources) == 0 || len(it.Sources) > 64 || it.EvidenceCount != len(it.Sources) {
				return nil, errors.New("invalid stored memory item")
			}
			for _, src := range it.Sources {
				if !validSource(src) {
					return nil, errors.New("invalid stored memory provenance")
				}
			}
			seen[it.ID] = true
		}
		revisions = append(revisions, rev)
	}
	return revisions, nil
}

// Lock excludes concurrent dream/rollback writers. Dry runs never create state;
// when no lock exists they use an immutable read snapshot and never commit it.
func (s Store) Lock(dry bool) (func(), error) {
	path := filepath.Join(s.root(), "dream.lock")
	if dry {
		for _, dir := range []string{s.Runtime, s.root()} {
			if err := checkDir(dir); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return func() {}, nil
				}
				return nil, errors.New("unsafe memory directory")
			}
		}
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return func() {}, nil
		}
	} else {
		for _, p := range []string{s.Runtime, s.root()} {
			if err := localstate.SecureDir(p); err != nil {
				return nil, errors.New("cannot create private dream directory")
			}
		}
	}
	if err := localstate.PrivateFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("unsafe dream lock")
	}
	f, err := localstate.Lock(path, false)
	if err != nil {
		return nil, errors.New("dream lock unavailable; another dream may be running")
	}
	return func() { f.Close() }, nil
}
func atomicFile(path string, body []byte) error {
	if err := localstate.PrivateFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".dream-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// Commit atomically replaces the log with its exact old bytes plus one revision.
// Markdown files are individually atomic caches; Load/show always replay the log.
func (s Store) Commit(previous []Revision, rev Revision) error {
	if err := localstate.SecureDir(s.user()); err != nil {
		return errors.New("cannot create private user memory")
	}
	topics := filepath.Join(s.user(), "topics")
	if err := localstate.SecureDir(topics); err != nil {
		return errors.New("cannot create private memory topics")
	}
	rev.Version = 1
	rev.Revision = len(previous) + 1
	var raw []byte
	path := filepath.Join(s.user(), "log.jsonl")
	if len(previous) > 0 {
		var err error
		raw, err = os.ReadFile(path)
		if err != nil {
			return errors.New("cannot read memory log")
		}
	}
	b, err := json.Marshal(rev)
	if err != nil {
		return errors.New("cannot encode memory revision")
	}
	raw = append(raw, b...)
	raw = append(raw, '\n')
	if len(raw) > maxLogBytes {
		return errors.New("memory log full; no revision applied")
	}
	// Stage all caches before committing the authoritative log. Failures can leave
	// stale derived Markdown, but cannot expose an uncommitted revision to show.
	byTopic := map[string][]Item{}
	for _, it := range rev.Items {
		byTopic[it.Topic] = append(byTopic[it.Topic], it)
	}
	names := make([]string, 0, len(byTopic))
	for name := range byTopic {
		names = append(names, name)
	}
	sort.Strings(names)
	var index strings.Builder
	for _, name := range names {
		fmt.Fprintf(&index, "- [%s](topics/%s.md)\n", name, name)
		if err := atomicFile(filepath.Join(topics, name+".md"), []byte(Render(byTopic[name], ""))); err != nil {
			return errors.New("cannot write memory topic; log unchanged")
		}
	}
	if err := atomicFile(filepath.Join(s.user(), "MEMORY.md"), []byte(index.String())); err != nil {
		return errors.New("cannot write memory index; log unchanged")
	}
	entries, err := os.ReadDir(topics)
	if err != nil {
		return errors.New("cannot read memory topics")
	}
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".md")
		if ok && slug.MatchString(name) && byTopic[name] == nil {
			if err := os.Remove(filepath.Join(topics, entry.Name())); err != nil {
				return errors.New("cannot remove obsolete memory topic")
			}
		}
	}
	if err := atomicFile(path, raw); err != nil {
		return errors.New("cannot commit memory revision")
	}
	return nil
}

// Render prints materialized items with their evidence, deterministically.
func Render(items []Item, topic string) string {
	var out strings.Builder
	for _, it := range items {
		if topic != "" && topic != it.Topic {
			continue
		}
		fmt.Fprintf(&out, "## %s / %s\n\n%s\n\n", it.Topic, it.ID, it.Text)
		for _, src := range it.Sources {
			fmt.Fprintf(&out, "- %s %s %s\n", src.Agent, src.SessionID, src.At.UTC().Format(time.RFC3339Nano))
		}
		out.WriteByte('\n')
	}
	return out.String()
}
func current(revs []Revision) ([]Item, channel.DreamProgress) {
	if len(revs) == 0 {
		return nil, channel.DreamProgress{}
	}
	r := revs[len(revs)-1]
	return r.Items, channel.DreamProgress{Since: r.Since, Cursors: r.Cursors}
}
func counts(old, new []Item) (add, update, remove int) {
	m := map[string]Item{}
	for _, it := range old {
		m[it.ID] = it
	}
	for _, it := range new {
		prior, ok := m[it.ID]
		if !ok {
			add++
		} else {
			a, _ := json.Marshal(prior)
			b, _ := json.Marshal(it)
			if !bytes.Equal(a, b) {
				update++
			}
		}
		delete(m, it.ID)
	}
	return add, update, len(m)
}

// Rollback appends a revision restoring exact item values, including timestamps.
func (s Store) Rollback(target int, now time.Time) error {
	unlock, err := s.Lock(false)
	if err != nil {
		return err
	}
	defer unlock()
	revs, err := s.Load()
	if err != nil {
		return err
	}
	if target < 1 || target > len(revs) {
		return errors.New("unknown dream revision")
	}
	old, progress := current(revs)
	items := revs[target-1].Items
	a, u, r := counts(old, items)
	return s.Commit(revs, Revision{At: now.UTC(), Since: progress.Since, Cursors: progress.Cursors, Rollback: target, Operations: []Operation{}, Items: items, Added: a, Updated: u, Removed: r})
}
