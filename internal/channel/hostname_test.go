package channel

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	pb "rhizome/proto"
)

func TestHostnameIdentity(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		for _, mode := range []string{"local", "lan", "tailscale"} {
			for _, hostname := range []string{"", "  同じ device  "} {
				t.Run(agent+"/"+mode+"/"+hostname, func(t *testing.T) {
					n := newNode(context.Background())
					defer n.close()
					n.identity.AgentType = agent
					o := Options{Runtime: filepath.Join(t.TempDir(), "run"), Hostname: hostname}
					if mode != "local" {
						o.BindAddr, o.AdvertiseAddr = "127.0.0.1", "127.0.0.1"
					}
					o.LAN = mode == "lan"
					if mode == "tailscale" {
						o.Tailscale = true
						o.GossipPort = freePort(t)
						o.tailscale = &fakeTailscale{st: loopbackStatus("different-tailnet-name", nil, nil)}
					}
					if err := n.start(o); err != nil {
						t.Fatal(err)
					}
					want := strings.TrimSpace(hostname)
					if want == "" {
						want = shortHost()
					}
					var advertised pb.Identity
					if err := json.Unmarshal(n.NodeMeta(512), &advertised); err != nil {
						t.Fatal(err)
					}
					peer := newNode(context.Background())
					defer peer.close()
					peer.mode, peer.token = n.mode, n.token
					actual, err := peer.probe(context.Background(), &advertised)
					if err != nil {
						t.Fatal(err)
					}
					if advertised.Host != want || actual.Host != want || actual.NodeId != n.local().NodeId {
						t.Fatalf("advertised %+v, actual %+v", &advertised, actual)
					}
				})
			}
		}
	}
}

func TestHostnameGossipTrimsRunes(t *testing.T) {
	n := newNode(context.Background())
	defer n.close()
	n.identity.Host = strings.Repeat("界", 21)
	full := n.NodeMeta(10000)
	for limit := len(full) - 1; limit >= len(full)-63; limit-- {
		b := n.NodeMeta(limit)
		var got pb.Identity
		if len(b) > limit || !utf8.Valid(b) || json.Unmarshal(b, &got) != nil || strings.ContainsRune(got.Host, '\ufffd') {
			t.Fatalf("limit %d: %s", limit, b)
		}
	}
}

func TestObserverHostname(t *testing.T) {
	o := observerHarness(t)
	o.opts.Hostname = "Observer device"
	path := filepath.Join(o.opts.ClaudeRoot, "session.jsonl")
	appendLog(t, path, claudeRecord("session", "old", "historical"))
	o.inventory(true)
	appendLog(t, path, claudeRecord("session", "new", "new activity"))
	o.visit(path, false)
	if f := o.files[path]; f == nil || f.n == nil || f.n.local().Host != o.opts.Hostname {
		t.Fatal("observer hostname was not propagated")
	}
}
