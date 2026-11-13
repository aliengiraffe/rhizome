package dream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rhizome/internal/channel"
	"rhizome/internal/config"
	"rhizome/internal/oai"
)

// digestAt is one fixed event time shared by the history and model replies.
var digestAt = time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

// scriptClient answers model calls in order and records the requests.
type scriptClient struct {
	replies  []string
	requests []oai.Request
}

func (s *scriptClient) Status() (oai.State, error) {
	var st oai.State
	err := json.Unmarshal([]byte(`{"secrets":{"id":"synthetic","chunks":1}}`), &st)
	return st, err
}
func (s *scriptClient) Respond(_ context.Context, r oai.Request) (string, error) {
	s.requests = append(s.requests, r)
	if len(s.requests) > len(s.replies) {
		return "", fmt.Errorf("unexpected model call %d", len(s.requests))
	}
	return s.replies[len(s.requests)-1], nil
}

func digestReplyJSON(entries ...DigestEntryProposal) string {
	b, _ := json.Marshal(DigestProposal{Version: 1, Entries: entries})
	return string(b)
}

// digestRun runs one dream over two sessions: a public one in a project and a
// private one. It returns the client and the configuration used.
func digestRun(t *testing.T, mode string, dry bool, replies ...string) (*scriptClient, config.Config, string) {
	t.Helper()
	runtime := filepath.Join(t.TempDir(), "runtime")
	c := config.Config{RuntimeDir: runtime, Dream: config.Dream{Enabled: true, Lookback: "7d", MaxInputBytes: 64 << 10}, Gateway: config.Gateway{Dream: config.DreamSharing{Mode: mode, Lookback: "7d"}}}
	if _, err := channel.SessionVisibility(context.Background(), runtime, "private-session", "private"); err != nil {
		t.Fatal(err)
	}
	at := digestAt
	pub := channel.DreamExcerpt{Source: Source{SessionID: "public-session", Agent: "claude", At: at}, Kind: "assistant_text", Text: "Switched the cache to LRU"}
	priv := channel.DreamExcerpt{Source: Source{SessionID: "private-session", Agent: "codex", At: at}, Kind: "assistant_text", Text: "PRIVATE-MARKER salary review"}
	history := func(_ context.Context, c config.Config, _ channel.DreamProgress, _ time.Time, _ int) (channel.DreamBatch, error) {
		b := channel.DreamBatch{Excerpts: []channel.DreamExcerpt{pub, priv}, Scanned: map[string]time.Time{channel.DreamKey("claude", "public-session"): at, channel.DreamKey("codex", "private-session"): at}}
		if c.Gateway.Dream.Enabled() {
			b.Projects = map[string]string{channel.DreamKey("claude", "public-session"): "github.com/acme/app", channel.DreamKey("codex", "private-session"): "github.com/acme/app"}
		}
		return b, nil
	}
	client := &scriptClient{replies: replies}
	var out, meta bytes.Buffer
	err := run(context.Background(), c, client, dry, &out, &meta, history)
	if err != nil {
		t.Fatalf("run: %v (stderr %s)", err, meta.String())
	}
	return client, c, out.String()
}

func memoryReply() string {
	return reply(Operation{Op: "add", ID: "cache", Topic: "decisions", Text: "Cache is LRU", Sources: []Source{{SessionID: "public-session", Agent: "claude", At: digestAt}}})
}

