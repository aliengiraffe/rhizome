// Package channel implements one session-bound Rhizome node, loopback-only by
// default and optionally peered across the local network.
package channel

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hashicorp/mdns"
	"github.com/hashicorp/memberlist"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	pb "rhizome/proto"
)

const maxText = 7 * 1024
const maxQueue = 256

var errUnbound = errors.New("UNBOUND: waiting for session hook; configure observe_session hooks and submit a prompt")

func id() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func clip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	s = s[:limit-16]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + " … [truncated]"
}

type Hook struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	Event          string `json:"hook_event_name"`
	Source         string `json:"source,omitempty"`
	AgentID        string `json:"agent_id,omitempty"`
	AgentType      string `json:"agent_type,omitempty"`
	ToolName       string `json:"tool_name,omitempty"`
}
type feed struct {
	cancel           context.CancelFunc
	status           string
	reconnects, gaps uint64
	token            string
}
type listener struct {
	queue chan *pb.Event
	done  chan struct{}
}
type pending struct {
	event        *pb.Event
	subscription string
	token        string
	generation   uint64
}
type counters struct{ Suppressed, Received, Queued, Written, Dropped, Pauses, ParserSkips, QueueHandoffs, QueueFailures, CollectionGaps uint64 }
type Node struct {
	bridge *gatewayProxy // immutable; only service-owned remote proxies

	collectionLease  *os.File
	archive          *historyService // read-only history catalog client; nil in parser tests
	suppressActivity bool
	codex            *codexCollector
	collectionState  string
	deliveryError    string
	deliveryReady    bool
	directSequence   uint64
	discovered       map[string]*pb.Identity
	pb.UnimplementedChannelServer
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// gate serializes lifecycle transitions with channel writes; collection never takes it.
	gate                         sync.Mutex
	mu                           sync.Mutex
	identity                     *pb.Identity
	names                        nameState
	retired                      map[string]bool
	stream                       string
	sequence                     uint64
	history                      []*pb.Event
	historyBytes                 int
	listeners                    map[*listener]bool
	feeds                        map[string]*feed
	incoming                     []pending
	seen                         map[string]bool
	seenOrder                    []string
	paused                       bool
	rearmed                      map[string]bool
	notifications                []time.Time
	explicit                     []time.Time
	stats                        counters
	collector                    *collector
	collectorCancel              context.CancelFunc
	ready                        bool
	notify                       func(context.Context, string, map[string]string) error
	members                      *memberlist.Memberlist
	rpc                          *grpc.Server
	mdns                         *mdns.Server
	token, runtime, registration string
	// opts, mode, bind, advertiseAddr, gossipAddr, beaconPort, ts and cert are
	// fixed by start before any goroutine reads them.
	opts                            Options
	mode                            mode
	bind, advertiseAddr, gossipAddr string
	beaconPort                      int
	ts                              tailscaleClient
	cert                            tls.Certificate
	// beacon and warned are guarded by mu.
	beacon net.Listener
	warned map[string]bool
}

func newNode(parent context.Context) *Node {
	ctx, cancel := context.WithCancel(parent)
	return &Node{discovered: map[string]*pb.Identity{}, ctx: ctx, cancel: cancel, identity: &pb.Identity{Protocol: 1, GatewayVersion: 1, NodeId: id(), AgentType: "claude", Host: shortHost()}, retired: map[string]bool{}, stream: id(), listeners: map[*listener]bool{}, feeds: map[string]*feed{}, seen: map[string]bool{}, rearmed: map[string]bool{}, warned: map[string]bool{}}
}
func cloneIdentity(i *pb.Identity) *pb.Identity { return proto.Clone(i).(*pb.Identity) }

// activitySink receives normalized transcript records. Node implements it for
// live collection; historical reads use an in-memory sink so both paths apply
// exactly the same content policy.
type activitySink interface {
	activity(gen uint64, kind, text string)
	setNativeTitle(gen uint64, custom bool, title string)
	local() *pb.Identity
	countSkip()
}

