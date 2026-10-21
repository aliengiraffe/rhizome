package channel

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"rhizome/internal/localstate"
	pb "rhizome/proto"
)

// Local discovery keeps one registration per live node in the runtime
// directory: <node_id>.json carries the routing fields peers need to probe
// and join it, and <node_id>.lock is an OS lock the node holds for its whole
// lifetime. The kernel releases the lock when the process exits, even after a
// crash, so an acquirable lock is the only proof that a registration is stale.
// A failed or slow probe never is: a live process can be briefly unreachable
// under load, and deleting its registration would hide it from every node
// started afterwards.

// defaultRegistrationRefresh is how often a live node verifies its own
// registration and restores it when it was removed or altered.
const defaultRegistrationRefresh = 30 * time.Second

func registrationPath(runtime, nodeID string) string {
	return filepath.Join(runtime, nodeID+".json")
}
func registrationLockPath(runtime, nodeID string) string {
	return filepath.Join(runtime, nodeID+".lock")
}

// registryNodeID returns the node ID of a registry file name with the given
// suffix, or "" for any other runtime file.
func registryNodeID(base, suffix string) string {
	if len(base) != 32+len(suffix) || base[32:] != suffix {
		return ""
	}
	if _, err := hex.DecodeString(base[:32]); err != nil {
		return ""
	}
	return base[:32]
}

// registrationRecord keeps only what bootstrap needs to probe and join a node:
// no session, project or name labels are written to the registry.
func registrationRecord(i *pb.Identity) *pb.Identity {
	return &pb.Identity{Protocol: i.Protocol, GatewayVersion: i.GatewayVersion, GatewayHost: i.GatewayHost, NodeId: i.NodeId, Endpoint: i.Endpoint, GossipEndpoint: i.GossipEndpoint, CertFingerprint: i.CertFingerprint}
}

// writeRegistration replaces path atomically so readers never observe a
// partial record. The temporary name never matches the registry pattern.
func writeRegistration(path string, raw []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".registration-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(raw)
	if e := f.Close(); err == nil {
		err = e
	}
	if err == nil {
		err = os.Chmod(tmp, 0600)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

// register takes the node's registration lock, publishes its registration and
// starts the self-healing loop. The caller holds the registry lock.
func (n *Node) register(runtime string) error {
	nodeID := n.local().NodeId
	lock, err := localstate.Lock(registrationLockPath(runtime, nodeID), false)
	if err != nil {
		return err
	}
	n.registryLock = lock
	n.registration = registrationPath(runtime, nodeID)
	raw, _ := json.Marshal(registrationRecord(n.local()))
	if err := writeRegistration(n.registration, raw); err != nil {
		return err
	}
	every := n.registrationRefresh
	if every <= 0 {
		every = defaultRegistrationRefresh
	}
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-n.ctx.Done():
				return
			case <-ticker.C:
				n.healRegistration()
			}
		}
	}()
	return nil
}

// healRegistration restores this node's registration when it is missing or no
// longer matches the advertised routing identity. close cancels the context
// and waits for the loop before unregistering, so a closed node never
// resurrects its registration.
func (n *Node) healRegistration() {
	if n.ctx.Err() != nil || n.registration == "" {
		return
	}
	want := registrationRecord(n.local())
	raw, err := os.ReadFile(n.registration)
	var got pb.Identity
	if err == nil && json.Unmarshal(raw, &got) == nil && proto.Equal(&got, want) {
		return
	}
	reason := "mismatch"
	if errors.Is(err, os.ErrNotExist) {
		reason = "missing"
	}
	raw, _ = json.Marshal(want)
	if err := writeRegistration(n.registration, raw); err != nil {
		slog.Warn("registration_restore_failed", "node_id", want.NodeId, "reason", reason, "error", err)
		return
	}
	slog.Info("registration_restored", "node_id", want.NodeId, "reason", reason)
}

