package channel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"rhizome/internal/localstate"
)

// ObserverOptions configures read-only discovery. Roots contain agent logs, not
// arbitrary user files. Runtime must match integrated Rhizome processes.
type ObserverOptions struct {
	Runtime, ClaudeRoot, CodexRoot, OpenCodeRoot, Hostname string
	// Details is [sessions].share_prompts_and_tools: observer publishers and
	// the gateway's history service publish prompts and tool inputs/outputs.
	Details bool
}
type observedFile struct {
	startupBaseline            bool
	c                          *collector
	agent, session, cwd, epoch string
	n                          *Node
	last                       time.Time
	blocked                    bool
	identityError              string
}
type observer struct {
	ctx   context.Context
	opts  ObserverOptions
	watch *fsnotify.Watcher
	files map[string]*observedFile
	dirs  map[string]bool
	retry bool
	index *historyIndex // catalog writer; nil only in harnesses that skip indexing
}

// RunObserver observes local log changes until cancellation. Only one observer
// may run in a runtime scope. It never starts or modifies an agent.
func RunObserver(ctx context.Context, opts ObserverOptions) error {
	var err error
	for _, root := range []*string{&opts.ClaudeRoot, &opts.CodexRoot, &opts.OpenCodeRoot} {
		if *root != "" {
			*root, err = filepath.Abs(*root)
			if err != nil {
				return err
			}
		}
	}
	if opts.ClaudeRoot != "" && opts.CodexRoot != "" && (opts.ClaudeRoot == opts.CodexRoot || strings.HasPrefix(opts.ClaudeRoot, opts.CodexRoot+string(os.PathSeparator)) || strings.HasPrefix(opts.CodexRoot, opts.ClaudeRoot+string(os.PathSeparator))) {
		return errors.New("agent log roots must not overlap")
	}
	opts.Runtime, err = localstate.Runtime(opts.Runtime)
	if err != nil {
		return err
	}
	if err = localstate.SecureDir(opts.Runtime); err != nil {
		return err
	}
	lock, err := localstate.Lock(filepath.Join(opts.Runtime, "observer.lock"), false)
	if err != nil {
		return err
	}
	defer lock.Close()
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	o := &observer{ctx: ctx, opts: opts, watch: w, files: map[string]*observedFile{}, dirs: map[string]bool{}}
	o.index = newHistoryIndex(historyScope{Runtime: opts.Runtime, ClaudeRoot: opts.ClaudeRoot, CodexRoot: opts.CodexRoot, OpenCodeRoot: opts.OpenCodeRoot}, func() bool { return o.retry })
	defer func() {
		for _, f := range o.files {
			if f.n != nil {
				f.n.close()
			}
		}
		// The catalog stays readable (visibly stale) after shutdown; only health is removed.
		if o.index.dirty {
			o.index.flush()
		}
		os.Remove(filepath.Join(opts.Runtime, "observer-health.json"))
	}()
	o.inventory(true)
	o.index.walkDone()
	o.health()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastRetry := time.Now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-w.Events:
			if !ok {
				return errors.New("filesystem event channel closed")
			}
			if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				for dir := range o.dirs {
					if dir == event.Name || strings.HasPrefix(dir, event.Name+string(os.PathSeparator)) {
						_ = w.Remove(dir)
						delete(o.dirs, dir)
					}
				}
				if f := o.files[event.Name]; f != nil && f.n != nil {
					f.n.activity(1, "gap", "Observed log removed or renamed; awaiting replacement")
				}
				o.index.removed(event.Name)
			}
			if event.Op&fsnotify.Create != 0 {
				for path, f := range o.files {
					if f.startupBaseline && (path == event.Name || strings.HasPrefix(path, event.Name+string(os.PathSeparator))) {
						f.c.offset = 0
						f.c.anchor = nil
						f.c.dropping = false
						f.c.peerResponse = false
						f.startupBaseline = false
					}
				}
			}
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename|fsnotify.Remove) != 0 {
				// Inventory only the affected subtree, never the whole tree on a timer.
				o.visit(event.Name, false)
				for _, root := range []string{opts.ClaudeRoot, opts.CodexRoot} {
					if root != "" && (event.Name == root || strings.HasPrefix(root, event.Name+string(os.PathSeparator))) {
						o.addRoot(root, false)
					}
				}
				o.openCodeEvent(event.Name)
			}
		case err, ok := <-w.Errors:
			if !ok {
				return errors.New("filesystem error channel closed")
			}
			o.watchFailed(err)
		case <-tick.C:
			if o.retry && time.Since(lastRetry) >= 30*time.Second {
				o.retry = false
				o.inventory(false)
				lastRetry = time.Now()
			}
			for path, f := range o.files {
				if f.n != nil && time.Since(f.last) > 10*time.Minute {
					f.n.close()
					f.n = nil
				}
				// Bounded continuation of an event-triggered read, not polling unchanged files.
				if f.c.more {
					o.scan(path, f)
				}
			}
			// Catalog backfill and watch-driven updates share this tick with a byte budget.
			o.index.work(ctx)
			o.health()
		}
	}
}
func (o *observer) watchFailed(err error) {
	slog.Warn("observer_watch_gap", "error", err)
	o.retry = true
	for _, f := range o.files {
		if f.n != nil {
			f.n.activity(1, "gap", "Filesystem notifications lost; reconciling log offsets")
		}
	}
}
func (o *observer) health() {
	active := 0
	metadataPending := 0
	var skips, suppressed uint64
	for _, f := range o.files {
		if f.identityError != "" {
			metadataPending++
		}
		if f.n != nil {
			active++
			f.n.mu.Lock()
			skips += f.n.stats.ParserSkips
			suppressed += f.n.stats.Suppressed
			f.n.mu.Unlock()
		}
	}
	health := map[string]any{"updated_at": time.Now().UTC(), "observed_sessions": active, "watched_directories": len(o.dirs), "tracked_files": len(o.files), "degraded": o.retry, "parser_skips": skips, "suppressed_events": suppressed, "metadata_unavailable": metadataPending}
	if o.index != nil {
		health["catalog"] = o.index.health()
	}
	b, _ := json.Marshal(health)
	path := filepath.Join(o.opts.Runtime, "observer-health.json")
	if err := os.WriteFile(path, b, 0600); err != nil {
		slog.Warn("observer_health_write", "error", err)
	}
}
func (o *observer) inventory(initial bool) {
	for _, root := range []string{o.opts.ClaudeRoot, o.opts.CodexRoot} {
		if root != "" {
			o.addRoot(root, initial)
		}
	}
	o.addOpenCodeRoot()
}

