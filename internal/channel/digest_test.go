package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rhizome/internal/config"
	"rhizome/internal/localstate"
)

var digestNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

func digestEntry(text string) DigestEntry {
	return DigestEntry{Type: "decision", Text: text, Project: "github.com/acme/app", Sources: []DigestSource{{SessionID: "s-public", Agent: "claude"}}, ExpiresAt: digestNow.Add(DigestDefaultTTL)}
}

func testDigest(id, status string, created time.Time, entries ...DigestEntry) Digest {
	d := Digest{Version: 1, ID: id, CreatedAt: created, Status: status, Entries: entries}
	if status == DigestApproved {
		for i := range entries {
			d.Approved = append(d.Approved, i)
		}
	}
	return d
}

func digestRuntime(t *testing.T) string {
	t.Helper()
	runtime := filepath.Join(t.TempDir(), "run")
	for _, dir := range []string{runtime, filepath.Join(runtime, "memory")} {
		if err := localstate.SecureDir(dir); err != nil {
			t.Fatal(err)
		}
	}
	return runtime
}

func TestDigestEntryValidation(t *testing.T) {
	if !ValidDigestEntry(digestEntry("Uses focused tests"), digestNow) {
		t.Fatal("valid entry rejected")
	}
	for name, change := range map[string]func(*DigestEntry){
		"type":             func(e *DigestEntry) { e.Type = "secret" },
		"blank":            func(e *DigestEntry) { e.Text = "  " },
		"control":          func(e *DigestEntry) { e.Text = "a\nb" },
		"text bound":       func(e *DigestEntry) { e.Text = strings.Repeat("x", DigestMaxText+1) },
		"invalid utf8":     func(e *DigestEntry) { e.Text = "\xff" },
		"project":          func(e *DigestEntry) { e.Project = "../etc" },
		"project creds":    func(e *DigestEntry) { e.Project = "user:pw@github.com/acme/app" },
		"no sources":       func(e *DigestEntry) { e.Sources = nil },
		"too many sources": func(e *DigestEntry) { e.Sources = make([]DigestSource, DigestMaxSources+1) },
		"duplicate source": func(e *DigestEntry) { e.Sources = append(e.Sources, e.Sources[0]) },
		"bad agent":        func(e *DigestEntry) { e.Sources[0].Agent = "shell" },
		"expired":          func(e *DigestEntry) { e.ExpiresAt = digestNow.Add(-time.Second) },
		"too far":          func(e *DigestEntry) { e.ExpiresAt = digestNow.Add(DigestMaxTTL + time.Hour) },
	} {
		e := digestEntry("ok")
		e.Sources = append([]DigestSource(nil), e.Sources...)
		change(&e)
		if ValidDigestEntry(e, digestNow) {
			t.Errorf("%s accepted", name)
		}
	}
	at := digestEntry("token ghp_q8Fz3kR1mN7vT2wX9bL4pH6sC0dJ5aG8eY1u in config")
	red, ok := RedactDigestEntry(at, digestNow)
	if !ok || strings.Contains(red.Text, "ghp_") {
		t.Fatalf("redaction: %v %q", ok, red.Text)
	}
}