// unregister removes the registration before releasing its lock, so no
// bootstrap can observe a registration whose lock looks free while the owner
// is still running. On Windows an open lock file cannot be deleted; a failed
// removal leaves an orphan lock that a later bootstrap sweeps.
func (n *Node) unregister() {
	if n.registration != "" {
		_ = os.Remove(n.registration)
	}
	if n.registryLock != nil {
		path := n.registryLock.Name()
		_ = n.registryLock.Close()
		n.registryLock = nil
		_ = os.Remove(path)
	}
}

// Bootstrap bounds: probes run concurrently outside the registry lock, and a
// live owner whose probe failed is not re-probed by this process for a while,
// so one stalled process cannot tax every node start in the runtime.
const (
	bootstrapProbes = 8
	unreachableTTL  = 30 * time.Second
	maxUnreachable  = 256
)

// unreachable is the process-local negative cache of live registrations
// whose probe failed recently, keyed by node ID.
var unreachable = struct {
	sync.Mutex
	at map[string]time.Time
}{at: map[string]time.Time{}}

func recentlyUnreachable(nodeID string) bool {
	unreachable.Lock()
	defer unreachable.Unlock()
	at, ok := unreachable.at[nodeID]
	if ok && time.Since(at) >= unreachableTTL {
		delete(unreachable.at, nodeID)
		ok = false
	}
	return ok
}
func markUnreachable(nodeID string, failed bool) {
	unreachable.Lock()
	defer unreachable.Unlock()
	if !failed {
		delete(unreachable.at, nodeID)
		return
	}
	if len(unreachable.at) >= maxUnreachable {
		for k, at := range unreachable.at {
			if time.Since(at) >= unreachableTTL {
				delete(unreachable.at, k)
			}
		}
	}
	if len(unreachable.at) < maxUnreachable {
		unreachable.at[nodeID] = time.Now()
	}
}

type bootstrapCounts struct {
	registrations, joined, unreachable, recent, unreadable, removed, otherMode, orphanLocks int
}

// registryCandidate is a parseable registration to probe. stale holds the
// owner's lock when it was free (the owner exited, or predates registration
// locks); it is nil while a live owner holds the lock.
type registryCandidate struct {
	nodeID string
	peer   *pb.Identity
	stale  *os.File
}

