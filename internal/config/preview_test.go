package config

import (
	"os"
	"testing"
)

func TestPreviewUsesResolverWithoutWriting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, body := range []string{`unknown = true`, `runtime_dir = "relative"`, `network.mode = "invalid"`, `hostname = "bad\u001b"`} {
		if _, err := Preview(body); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	c, err := Preview("network.mode = 'tailscale'\nnetwork.gossip_port = 7950\n")
	if err != nil || c.Network.GossipPort != 7950 || c.Sources["network.mode"] != "file" {
		t.Fatal(c, err)
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 0 {
		t.Fatal("preview wrote files", entries, err)
	}
}