// addOpenCodeRoot watches the OpenCode data directory itself, not its subtree:
// the session database and its write-ahead log live directly inside it, and its
// other contents (logs, snapshots) must never wake the indexer. It also asks the
// index to refresh, so a reconciliation inventory re-reads the database.
func (o *observer) addOpenCodeRoot() {
	root := o.opts.OpenCodeRoot
	if root == "" || o.index == nil {
		return
	}
	for parent := filepath.Dir(root); ; {
		if st, err := os.Stat(parent); err == nil && st.IsDir() {
			o.addDir(parent) // nearest existing parent, so a missing root can appear later
			break
		}
		next := filepath.Dir(parent)
		if next == parent {
			break
		}
		parent = next
	}
	if st, err := os.Lstat(root); err == nil && st.IsDir() {
		o.addDir(root)
	}
	o.index.openCodeChanged()
}

// openCodeEvent reacts to a filesystem event in or around the data directory.
func (o *observer) openCodeEvent(name string) {
	root := o.opts.OpenCodeRoot
	if root == "" || o.index == nil {
		return
	}
	switch {
	case name == root || strings.HasPrefix(root, name+string(os.PathSeparator)):
		o.addOpenCodeRoot()
	case filepath.Dir(name) == root:
		switch filepath.Base(name) {
		case openCodeDBName, openCodeDBName + "-wal":
			o.index.openCodeChanged()
		}
	}
}
func (o *observer) addRoot(root string, initial bool) {
	// Watch the nearest existing parent so missing roots can appear later.
	parent := filepath.Dir(root)
	for {
		if st, err := os.Stat(parent); err == nil && st.IsDir() {
			o.addDir(parent)
			break
		}
		next := filepath.Dir(parent)
		if next == parent {
			break
		}
		parent = next
	}
	o.visit(root, initial)
}
func (o *observer) addDir(path string) {
	if o.dirs[path] {
		return
	}
	if len(o.dirs) >= 4096 {
		o.problem("directory limit reached")
		return
	}
	if err := o.watch.Add(path); err != nil {
		o.problem("watch registration failed")
		return
	}
	o.dirs[path] = true
}
func (o *observer) problem(reason string) {
	if !o.retry {
		slog.Warn("observer_degraded", "reason", reason)
	}
	o.retry = true
}
func (o *observer) agent(path string) string {
	for _, item := range []struct{ root, agent string }{{o.opts.ClaudeRoot, "claude"}, {o.opts.CodexRoot, "codex"}} {
		if item.root != "" && (path == item.root || strings.HasPrefix(path, item.root+string(os.PathSeparator))) {
			return item.agent
		}
	}
	return ""
}
func (o *observer) visit(path string, initial bool) {
	if o.agent(path) == "" {
		return
	}
	_ = filepath.WalkDir(path, func(path string, d os.DirEntry, err error) error {
		if o.ctx.Err() != nil {
			return o.ctx.Err()
		}
		if err != nil {
			if !os.IsNotExist(err) {
				o.problem("log path inaccessible")
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			if d.Name() == "subagents" {
				return filepath.SkipDir
			}
			o.addDir(path)
			if len(o.dirs) >= 4096 {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		if o.index != nil {
			o.index.enqueue(path)
		}
		if f := o.files[path]; f != nil {
			if !initial {
				o.scan(path, f)
			}
			return nil
		}
		if len(o.files) >= 1024 {
			o.problem("tracked file limit reached")
			return nil
		}
		c, err := newCollector(path)
		if err != nil {
			o.problem("log open failed")
			return nil
		}
		c.agent = o.agent(path)
		c.passive = true
		c.peerResponse = initial
		// No history is emitted for startup files. New files begin at byte zero,
		// including bytes written before directory registration completed.
		if !initial {
			c.offset = 0
			c.anchor = nil
			c.dropping = false
		}
		f := &observedFile{c: c, agent: c.agent, last: time.Now(), startupBaseline: initial}
		o.files[path] = f
		if initial {
			if session, cwd, err := logIdentity(path, f.agent); err == nil {
				f.session = session
				f.cwd = cwd
				lockPath, epochPath, err := ownershipPaths(o.opts.Runtime, session)
				if err != nil {
					o.problem("startup ownership unavailable")
					return nil
				}
				lock, err := localstate.Lock(lockPath, false)
				if err != nil && !errors.Is(err, localstate.ErrBusy) {
					o.problem("startup ownership unavailable")
					return nil
				}
				f.blocked = errors.Is(err, localstate.ErrBusy)
				epoch, _ := os.ReadFile(epochPath)
				f.epoch = string(epoch)
				// Capture EOF under the same ownership fence as live scans.
				fresh, err := newCollector(path)
				if lock != nil {
					lock.Close()
				}
				if err != nil {
					o.problem("startup baseline unavailable")
					return nil
				}
				fresh.agent = f.agent
				fresh.passive = true
				fresh.peerResponse = true
				f.c = fresh
			}
		}
		if !initial {
			o.scan(path, f)
		}
		return nil
	})
}
func logIdentity(path, agent string) (string, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	s := bufio.NewScanner(io.LimitReader(f, 2*maxRecord))
	s.Buffer(make([]byte, 4096), maxRecord)
	for s.Scan() {
		var r struct {
			Type    string `json:"type"`
			Session string `json:"sessionId"`
			Cwd     string `json:"cwd"`
			Child   bool   `json:"isSidechain"`
			Payload struct {
				ID           string          `json:"id"`
				Session      string          `json:"session_id"`
				Cwd          string          `json:"cwd"`
				Source       json.RawMessage `json:"source"`
				ThreadSource json.RawMessage `json:"thread_source"`
				Parent       string          `json:"parent_thread_id"`
			} `json:"payload"`
		}
		if json.Unmarshal(s.Bytes(), &r) != nil {
			continue
		}
		if r.Child {
			return "", "", errors.New("child transcript")
		}
		if agent == "codex" {
			if r.Type != "session_meta" {
				continue
			}
			if r.Payload.Parent != "" || (strings.Contains(string(r.Payload.Source), "subagent") || strings.Contains(string(r.Payload.ThreadSource), "subagent")) {
				return "", "", errors.New("child transcript")
			}
			r.Session = r.Payload.ID
			if r.Session == "" {
				r.Session = r.Payload.Session
			}
			r.Cwd = r.Payload.Cwd
		}
		if r.Session != "" && len(r.Session) <= 128 && filepath.IsAbs(r.Cwd) {
			return r.Session, r.Cwd, nil
		}
	}
	return "", "", errors.New("session metadata unavailable or unsupported")
}
func (o *observer) scan(path string, f *observedFile) {
	st, err := os.Stat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			o.problem("log inaccessible")
		}
		return
	}
	if f.n == nil && f.c.info != nil && os.SameFile(st, f.c.info) && st.Size() == f.c.offset {
		return
	}
	session, cwd, err := logIdentity(path, f.agent)
	if err != nil {
		if f.identityError != err.Error() {
			slog.Warn("observer_identity_unavailable", "path", path, "reason", err.Error())
			f.identityError = err.Error()
		}
		return
	}
	f.identityError = ""
	if f.session != "" && session != f.session {
		if f.n != nil {
			f.n.activity(1, "gap", "Log identity changed; old observed session retired")
			f.n.close()
			f.n = nil
		}
		fresh, err := newCollector(path)
		if err != nil {
			o.problem("replacement baseline unavailable")
			return
		}
		fresh.agent = f.agent
		fresh.passive = true
		fresh.peerResponse = true
		f.c = fresh
		f.session = session
		f.cwd = cwd
		f.epoch = ""
		f.blocked = false
		return
	}
	f.session = session
	f.cwd = cwd

	// Never open more than 64 observed publisher nodes per service.
	if f.n == nil {
		active := 0
		for _, other := range o.files {
			if other.n != nil {
				active++
			}
			if other != f && other.n != nil && other.session == f.session {
				o.problem("multiple log files claim one session")
				return
			}
		}
		if active >= 64 {
			o.problem("active session limit reached")
			return
		}
		n := newNode(o.ctx)
		n.identity.SessionId = f.session
		n.identity.Generation = 1
		n.identity.AgentType = f.agent
		n.identity.Project = clip(f.cwd, 180)
		n.identity.DisplayName = clip(filepath.Base(f.cwd), 40)
		n.identity.ObservationOnly = true
		n.details = o.opts.Details
		if err := n.start(Options{Runtime: o.opts.Runtime, Hostname: o.opts.Hostname}); err != nil {
			n.close()
			o.problem("observer publisher startup failed")
			return
		}
		f.n = n
	}
	lockPath, epochPath, err := ownershipPaths(o.opts.Runtime, f.session)
	if err != nil {
		o.problem("ownership directory unavailable")
		return
	}
	lock, err := localstate.Lock(lockPath, false)
	blocked := errors.Is(err, localstate.ErrBusy)
	if err != nil && !blocked {
		o.problem("ownership lock unavailable")
		return
	}
	if lock != nil {
		defer lock.Close()
	}
	epoch, _ := os.ReadFile(epochPath)
	handoff := !blocked && (f.blocked || (f.epoch != string(epoch)))
	f.n.mu.Lock()
	f.n.suppressActivity = blocked || handoff
	f.n.mu.Unlock()
	before := f.c.offset
	if handoff {
		fresh, err := newCollector(path)
		if err != nil {
			o.problem("handoff baseline unavailable")
			return
		}
		fresh.agent = f.agent
		fresh.passive = true
		fresh.peerResponse = f.c.peerResponse
		f.c = fresh
	} else {
		f.c.scan(f.n, 1)
	}
	if f.c.offset != before {
		f.last = time.Now()
	}
	f.n.mu.Lock()
	f.n.suppressActivity = false
	f.n.mu.Unlock()
	if handoff {
		f.n.activity(1, "gap", "Direct integration ended; observed collection resumed at a fresh boundary")
	}
	f.blocked = blocked
	f.epoch = string(epoch)
}
