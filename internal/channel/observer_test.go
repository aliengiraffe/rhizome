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

	"github.com/fsnotify/fsnotify"
	"rhizome/internal/localstate"
	pb "rhizome/proto"
)

func claudeRecord(session, uid, text string) string {
	b, _ := json.Marshal(map[string]any{"type": "assistant", "uuid": uid, "sessionId": session, "cwd": os.TempDir(), "message": map[string]any{"role": "assistant", "content": []map[string]string{{"type": "text", "text": text}}}})
	return string(b) + "\n"
}
func appendLog(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}
func observerHarness(t *testing.T) *observer {
	t.Helper()
	dir := t.TempDir()
	w, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	o := &observer{ctx: context.Background(), opts: ObserverOptions{Runtime: filepath.Join(dir, "runtime"), ClaudeRoot: filepath.Join(dir, "claude"), CodexRoot: filepath.Join(dir, "codex")}, watch: w, files: map[string]*observedFile{}, dirs: map[string]bool{}}
	for _, p := range []string{o.opts.Runtime, o.opts.ClaudeRoot, o.opts.CodexRoot} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		w.Close()
		for _, f := range o.files {
			if f.n != nil {
				f.n.close()
			}
		}
	})
	return o
}
func activityTexts(n *Node) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, e := range n.history {
		if e.Kind == "activity" {
			out = append(out, e.Text)
		}
	}
	return out
}
func TestObservedOwnershipHandoff(t *testing.T) {
	for _, directFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(directFirst), func(t *testing.T) {
			o := observerHarness(t)
			path := filepath.Join(o.opts.ClaudeRoot, "session.jsonl")
			appendLog(t, path, claudeRecord("session", "old", "historical"))
			o.inventory(true)
			direct := newNode(context.Background())
			if err := direct.start(Options{Runtime: o.opts.Runtime}); err != nil {
				t.Fatal(err)
			}
			closed := false
			defer func() {
				if !closed {
					direct.close()
				}
			}()
			hook := Hook{SessionID: "session", TranscriptPath: path, Cwd: o.opts.ClaudeRoot, Event: "UserPromptSubmit"}
			if directFirst {
				if err := direct.observe(hook); err != nil {
					t.Fatal(err)
				}
			}
			// Human prompt unlocks conservative startup feedback suppression.
			appendLog(t, path, `{"type":"user","uuid":"human","sessionId":"session","message":{"content":"do work"}}`+"\n"+claudeRecord("session", "first", "first"))
			o.visit(path, false)
			f := o.files[path]
			if f.n == nil {
				t.Fatal("not discovered")
			}
			if directFirst {
				if len(activityTexts(f.n)) != 0 {
					t.Fatal("observer published direct-owned activity")
				}
			} else {
				if got := activityTexts(f.n); len(got) != 1 || got[0] != "first" {
					t.Fatal(got)
				}
				if err := direct.observe(hook); err != nil {
					t.Fatal(err)
				}
			}
			appendLog(t, path, claudeRecord("session", "second", "second"))
			o.visit(path, false)
			eventually(t, func() bool { return strings.Contains(strings.Join(activityTexts(direct), ","), "second") })
			if strings.Contains(strings.Join(activityTexts(f.n), ","), "second") {
				t.Fatal("duplicate observer activity")
			}
			eventually(t, func() bool {
				target, err := f.n.resolve(context.Background(), "session")
				return err == nil && target.NodeId == direct.local().NodeId
			})
			peers := logicalPeers([]*pb.Identity{f.n.local(), direct.local()})
			if len(peers) != 1 || peers[0].ObservationOnly {
				t.Fatal("duplicate logical session")
			}
			direct.close()
			closed = true
			o.visit(path, false) // explicit fresh boundary on takeover
			appendLog(t, path, `{"type":"user","uuid":"next-human","sessionId":"session","message":{"content":"continue"}}`+"\n"+claudeRecord("session", "third", "third"))
			o.visit(path, false)
			o.visit(path, false)
			got := activityTexts(f.n)
			if got[len(got)-1] != "third" {
				t.Fatal(got)
			}
			count := 0
			for _, s := range got {
				if s == "third" {
					count++
				}
			}
			if count != 1 {
				t.Fatal(got)
			}
			if _, err := f.n.Send(context.Background(), &pb.SendRequest{}); err == nil || !strings.Contains(err.Error(), "observation-only") {
				t.Fatal(err)
			}
		})
	}
}
func TestObserverNativeNotificationsAndMissingRoots(t *testing.T) {
	dir := t.TempDir()
	rt := filepath.Join(dir, "runtime")
	if err := os.Mkdir(rt, 0700); err != nil {
		t.Fatal(err)
	}
	opts := ObserverOptions{Runtime: rt, ClaudeRoot: filepath.Join(dir, "late", "claude"), CodexRoot: filepath.Join(dir, "late", "codex")}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runObserver(ctx, opts, 20*time.Millisecond) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("observer did not stop")
		}
	})
	eventually(t, func() bool { _, err := os.Stat(filepath.Join(rt, "observer-health.json")); return err == nil })
	if err := RunObserver(ctx, opts); !errors.Is(err, localstate.ErrBusy) {
		t.Fatalf("singleton: %v", err)
	}
	peer := newNode(context.Background())
	if err := peer.start(Options{Runtime: rt}); err != nil {
		t.Fatal(err)
	}
	defer peer.close()
	bind(t, peer, "reader", dir)
	for _, root := range []string{opts.ClaudeRoot, opts.CodexRoot} {
		if err := os.MkdirAll(filepath.Join(root, "nested"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	appendLog(t, filepath.Join(opts.ClaudeRoot, "nested", "session.jsonl"), claudeRecord("native-claude", "1", "first"))
	appendLog(t, filepath.Join(opts.CodexRoot, "nested", "rollout.jsonl"), fmt.Sprintf(`{"type":"session_meta","payload":{"id":"native-codex","cwd":%q,"source":"cli"}}`+"\n", dir)+`{"type":"response_item","payload":{"type":"message","id":"a","role":"assistant","content":[{"type":"output_text","text":"first"}]}}`+"\n")
	reader := &catalogReader{scope: historyScope{Runtime: rt, ClaudeRoot: opts.ClaudeRoot, CodexRoot: opts.CodexRoot}}
	// Missing roots are discovered by the bounded root retry; subsequent changes use notifications.
	for _, id := range []string{"native-claude", "native-codex"} {
		eventually(t, func() bool { p, err := peer.resolve(context.Background(), id); return err == nil && p.ObservationOnly })
		eventually(t, func() bool {
			catalog, _, err := reader.load()
			if err != nil {
				return false
			}
			for key := range catalog.sessions() {
				if key.Session == id {
					return true
				}
			}
			return false
		})
	}
}
func TestCodexLogFilteringAndDuplicates(t *testing.T) {
	n := newNode(context.Background())
	defer n.close()
	n.identity.SessionId = "codex"
	n.identity.Generation = 1
	c := &collector{agent: "codex", seen: map[string]bool{}, tools: map[string]string{}}
	lines := []string{
		`{"type":"response_item","payload":{"type":"message","id":"one","role":"assistant","content":[{"type":"output_text","text":"same"}]}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"same"}}`,
		`{"type":"response_item","payload":{"type":"message","id":"two","role":"assistant","content":[{"type":"output_text","text":"same"}]}}`,
		`{"type":"response_item","payload":{"type":"reasoning","id":"secret","text":"secret"}}`,
		`{"type":"response_item","payload":{"type":"function_call","id":"tool","call_id":"call","name":"mcp__rhizome__publish"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"call","output":"secret"}}`,
		`{"type":"response_item","payload":{"type":"message","id":"peer","role":"user","content":[{"type":"input_text","text":"<rhizome-peer-event>injected"}]}}`,
		`{"type":"response_item","payload":{"type":"message","id":"reaction","role":"assistant","content":[{"type":"output_text","text":"reaction"}]}}`,
	}
	for _, line := range lines {
		c.normalize([]byte(line), n, 1)
	}
	c.normalize([]byte(lines[0]), n, 1)
	if got := activityTexts(n); len(got) != 2 || got[0] != "same" || got[1] != "same" {
		t.Fatal(got)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.history) != 2 {
		t.Fatalf("excluded content published: %v", n.history)
	}
}
func TestObserverPartialRotationAndStartup(t *testing.T) {
	o := observerHarness(t)
	path := filepath.Join(o.opts.ClaudeRoot, "session.jsonl")
	appendLog(t, path, claudeRecord("partial", "old", "old"))
	o.inventory(true)
	if o.files[path].n != nil {
		t.Fatal("historical file advertised")
	}
	appendLog(t, path, `{"type":"user","uuid":"prompt","message":{"content":"go"}}`+"\n")
	record := claudeRecord("partial", "new", "new")
	appendLog(t, path, record[:len(record)/2])
	o.visit(path, false)
	f := o.files[path]
	if len(activityTexts(f.n)) != 0 {
		t.Fatal("partial emitted")
	}
	appendLog(t, path, record[len(record)/2:])
	o.visit(path, false)
	if got := activityTexts(f.n); len(got) != 1 || got[0] != "new" {
		t.Fatal(got)
	}
	if err := os.WriteFile(path, []byte(claudeRecord("partial", "new", "new")), 0600); err != nil {
		t.Fatal(err)
	}
	o.visit(path, false)
	if len(activityTexts(f.n)) != 1 {
		t.Fatal("rotation replayed history")
	}
}

func TestObserverRestartWithRetainedEpoch(t *testing.T) {
	o := observerHarness(t)
	path := filepath.Join(o.opts.ClaudeRoot, "restart.jsonl")
	appendLog(t, path, claudeRecord("restart", "old", "old"))
	direct := newNode(context.Background())
	if err := direct.start(Options{Runtime: o.opts.Runtime}); err != nil {
		t.Fatal(err)
	}
	if err := direct.observe(Hook{SessionID: "restart", TranscriptPath: path, Cwd: o.opts.ClaudeRoot, Event: "UserPromptSubmit"}); err != nil {
		direct.close()
		t.Fatal(err)
	}
	direct.close()
	o.inventory(true)
	appendLog(t, path, `{"type":"user","uuid":"human","message":{"content":"continue"}}`+"\n"+claudeRecord("restart", "fresh", "fresh"))
	o.visit(path, false)
	f := o.files[path]
	if got := activityTexts(f.n); len(got) != 1 || got[0] != "fresh" {
		t.Fatalf("restart lost new prompt/activity: %v", got)
	}
}

func TestObserverReconciliationAfterWatchFailure(t *testing.T) {
	o := observerHarness(t)
	path := filepath.Join(o.opts.ClaudeRoot, "recover.jsonl")
	appendLog(t, path, claudeRecord("recover", "one", "one"))
	o.visit(path, false)
	o.watchFailed(errors.New("synthetic overflow"))
	if !o.retry {
		t.Fatal("watch failure hidden")
	}
	appendLog(t, path, claudeRecord("recover", "two", "two"))
	o.inventory(false)
	o.inventory(false)
	f := o.files[path]
	if got := activityTexts(f.n); len(got) != 2 || got[1] != "two" {
		t.Fatal(got)
	}
	f.n.mu.Lock()
	defer f.n.mu.Unlock()
	gap := false
	for _, e := range f.n.history {
		if e.Kind == "gap" {
			gap = true
		}
	}
	if !gap {
		t.Fatal("no overflow gap")
	}
}
func TestLogIdentityExclusions(t *testing.T) {
	for _, tc := range []struct{ name, agent, record string }{
		{"claude-child", "claude", `{"type":"assistant","sessionId":"child","cwd":"/tmp","isSidechain":true}`},
		{"codex-child", "codex", `{"type":"session_meta","payload":{"id":"child","cwd":"/tmp","source":{"subagent":"fork"}}}`},
		{"codex-thread-source", "codex", `{"type":"session_meta","payload":{"id":"child","cwd":"/tmp","source":"cli","thread_source":{"subagent":"fork"}}}`},
		{"no-metadata", "codex", `{"type":"response_item","payload":{"type":"message","id":"not-a-session"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "log.jsonl")
			appendLog(t, path, tc.record+"\n")
			if _, _, err := logIdentity(path, tc.agent); err == nil {
				t.Fatal("untrusted session accepted")
			}
		})
	}
}

func TestObserverDirectoryReconcileDoesNotActivateHistory(t *testing.T) {
	o := observerHarness(t)
	path := filepath.Join(o.opts.ClaudeRoot, "historical.jsonl")
	appendLog(t, path, claudeRecord("history", "old", "old"))
	o.inventory(true)
	o.inventory(false)
	if o.files[path].n != nil {
		t.Fatal("unchanged history advertised during directory reconciliation")
	}
}
func TestObservedPathCannotRebindSilently(t *testing.T) {
	o := observerHarness(t)
	path := filepath.Join(o.opts.CodexRoot, "rollout.jsonl")
	meta := func(id string) string {
		return fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":%q,"source":"cli"}}`+"\n", id, o.opts.CodexRoot)
	}
	appendLog(t, path, meta("old"))
	o.visit(path, false)
	f := o.files[path]
	if f.n == nil {
		t.Fatal("missing original publisher")
	}
	replacement := filepath.Join(o.opts.CodexRoot, "replacement.tmp")
	if err := os.WriteFile(replacement, []byte(meta("new")), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	o.visit(path, false)
	if f.n != nil || f.session != "new" {
		t.Fatal("replacement retained stale routing identity")
	}
	appendLog(t, path, `{"type":"response_item","payload":{"type":"message","id":"human","role":"user","content":[{"type":"input_text","text":"continue"}]}}`+"\n"+`{"type":"response_item","payload":{"type":"message","id":"answer","role":"assistant","content":[{"type":"output_text","text":"new-only"}]}}`+"\n")
	o.visit(path, false)
	if f.n.local().SessionId != "new" {
		t.Fatal("wrong identity")
	}
	if got := activityTexts(f.n); len(got) != 1 || got[0] != "new-only" {
		t.Fatal(got)
	}
}

func TestObserverWatchScope(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(fmt.Sprintf("present=%v", present), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			o := observerHarness(t)
			o.opts.ClaudeRoot = filepath.Join(home, ".claude", "projects")
			o.opts.CodexRoot = filepath.Join(home, ".codex", "sessions")
			o.opts.PiRoot = filepath.Join(home, ".pi", "agent", "sessions")
			o.opts.OpenCodeRoot = filepath.Join(home, ".local", "share", "opencode")
			o.index = newHistoryIndex(historyScope{Runtime: o.opts.Runtime, OpenCodeRoot: o.opts.OpenCodeRoot}, nil)
			roots := []string{o.opts.ClaudeRoot, o.opts.CodexRoot, o.opts.PiRoot, o.opts.OpenCodeRoot}
			want := map[string]bool{}
			if present {
				for _, root := range roots {
					nested := filepath.Join(root, "nested")
					if err := os.MkdirAll(nested, 0700); err != nil {
						t.Fatal(err)
					}
					want[root] = true
					if root != o.opts.OpenCodeRoot {
						want[nested] = true
					}
				}
			}
			o.inventory(true)
			if len(o.dirs) != len(want) {
				t.Fatalf("watches = %v, want %v", o.dirs, want)
			}
			for dir := range o.dirs {
				if !want[dir] {
					t.Errorf("unexpected watch: %s", dir)
				}
			}
			if o.retry {
				t.Fatal("ordinary missing roots should not trigger full-tree reconciliation")
			}
		})
	}
}

func TestObserverRejectsHomeAncestors(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(base, alias); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{home, base, filepath.VolumeName(home) + string(os.PathSeparator), filepath.Join(alias, "home"), alias} {
		for _, agent := range []string{"claude", "opencode"} {
			t.Run(agent+"/"+root, func(t *testing.T) {
				o := observerHarness(t)
				o.opts.CodexRoot = ""
				o.opts.ClaudeRoot = ""
				if agent == "claude" {
					o.opts.ClaudeRoot = root
				} else {
					o.opts.OpenCodeRoot = root
					o.index = newHistoryIndex(historyScope{Runtime: o.opts.Runtime}, nil)
				}
				o.inventory(true)
				if len(o.dirs) != 0 || (root != alias && !o.retry) {
					t.Fatalf("unsafe root: watches=%v degraded=%v", o.dirs, o.retry)
				}
			})
		}
	}
}

func TestObserverMissingCustomRootAndOpenCodeRecovery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	o := observerHarness(t)
	o.opts.ClaudeRoot = filepath.Join(home, "custom", "projects")
	o.opts.CodexRoot = ""
	o.opts.OpenCodeRoot = filepath.Join(home, ".local", "share", "opencode")
	o.index = newHistoryIndex(historyScope{Runtime: o.opts.Runtime, ClaudeRoot: o.opts.ClaudeRoot, OpenCodeRoot: o.opts.OpenCodeRoot}, nil)
	o.inventory(true)
	if len(o.dirs) != 0 {
		t.Fatalf("missing roots watched parents: %v", o.dirs)
	}
	for _, root := range []string{o.opts.ClaudeRoot, o.opts.OpenCodeRoot} {
		if err := os.MkdirAll(filepath.Join(root, "nested"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(o.opts.ClaudeRoot, "new.jsonl")
	appendLog(t, path, claudeRecord("late-root", "1", "new"))
	o.index.ocDirty = false
	o.retryRoots()
	if !o.dirs[o.opts.ClaudeRoot] || !o.dirs[o.opts.OpenCodeRoot] || !o.index.ocDirty {
		t.Fatalf("root recovery failed: watches=%v opencode dirty=%v", o.dirs, o.index.ocDirty)
	}
	if o.dirs[filepath.Join(o.opts.OpenCodeRoot, "nested")] {
		t.Fatal("OpenCode subtree watched")
	}
	if o.files[path] == nil || o.files[path].n == nil || len(o.index.queue) == 0 {
		t.Fatal("new log not collected and queued for indexing")
	}
	// Already watched roots are not traversed or refreshed by the retry timer.
	o.index.ocDirty = false
	o.retryRoots()
	if o.index.ocDirty {
		t.Fatal("unchanged OpenCode root refreshed by timer")
	}
}

func TestObserverWatchDirectoryLimit(t *testing.T) {
	o := observerHarness(t)
	for i := 0; i < 4096; i++ {
		o.dirs[fmt.Sprintf("existing-%d", i)] = true
	}
	o.addRoot(o.opts.ClaudeRoot, false)
	o.health()
	if len(o.dirs) != 4096 || !o.retry {
		t.Fatalf("limit not enforced: %d, degraded=%v", len(o.dirs), o.retry)
	}
	data, err := os.ReadFile(filepath.Join(o.opts.Runtime, "observer-health.json"))
	if err != nil {
		t.Fatal(err)
	}
	var health struct {
		Degraded bool `json:"degraded"`
	}
	if err := json.Unmarshal(data, &health); err != nil || !health.Degraded {
		t.Fatalf("degraded health missing: %s (%v)", data, err)
	}
}