func TestDigestGeneration(t *testing.T) {
	entry := DigestEntryProposal{Type: "decision", Text: "Cache switched to LRU", Project: "github.com/acme/app", Sources: []channel.DigestSource{{SessionID: "public-session", Agent: "claude"}}}

	// Off makes exactly the P0 memory call and stores no digest.
	client, c, _ := digestRun(t, config.DreamShareOff, false, memoryReply())
	if len(client.requests) != 1 {
		t.Fatalf("off: %d model calls", len(client.requests))
	}
	if all, _ := (channel.DigestStore{Runtime: c.RuntimeDir}).Load(time.Now()); len(all) != 0 {
		t.Fatal("off stored a digest")
	}

	// Review stores a pending digest from public sessions only.
	client, c, _ = digestRun(t, config.DreamShareReview, false, memoryReply(), digestReplyJSON(entry))
	if len(client.requests) != 2 || client.requests[1].Instructions != DigestInstructions {
		t.Fatalf("review: %d calls", len(client.requests))
	}
	sent := client.requests[1].Input[0].Content
	if strings.Contains(sent, "PRIVATE-MARKER") || strings.Contains(sent, "private-session") || !strings.Contains(sent, "github.com/acme/app") {
		t.Fatalf("digest input leaked private data or missed project: %s", sent)
	}
	all, err := (channel.DigestStore{Runtime: c.RuntimeDir}).Load(time.Now())
	if err != nil || len(all) != 1 || all[0].Status != channel.DigestPending || all[0].Entries[0].Text != entry.Text {
		t.Fatalf("review digest: %+v %v", all, err)
	}
	if d := all[0].Entries[0].ExpiresAt.Sub(all[0].CreatedAt); d != channel.DigestDefaultTTL {
		t.Fatalf("default expiry %v", d)
	}

	// Auto publishes at creation.
	_, c, _ = digestRun(t, config.DreamShareAuto, false, memoryReply(), digestReplyJSON(entry))
	all, _ = (channel.DigestStore{Runtime: c.RuntimeDir}).Load(time.Now())
	if len(all) != 1 || all[0].Status != channel.DigestPublished {
		t.Fatalf("auto digest: %+v", all)
	}

	// Dry run prints the proposal and stores nothing.
	_, c, out := digestRun(t, config.DreamShareAuto, true, memoryReply(), digestReplyJSON(entry))
	if !strings.Contains(out, `"digest"`) || !strings.Contains(out, entry.Text) {
		t.Fatalf("dry output %s", out)
	}
	if all, _ := (channel.DigestStore{Runtime: c.RuntimeDir}).Load(time.Now()); len(all) != 0 {
		t.Fatal("dry run stored a digest")
	}
}

func TestDigestGenerationFailureKeepsMemory(t *testing.T) {
	runtime := filepath.Join(t.TempDir(), "runtime")
	c := config.Config{RuntimeDir: runtime, Dream: config.Dream{Enabled: true, Lookback: "7d", MaxInputBytes: 64 << 10}, Gateway: config.Gateway{Dream: config.DreamSharing{Mode: "review", Lookback: "7d"}}}
	at := digestAt
	ex := channel.DreamExcerpt{Source: Source{SessionID: "public-session", Agent: "claude", At: at}, Kind: "assistant_text", Text: "x"}
	history := func(context.Context, config.Config, channel.DreamProgress, time.Time, int) (channel.DreamBatch, error) {
		return channel.DreamBatch{Excerpts: []channel.DreamExcerpt{ex}, Scanned: map[string]time.Time{channel.DreamKey("claude", "public-session"): at}, Projects: map[string]string{channel.DreamKey("claude", "public-session"): "github.com/acme/app"}}, nil
	}
	bad := digestReplyJSON(DigestEntryProposal{Type: "decision", Text: "x", Project: "github.com/acme/app", Sources: []channel.DigestSource{{SessionID: "invented", Agent: "claude"}}})
	err := run(context.Background(), c, &scriptClient{replies: []string{memoryReply(), bad}}, false, io.Discard, io.Discard, history)
	if err == nil || !strings.Contains(err.Error(), "memory revision 1 committed") {
		t.Fatalf("err %v", err)
	}
	if revs, _ := (Store{Runtime: runtime}).Load(); len(revs) != 1 {
		t.Fatal("memory revision lost")
	}
	if all, _ := (channel.DigestStore{Runtime: runtime}).Load(time.Now()); len(all) != 0 {
		t.Fatal("invalid digest stored")
	}
}