func (n *Node) countSkip() {
	n.mu.Lock()
	n.stats.ParserSkips++
	n.mu.Unlock()
}
func (n *Node) local() *pb.Identity {
	n.mu.Lock()
	defer n.mu.Unlock()
	return cloneIdentity(n.identity)
}
func (n *Node) bound() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.identity.SessionId == "" {
		return errUnbound
	}
	return nil
}
func (n *Node) observe(h Hook) error {
	if h.AgentID != "" {
		return nil
	}
	if strings.HasPrefix(h.ToolName, "mcp__rhizome__") {
		return nil
	}
	switch h.Event {
	case "SessionStart", "UserPromptSubmit", "PreToolUse", "Stop", "SessionEnd":
	default:
		return fmt.Errorf("unsupported hook event %q", h.Event)
	}
	if h.SessionID == "" || len(h.SessionID) > 128 || !filepath.IsAbs(h.TranscriptPath) || !filepath.IsAbs(h.Cwd) {
		return errors.New("hook requires session_id and absolute transcript_path/cwd")
	}
	n.gate.Lock()
	defer n.gate.Unlock()
	n.mu.Lock()
	if n.retired[h.SessionID] {
		n.mu.Unlock()
		return errors.New("stale hook for a retired session")
	}
	old := n.identity.SessionId
	if h.Event == "SessionEnd" {
		n.mu.Unlock()
		slog.Info("session_end_advisory", "session_id", h.SessionID)
		return nil
	}
	if old != "" && old != h.SessionID && h.Event != "SessionStart" && h.Event != "UserPromptSubmit" {
		n.mu.Unlock()
		return errors.New("unexpected session change on tool/stop hook")
	}
	if old == h.SessionID && n.collector != nil && n.collector.path != h.TranscriptPath {
		n.mu.Unlock()
		return errors.New("transcript path changed for current session; refusing unrelated file")
	}
	if old != h.SessionID {
		// Serialize the baseline with passive scans before advertising this binding.
		lease, err := n.claimCollection(h.SessionID)
		if err != nil {
			n.mu.Unlock()
			return err
		}
		// Establish the baseline synchronously, before any hook returns.
		c, err := newCollector(h.TranscriptPath)
		if err != nil {
			if lease != nil {
				lease.Close()
			}
			n.mu.Unlock()
			return err
		}
		if old != "" {
			n.publishLocked("session-ended", "Session binding ended", "")
			n.retired[old] = true
		}
		if n.collectorCancel != nil {
			n.collectorCancel()
		}
		for l := range n.listeners {
			close(l.done)
			delete(n.listeners, l)
		}
		for _, f := range n.feeds {
			f.cancel()
		}
		n.feeds = map[string]*feed{}
		n.stats.Dropped += uint64(len(n.incoming))
		n.incoming = nil
		n.seen = map[string]bool{}
		n.seenOrder = nil
		n.paused = false
		n.rearmed = map[string]bool{}
		n.notifications = nil
		if n.collectionLease != nil {
			n.collectionLease.Close()
		}
		n.collectionLease = lease
		n.identity.SessionId = h.SessionID
		n.identity.Generation++
		n.names = nameState{explicit: loadOverride(n.runtime, h.SessionID)}
		n.stream = id()
		n.sequence = 0
		n.directSequence = 0
		n.history = nil
		n.historyBytes = 0
		n.collector = c
		ctx, cancel := context.WithCancel(n.ctx)
		n.collectorCancel = cancel
		gen := n.identity.Generation
		n.wg.Add(1)
		go func() { defer n.wg.Done(); c.run(ctx, n, gen) }()
	}
	n.identity.Project = clip(h.Cwd, 180)
	n.identity.DisplayName = clip(filepath.Base(h.Cwd), 40)
	n.resolveNameLocked()
	if n.collector != nil {
		select {
		case n.collector.wake <- struct{}{}:
		default:
		}
	}
	generation := n.identity.Generation
	n.mu.Unlock()
	if n.members != nil {
		if err := n.members.UpdateNode(time.Second); err != nil {
			slog.Warn("discovery_update", "error", err)
		}
	}
	slog.Info("binding", "session_id", h.SessionID, "generation", generation, "hook", h.Event, "source", h.Source)
	return nil
}
func (n *Node) evictLocked(now time.Time) {
	for len(n.history) > 0 && (len(n.history) > 1000 || n.historyBytes > 8*1024*1024 || now.Sub(time.UnixMilli(n.history[0].UnixMillis)) > 5*time.Minute) {
		n.historyBytes -= proto.Size(n.history[0])
		n.history = n.history[1:]
	}
}
func (n *Node) publishLocked(kind, text, reply string) *pb.Event {
	n.sequence++
	e := &pb.Event{SessionId: n.identity.SessionId, NodeId: n.identity.NodeId, Generation: n.identity.Generation, StreamId: n.stream, Sequence: n.sequence, Kind: kind, UnixMillis: time.Now().UnixMilli(), Text: clip(text, maxText), MessageId: id(), ReplyTo: clip(reply, 128)}
	n.history = append(n.history, e)
	n.historyBytes += proto.Size(e)
	n.evictLocked(time.Now())
	for l := range n.listeners {
		select {
		case l.queue <- e:
		default:
			close(l.done)
			delete(n.listeners, l)
			slog.Warn("subscriber_overflow")
		}
	}
	return e
}
func (n *Node) activity(gen uint64, kind, text string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.suppressActivity {
		n.stats.Suppressed++
		return
	}
	if n.identity.Generation == gen && n.identity.SessionId != "" {
		n.publishLocked(kind, text, "")
	}
}
func prune(times []time.Time, now time.Time) []time.Time {
	i := 0
	for i < len(times) && now.Sub(times[i]) >= time.Minute {
		i++
	}
	return times[i:]
}
func (n *Node) rateLocked() error {
	n.explicit = prune(n.explicit, time.Now())
	if len(n.explicit) >= 10 {
		return errors.New("rate limit: at most ten publish/send calls per minute")
	}
	n.explicit = append(n.explicit, time.Now())
	return nil
}
func (n *Node) announce(text, reply string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.identity.SessionId == "" {
		return errUnbound
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("text must not be empty")
	}
	if err := n.rateLocked(); err != nil {
		return err
	}
	n.publishLocked("announcement", text, reply)
	return nil
}
func (n *Node) close() {
	n.cancel()
	if n.mdns != nil {
		_ = n.mdns.Shutdown()
	}
	if n.rpc != nil {
		n.rpc.Stop()
	}
	n.mu.Lock()
	if n.beacon != nil {
		_ = n.beacon.Close()
	}
	n.mu.Unlock()
	if n.members != nil {
		// The WAN gossip interval is 500ms; give the leave time to go out.
		leave := 300 * time.Millisecond
		if n.mode == modeTailscale {
			leave = time.Second
		}
		_ = n.members.Leave(leave)
		_ = n.members.Shutdown()
	}
	n.wg.Wait()
	if n.codex != nil {
		n.codex.reader.close()
	}
	if n.collectionLease != nil {
		n.collectionLease.Close()
	}
	n.unregister()
}