func TestDigestBounds(t *testing.T) {
	entries := make([]DigestEntry, DigestMaxEntries+1)
	for i := range entries {
		entries[i] = digestEntry(fmt.Sprint("entry ", i))
	}
	if err := ValidDigest(testDigest("0123456789abcdef", DigestPending, digestNow, entries...), digestNow); err == nil {
		t.Fatal("17 entries accepted")
	}
	big := make([]DigestEntry, DigestMaxEntries)
	for i := range big {
		big[i] = digestEntry(strings.Repeat("y", DigestMaxText))
	}
	if err := ValidDigest(testDigest("0123456789abcdef", DigestPending, digestNow, big...), digestNow); err == nil || !strings.Contains(err.Error(), "8 KiB") {
		t.Fatalf("oversized digest: %v", err)
	}
	for name, d := range map[string]Digest{
		"id":       testDigest("../../x", DigestPending, digestNow, digestEntry("a")),
		"status":   testDigest("0123456789abcdef", "shared", digestNow, digestEntry("a")),
		"empty":    testDigest("0123456789abcdef", DigestPending, digestNow),
		"future":   testDigest("0123456789abcdef", DigestPending, digestNow.Add(time.Hour), digestEntry("a")),
		"approval": {Version: 1, ID: "0123456789abcdef", CreatedAt: digestNow, Status: DigestApproved, Approved: []int{3}, Entries: []DigestEntry{digestEntry("a")}},
	} {
		if ValidDigest(d, digestNow) == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestDigestStore(t *testing.T) {
	runtime := digestRuntime(t)
	s := DigestStore{Runtime: runtime}
	if all, err := s.Load(digestNow); err != nil || len(all) != 0 {
		t.Fatalf("empty store: %v %v", all, err)
	}
	old := testDigest("0000000000000001", DigestPending, digestNow.Add(-time.Hour), digestEntry("old"))
	recent := testDigest("0000000000000002", DigestApproved, digestNow, digestEntry("new"))
	for _, d := range []Digest{old, recent} {
		if err := s.Save(d, digestNow); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(filepath.Join(runtime, "memory", "digests", recent.ID+".json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("digest file mode: %v %v", info, err)
	}
	all, err := s.Load(digestNow)
	if err != nil || len(all) != 2 || all[0].ID != recent.ID {
		t.Fatalf("load newest first: %+v %v", all, err)
	}
	// Invalid or renamed files are skipped, never served.
	if err := os.WriteFile(filepath.Join(runtime, "memory", "digests", "0000000000000003.json"), []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(old)
	if err := os.WriteFile(filepath.Join(runtime, "memory", "digests", "00000000000000ff.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.Load(digestNow); len(all) != 2 {
		t.Fatalf("invalid files loaded: %d", len(all))
	}
	// Expired digests are pruned.
	if err := s.Prune(digestNow.Add(DigestDefaultTTL + time.Hour)); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.Load(digestNow); len(all) != 0 {
		t.Fatalf("expired digests kept: %d", len(all))
	}
}

func TestServableDigestsPolicy(t *testing.T) {
	runtime := digestRuntime(t)
	s := DigestStore{Runtime: runtime}
	pending := testDigest("00000000000000a1", DigestPending, digestNow, digestEntry("pending"))
	approved := testDigest("00000000000000a2", DigestApproved, digestNow, digestEntry("approved one"), digestEntry("approved two"))
	approved.Approved = []int{1}
	published := testDigest("00000000000000a3", DigestPublished, digestNow, digestEntry("auto"))
	rejected := testDigest("00000000000000a4", DigestRejected, digestNow, digestEntry("rejected"))
	stale := testDigest("00000000000000a5", DigestApproved, digestNow.Add(-8*24*time.Hour), DigestEntry{Type: "gotcha", Text: "stale", Project: "github.com/acme/app", Sources: []DigestSource{{SessionID: "s-public", Agent: "claude"}}, ExpiresAt: digestNow.Add(time.Hour)})
	for _, d := range []Digest{pending, approved, published, rejected, stale} {
		if err := s.Save(d, digestNow); err != nil {
			t.Fatal(err)
		}
	}
	texts := func(policy config.DreamSharing) []string {
		var out []string
		for _, d := range servableDigests(runtime, policy, digestNow) {
			for _, e := range d.Entries {
				out = append(out, e.Text)
			}
		}
		return out
	}
	for _, tc := range []struct {
		policy config.DreamSharing
		want   string
	}{
		{config.DreamSharing{}, ""},
		{config.DreamSharing{Mode: "off", Lookback: "7d"}, ""},
		{config.DreamSharing{Mode: "review", Lookback: "7d"}, "approved two"},
		{config.DreamSharing{Mode: "auto", Lookback: "7d"}, "approved two,auto"},
		{config.DreamSharing{Mode: "review", Lookback: "30d"}, "approved two,stale"},
	} {
		got := texts(tc.policy)
		if strings.Join(sortedCopy(got), ",") != tc.want {
			t.Errorf("%+v served %v, want %s", tc.policy, got, tc.want)
		}
	}

	// A session made private after approval is withheld on the next request.
	review := config.DreamSharing{Mode: "review", Lookback: "7d"}
	if _, err := SessionVisibility(context.Background(), runtime, "s-public", "private"); err != nil {
		t.Fatal(err)
	}
	if got := texts(review); len(got) != 0 {
		t.Fatalf("private session served: %v", got)
	}
	if _, err := SessionVisibility(context.Background(), runtime, "s-public", "public"); err != nil {
		t.Fatal(err)
	}
	// Expired entries are withheld.
	if got := servableDigests(runtime, review, digestNow.Add(DigestDefaultTTL+time.Minute)); len(got) != 0 {
		t.Fatalf("expired served: %v", got)
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestServableDigestsResponseBound(t *testing.T) {
	runtime := digestRuntime(t)
	s := DigestStore{Runtime: runtime}
	for i := 0; i < 20; i++ {
		entries := make([]DigestEntry, 12)
		for j := range entries {
			entries[j] = digestEntry(strings.Repeat("z", 480))
		}
		if err := s.Save(testDigest(fmt.Sprintf("%016x", i+1), DigestPublished, digestNow.Add(-time.Duration(i)*time.Minute), entries...), digestNow); err != nil {
			t.Fatal(err)
		}
	}
	got := servableDigests(runtime, config.DreamSharing{Mode: "auto", Lookback: "7d"}, digestNow)
	b, _ := json.Marshal(digestReply{Version: 1, Digests: got})
	if len(got) == 0 || len(got) == 20 || len(b) > digestMaxResponse {
		t.Fatalf("response bound: %d digests, %d bytes", len(got), len(b))
	}
	if got[0].ID != fmt.Sprintf("%016x", 1) {
		t.Fatal("newest digest not first")
	}
}

func TestParsePeerDigests(t *testing.T) {
	good := digestReply{Version: 1, Digests: []PeerDigest{{ID: "00000000000000b1", CreatedAt: digestNow, Entries: []DigestEntry{digestEntry("token ghp_q8Fz3kR1mN7vT2wX9bL4pH6sC0dJ5aG8eY1u")}}}}
	b, _ := json.Marshal(good)
	got, err := parsePeerDigests(b, digestNow)
	if err != nil || len(got) != 1 || strings.Contains(got[0].Entries[0].Text, "ghp_") {
		t.Fatalf("parse: %+v %v", got, err)
	}
	tooMany := good
	tooMany.Digests = []PeerDigest{{ID: "00000000000000b1", CreatedAt: digestNow, Entries: make([]DigestEntry, DigestMaxEntries+1)}}
	for i := range tooMany.Digests[0].Entries {
		tooMany.Digests[0].Entries[i] = digestEntry("x")
	}
	tm, _ := json.Marshal(tooMany)
	for name, body := range map[string][]byte{
		"not json":    []byte("nope"),
		"version":     []byte(`{"version":2,"digests":[]}`),
		"unknown":     []byte(`{"version":1,"digests":[],"forward_to":"x"}`),
		"null":        []byte(`{"version":1,"digests":null}`),
		"bad entry":   []byte(`{"version":1,"digests":[{"id":"00000000000000b1","created_at":"2026-11-13T12:00:00Z","entries":[{"type":"x"}]}]}`),
		"too many":    tm,
		"oversized":   []byte(`{"version":1,"digests":[]}` + strings.Repeat(" ", digestMaxResponse)),
		"duplicate":   mustJSON(digestReply{Version: 1, Digests: []PeerDigest{good.Digests[0], good.Digests[0]}}),
		"trailing":    append(b, []byte(" {}")...),
		"bad id":      []byte(`{"version":1,"digests":[{"id":"../x","created_at":"2026-11-13T12:00:00Z","entries":[]}]}`),
		"no entries":  []byte(`{"version":1,"digests":[{"id":"00000000000000b1","created_at":"2026-11-13T12:00:00Z","entries":[]}]}`),
		"no creation": []byte(`{"version":1,"digests":[{"id":"00000000000000b1","entries":[]}]}`),
	} {
		if _, err := parsePeerDigests(body, digestNow); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