// scanRegistry makes the file-system decisions of bootstrap and must run
// under the registry lock: it lists registrations, deletes unreadable ones
// whose owner is gone, sweeps free orphan locks, and returns the rest to
// probe. Taking a free owner lock creates <node_id>.lock beside registrations
// of other-mode or pre-lock processes; such a lock is swept once its
// registration is gone.
func (n *Node) scanRegistry(runtime string, c *bootstrapCounts) ([]registryCandidate, error) {
	files, err := filepath.Glob(filepath.Join(runtime, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []registryCandidate
	for _, path := range files {
		nodeID := registryNodeID(filepath.Base(path), ".json")
		if nodeID == "" {
			continue
		}
		c.registrations++
		if cand, ok := n.scanRegistration(runtime, nodeID, c); ok {
			out = append(out, cand)
		}
	}
	locks, err := filepath.Glob(filepath.Join(runtime, "*.lock"))
	if err != nil {
		releaseCandidates(out)
		return nil, err
	}
	for _, path := range locks {
		nodeID := registryNodeID(filepath.Base(path), ".lock")
		if nodeID == "" {
			continue
		}
		if _, err := os.Lstat(registrationPath(runtime, nodeID)); err == nil {
			continue
		}
		// A lock without a registration belongs to a node that is starting
		// (and holds it) or to one that is gone.
		if lock, err := localstate.Lock(path, false); err == nil {
			_ = lock.Close()
			if os.Remove(path) == nil {
				c.orphanLocks++
			}
		}
	}
	return out, nil
}

func releaseCandidates(cands []registryCandidate) {
	for _, cand := range cands {
		if cand.stale != nil {
			_ = cand.stale.Close()
		}
	}
}

// removeRegistration deletes a registration whose owner lock this process
// holds, then the lock itself. The held lock, not the registry lock, is what
// makes this safe: a live owner never releases its lock before removing its
// registration.
func removeRegistration(runtime, nodeID string, stale *os.File) {
	_ = os.Remove(registrationPath(runtime, nodeID))
	_ = stale.Close()
	_ = os.Remove(registrationLockPath(runtime, nodeID))
}

func (n *Node) scanRegistration(runtime, nodeID string, c *bootstrapCounts) (registryCandidate, bool) {
	path := registrationPath(runtime, nodeID)
	stale, err := localstate.Lock(registrationLockPath(runtime, nodeID), false)
	if err != nil {
		stale = nil
		if !errors.Is(err, localstate.ErrBusy) {
			// The lock cannot be inspected: never treat the owner as gone.
			slog.Warn("bootstrap_lock", "node_id", nodeID, "error", err)
		}
	}
	raw, err := os.ReadFile(path)
	var peer pb.Identity
	if err != nil || json.Unmarshal(raw, &peer) != nil || peer.NodeId != nodeID {
		if stale != nil {
			removeRegistration(runtime, nodeID, stale)
			c.removed++
		} else if !errors.Is(err, os.ErrNotExist) {
			c.unreadable++
		}
		return registryCandidate{}, false
	}
	if !n.allowed(peer.Endpoint) || (peer.CertFingerprint != "") != (n.mode != modeLocal) {
		// A registration from the other discovery mode may be live; it is
		// unreachable for us but not stale, so leave it in place.
		if stale != nil {
			_ = stale.Close()
		}
		slog.Info("bootstrap_skip", "node_id", nodeID, "reason", "discovery mode mismatch")
		c.otherMode++
		return registryCandidate{}, false
	}
	return registryCandidate{nodeID: nodeID, peer: &peer, stale: stale}, true
}

// joinRegistered probes candidates concurrently (at most bootstrapProbes at
// once, each with the usual probe timeout) without the registry lock, joins
// the live ones, and deletes a registration only when its owner lock was free
// and the probe failed. A free lock with a successful probe is a live process
// that predates registration locks, so it is kept and joined.
func (n *Node) joinRegistered(runtime string, cands []registryCandidate, c *bootstrapCounts) {
	results := make([]*pb.Identity, len(cands))
	probed := make([]bool, len(cands))
	sem := make(chan struct{}, bootstrapProbes)
	var wg sync.WaitGroup
	for i, cand := range cands {
		if cand.stale == nil && recentlyUnreachable(cand.nodeID) {
			c.recent++
			continue
		}
		probed[i] = true
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if actual, err := n.probe(n.ctx, cand.peer); err == nil {
				results[i] = actual
			}
		}()
	}
	wg.Wait()
	var endpoints []string
	for i, cand := range cands {
		switch {
		case !probed[i]:
		case results[i] == nil && cand.stale != nil:
			removeRegistration(runtime, cand.nodeID, cand.stale)
			cand.stale = nil
			c.removed++
		case results[i] == nil:
			// The owner is alive; it may be briefly overloaded. Skip it now and
			// let gossip from other members, or a later bootstrap, find it.
			markUnreachable(cand.nodeID, true)
			slog.Info("bootstrap_skip", "node_id", cand.nodeID, "reason", "probe failed; owner alive")
			c.unreachable++
		default:
			markUnreachable(cand.nodeID, false)
			endpoints = append(endpoints, results[i].GossipEndpoint)
		}
		if cand.stale != nil {
			_ = cand.stale.Close()
		}
	}
	if len(endpoints) > 0 {
		joined, err := n.members.Join(endpoints)
		if err != nil {
			slog.Warn("bootstrap_join", "error", err)
		}
		c.joined = joined
	}
}

func (c bootstrapCounts) log() {
	slog.Info("bootstrap", "registrations", c.registrations, "joined", c.joined, "unreachable_live", c.unreachable, "skipped_recently_unreachable", c.recent, "unreadable_live", c.unreadable, "removed_stale", c.removed, "other_mode", c.otherMode, "orphan_locks_removed", c.orphanLocks)
}
