package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileNames(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../secret", "a/b", "a\\b", "a:b", "a\n", "\xff"} {
		if safeFileName(name) {
			t.Errorf("accepted unsafe name %q", name)
		}
	}
	if !safeFileName("report résumé.md") {
		t.Fatal("valid name rejected")
	}
}
func TestGatewayFiles(t *testing.T) {
	a, b := startGatewayHarness(t), startGatewayHarness(t)
	na, nb := gatewayTestNode(t, a, "files-a"), gatewayTestNode(t, b, "files-b")
	a.command(t, GatewayCommand{Action: "join", Code: b.invite(t)})
	gatewayUntil(t, func() bool {
		_, e := na.resolve(context.Background(), "files-b")
		_, f := nb.resolve(context.Background(), "files-a")
		return e == nil && f == nil
	})
	send := func(path string) FileTransfer {
		t.Helper()
		var offer FileTransfer
		raw := a.command(t, GatewayCommand{Action: "files-send", Session: "files-a", Target: "files-b", Path: path})
		if err := json.Unmarshal(raw, &offer); err != nil {
			t.Fatal(err)
		}
		return offer
	}
	fail := func(h *gatewayHarness, c GatewayCommand) {
		t.Helper()
		if _, err := GatewayAdmin(context.Background(), h.runtime, c); err == nil {
			t.Fatalf("unexpected success: %s", c.Action)
		}
	}
	path := filepath.Join(t.TempDir(), "report.bin")
	data := bytes.Repeat([]byte{0, 255, 128, 4}, fileChunk/2+37)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	offer := send(path)
	if offer.State != "offered" || offer.Size != int64(len(data)) {
		t.Fatalf("offer: %+v", offer)
	}
	// Sender mutations cannot alter the staged snapshot.
	os.WriteFile(path, []byte("changed"), 0600)
	var listing []FileTransfer
	json.Unmarshal(b.command(t, GatewayCommand{Action: "files-list", Session: "files-b"}), &listing)
	if len(listing) != 1 || listing[0].Path != "" {
		t.Fatalf("unexpected inbox: %+v", listing)
	}
	fail(b, GatewayCommand{Action: "files-accept", Session: "files-a", ID: offer.ID})
	var received FileTransfer
	json.Unmarshal(b.command(t, GatewayCommand{Action: "files-accept", Session: "files-b", ID: offer.ID}), &received)
	got, err := os.ReadFile(received.Path)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("download mismatch: %v", err)
	}
	st, _ := os.Stat(received.Path)
	if st.Mode().Perm() != 0600 {
		t.Fatal("insecure download permissions")
	}
	if received.State != "verified" {
		t.Fatal("missing verification")
	}
	json.Unmarshal(a.command(t, GatewayCommand{Action: "files-list", Session: "files-a"}), &listing)
	if listing[0].State != "verified" {
		t.Fatal("sender missing receipt")
	}
	// Acceptance is idempotent; cancellation removes the downloaded copy.
	b.command(t, GatewayCommand{Action: "files-accept", Session: "files-b", ID: offer.ID})
	b.command(t, GatewayCommand{Action: "files-cancel", Session: "files-b", ID: offer.ID})
	if _, err = os.Stat(received.Path); !os.IsNotExist(err) {
		t.Fatal("cancel retained download")
	}
	fail(b, GatewayCommand{Action: "files-accept", Session: "files-b", ID: offer.ID})
	// Empty files verify correctly.
	os.WriteFile(path, nil, 0600)
	empty := send(path)
	b.command(t, GatewayCommand{Action: "files-accept", Session: "files-b", ID: empty.ID})
	// Symlinks and oversized files must never be offered.
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(path, link)
	fail(a, GatewayCommand{Action: "files-send", Session: "files-a", Target: "files-b", Path: link})
	f, _ := os.OpenFile(path, os.O_WRONLY, 0600)
	f.Truncate(fileLimit + 1)
	f.Close()
	fail(a, GatewayCommand{Action: "files-send", Session: "files-a", Target: "files-b", Path: path})
	// Corruption of a private staged file is detected before exposing a final path.
	os.WriteFile(path, []byte("original"), 0600)
	bad := send(path)
	entries, _ := filepath.Glob(filepath.Join(a.runtime, "transfers", "transfer-*", "payload"))
	for _, p := range entries {
		os.WriteFile(p, []byte("tampered"), 0600)
	}
	fail(b, GatewayCommand{Action: "files-accept", Session: "files-b", ID: bad.ID})
	// Disconnect/revocation fences offers, even after a new trust grant is created.
	var peers []struct {
		Host string `json:"host_id"`
	}
	json.Unmarshal(a.command(t, GatewayCommand{Action: "peers"}), &peers)
	a.command(t, GatewayCommand{Action: "revoke", ID: peers[0].Host})
	fail(b, GatewayCommand{Action: "files-accept", Session: "files-b", ID: bad.ID})
}

func TestFileStoreBoundsAndExpiry(t *testing.T) {
	store, err := newGatewayFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	link := &gatewayLink{ctx: ctx}
	for i := 0; i < fileCount; i++ {
		r := &fileRecord{link: link, info: FileTransfer{ID: fmt.Sprint(i), Expires: time.Now().Add(time.Hour)}}
		if err := store.add(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.add(&fileRecord{link: link, info: FileTransfer{ID: "overflow"}}); err == nil {
		t.Fatal("quota not enforced")
	}
	expired := store.get("0")
	expired.mu.Lock()
	expired.info.Expires = time.Now().Add(-time.Second)
	expired.mu.Unlock()
	store.expire()
	if store.get("0") != nil {
		t.Fatal("expired record retained")
	}
	cancel()
	store.expire()
	if len(store.records) != 0 {
		t.Fatal("disconnected records retained")
	}
}