func TestValidateDigest(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	src := channel.DigestSource{SessionID: "s1", Agent: "claude"}
	other := channel.DigestSource{SessionID: "s2", Agent: "codex"}
	allowed := map[channel.DigestSource]string{src: "github.com/acme/app", other: "github.com/acme/other"}
	ok := func() DigestEntryProposal {
		return DigestEntryProposal{Type: "blocker", Text: "CI is red on main", Project: "github.com/acme/app", Sources: []channel.DigestSource{src}}
	}
	if got, err := ValidateDigest(digestReplyJSON(ok()), allowed, now); err != nil || len(got) != 1 {
		t.Fatalf("valid: %v %v", got, err)
	}
	if got, err := ValidateDigest(`{"version":1,"entries":[]}`, allowed, now); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
	for name, change := range map[string]func(*DigestEntryProposal){
		"invented source":  func(e *DigestEntryProposal) { e.Sources = []channel.DigestSource{{SessionID: "nope", Agent: "claude"}} },
		"wrong project":    func(e *DigestEntryProposal) { e.Project = "github.com/acme/other" },
		"mixed projects":   func(e *DigestEntryProposal) { e.Sources = append(e.Sources, other) },
		"unknown type":     func(e *DigestEntryProposal) { e.Type = "secret" },
		"text bound":       func(e *DigestEntryProposal) { e.Text = strings.Repeat("x", channel.DigestMaxText+1) },
		"control":          func(e *DigestEntryProposal) { e.Text = "a\nb" },
		"no sources":       func(e *DigestEntryProposal) { e.Sources = nil },
		"past expiry":      func(e *DigestEntryProposal) { at := now.Add(-time.Hour); e.ExpiresAt = &at },
		"far expiry":       func(e *DigestEntryProposal) { at := now.Add(31 * 24 * time.Hour); e.ExpiresAt = &at },
		"non-UTC expiry":   func(e *DigestEntryProposal) { at := now.Add(time.Hour).In(time.FixedZone("x", 3600)); e.ExpiresAt = &at },
		"duplicate source": func(e *DigestEntryProposal) { e.Sources = append(e.Sources, src) },
	} {
		e := ok()
		change(&e)
		if _, err := ValidateDigest(digestReplyJSON(e), allowed, now); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	many := make([]DigestEntryProposal, channel.DigestMaxEntries+1)
	for i := range many {
		many[i] = ok()
	}
	if _, err := ValidateDigest(digestReplyJSON(many...), allowed, now); err == nil {
		t.Error("17 entries accepted")
	}
	big := make([]DigestEntryProposal, channel.DigestMaxEntries)
	for i := range big {
		big[i] = ok()
		big[i].Text = strings.Repeat("w", channel.DigestMaxText)
	}
	if _, err := ValidateDigest(digestReplyJSON(big...), allowed, now); err == nil {
		t.Error("over 8 KiB accepted")
	}
	for _, raw := range []string{`nope`, `{"version":2,"entries":[]}`, `{"version":1,"entries":null}`, `{"version":1,"entries":[],"extra":1}`, `{"version":1,"entries":[{"type":"blocker","text":"x","project":"github.com/acme/app","sources":[{"session_id":"s1","agent":"claude"}],"host_id":"x"}]}`} {
		if _, err := ValidateDigest(raw, allowed, now); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	secret := ok()
	secret.Text = "token ghp_q8Fz3kR1mN7vT2wX9bL4pH6sC0dJ5aG8eY1u"
	got, err := ValidateDigest(digestReplyJSON(secret), allowed, now)
	if err != nil || strings.Contains(got[0].Text, "ghp_") {
		t.Fatalf("redaction: %v %v", got, err)
	}
}

func TestDigestReview(t *testing.T) {
	runtime := filepath.Join(t.TempDir(), "runtime")
	unlock, err := (Store{Runtime: runtime}).Lock(false) // creates the private memory tree
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	now := time.Now().UTC()
	entries := []channel.DigestEntry{}
	for i := 0; i < 3; i++ {
		entries = append(entries, channel.DigestEntry{Type: "gotcha", Text: fmt.Sprint("entry ", i+1), Project: "github.com/acme/app", Sources: []channel.DigestSource{{SessionID: "s1", Agent: "claude"}}, ExpiresAt: now.Add(time.Hour)})
	}
	if err := (channel.DigestStore{Runtime: runtime}).Save(channel.Digest{Version: 1, ID: "00000000000000e1", CreatedAt: now, Status: channel.DigestPending, Entries: entries}, now); err != nil {
		t.Fatal(err)
	}
	r := Review{Runtime: runtime}
	var out bytes.Buffer
	if err := r.List(&out); err != nil || !strings.Contains(out.String(), "00000000000000e1") || !strings.Contains(out.String(), "pending") {
		t.Fatalf("list: %v %s", err, out.String())
	}
	for _, bad := range []string{"0", "4", "1,1", "x"} {
		if err := r.Approve("00000000000000e1", bad); err == nil {
			t.Errorf("approve --entries %s accepted", bad)
		}
	}
	if err := r.Approve("missing00000000", ""); err == nil {
		t.Error("unknown digest approved")
	}
	if err := r.Approve("00000000000000e1", "3,1"); err != nil {
		t.Fatal(err)
	}
	d, _ := (channel.DigestStore{Runtime: runtime}).Get("00000000000000e1", now)
	if d.Status != channel.DigestApproved || fmt.Sprint(d.Approved) != "[0 2]" {
		t.Fatalf("approved %+v", d)
	}
	out.Reset()
	if err := r.Show(&out, "00000000000000e1"); err != nil || !strings.Contains(out.String(), "*[1]") || strings.Contains(out.String(), "*[2]") {
		t.Fatalf("show: %v %s", err, out.String())
	}
	if err := r.Reject("00000000000000e1"); err != nil {
		t.Fatal(err)
	}
	if err := r.Approve("00000000000000e1", ""); err == nil {
		t.Error("rejected digest re-approved")
	}
	out.Reset()
	if err := r.Peers(&out); err != nil || !strings.Contains(out.String(), "no digests received") {
		t.Fatalf("peers without gateway: %v %s", err, out.String())
	}
}
