package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"rhizome/internal/localstate"
	pb "rhizome/proto"
)

// bootstrapIn starts and closes a node in runtime, which runs one bootstrap.
func bootstrapIn(t *testing.T, runtime string) {
	t.Helper()
	n := newNode(context.Background())
	defer n.close()
	if err := n.start(Options{Runtime: runtime}); err != nil {
		t.Fatal(err)
	}
}

// refusedEndpoint returns a loopback address with no listener.
func refusedEndpoint(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// silentEndpoint completes TCP handshakes (kernel backlog) but never answers,
// so a probe runs into its deadline.
func silentEndpoint(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l.Addr().String()
}

func writeFakeRegistration(t *testing.T, runtime, nodeID, endpoint string, garbage bool) {
	t.Helper()
	raw, _ := json.Marshal(&pb.Identity{Protocol: 1, GatewayVersion: 1, NodeId: nodeID, Endpoint: endpoint, GossipEndpoint: endpoint})
	if garbage {
		raw = []byte(`{"node_id":`)
	}
	if err := os.WriteFile(registrationPath(runtime, nodeID), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

// TestBootstrapPrunesOnlyUnlockedRegistrations proves that a registration is
// removed only when its owner's lock is free, never because a probe failed or
// timed out, and that it is removed once the owner releases the lock.
func TestBootstrapPrunesOnlyUnlockedRegistrations(t *testing.T) {
	cases := []struct {
		name     string
		endpoint func(*testing.T) string
		garbage  bool
		locked   bool
		kept     bool
	}{
		{name: "live owner, probe refused", endpoint: refusedEndpoint, locked: true, kept: true},
		{name: "live owner, probe timeout", endpoint: silentEndpoint, locked: true, kept: true},
		{name: "live owner, mid-write record", endpoint: refusedEndpoint, garbage: true, locked: true, kept: true},
		{name: "owner gone, probe refused", endpoint: refusedEndpoint},
		{name: "owner gone, probe timeout", endpoint: silentEndpoint},
		{name: "owner gone, unreadable record", endpoint: refusedEndpoint, garbage: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runtime := filepath.Join(t.TempDir(), "run")
			if err := localstate.SecureDir(runtime); err != nil {
				t.Fatal(err)
			}
			nodeID := id()
			lockPath := registrationLockPath(runtime, nodeID)
			var owner *os.File
			if tc.locked {
				var err error
				if owner, err = localstate.Lock(lockPath, false); err != nil {
					t.Fatal(err)
				}
				defer owner.Close()
			}
			writeFakeRegistration(t, runtime, nodeID, tc.endpoint(t), tc.garbage)
			bootstrapIn(t, runtime)
			if got := exists(registrationPath(runtime, nodeID)); got != tc.kept {
				t.Fatalf("registration kept=%v, want %v", got, tc.kept)
			}
			if !tc.kept {
				if exists(lockPath) {
					t.Fatal("stale lock file left behind")
				}
				return
			}
			// The owner exits: the next bootstrap prunes both files.
			owner.Close()
			bootstrapIn(t, runtime)
			if exists(registrationPath(runtime, nodeID)) || exists(lockPath) {
				t.Fatal("registration of an exited owner survived bootstrap")
			}
		})
	}
}

// TestBootstrapKeepsAnsweringUnlockedPeer covers a live process that predates
// registration locks: its lock is free but it answers probes, so bootstrap
// joins it and keeps its registration.
func TestBootstrapKeepsAnsweringUnlockedPeer(t *testing.T) {
	runtime := filepath.Join(t.TempDir(), "run")
	legacy := node(t)
	if err := legacy.start(Options{Runtime: runtime}); err != nil {
		t.Fatal(err)
	}
	path := legacy.registration
	lockPath := legacy.registryLock.Name()
	legacy.registryLock.Close()
	legacy.registryLock = nil
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	other := node(t)
	if err := other.start(Options{Runtime: runtime}); err != nil {
		t.Fatal(err)
	}
	if !exists(path) {
		t.Fatal("answering peer's registration removed")
	}
	gatewayUntil(t, func() bool {
		for _, p := range other.peers() {
			if p.NodeId == legacy.local().NodeId {
				return true
			}
		}
		return false
	})
}

// TestBootstrapSweepsOnlyFreeOrphanLocks covers locks without registrations:
// a starting node holds its lock before writing the registration, so a held
// orphan lock is kept and a free one is removed.
func TestBootstrapSweepsOnlyFreeOrphanLocks(t *testing.T) {
	runtime := filepath.Join(t.TempDir(), "run")
	if err := localstate.SecureDir(runtime); err != nil {
		t.Fatal(err)
	}
	held, free := registrationLockPath(runtime, id()), registrationLockPath(runtime, id())
	owner, err := localstate.Lock(held, false)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	gone, err := localstate.Lock(free, false)
	if err != nil {
		t.Fatal(err)
	}
	gone.Close()
	bootstrapIn(t, runtime)
	if !exists(held) {
		t.Fatal("held lock removed")
	}
	if exists(free) {
		t.Fatal("orphan lock kept")
	}
}

// TestRegistrationSelfHeals proves a live node restores a deleted or altered
// registration and a closed node never resurrects it.
func TestRegistrationSelfHeals(t *testing.T) {
	runtime := filepath.Join(t.TempDir(), "run")
	n := newNode(context.Background())
	var closeOnce sync.Once
	closeNode := func() { closeOnce.Do(n.close) }
	t.Cleanup(closeNode)
	n.registrationRefresh = 10 * time.Millisecond
	if err := n.start(Options{Runtime: runtime}); err != nil {
		t.Fatal(err)
	}
	path, lockPath := n.registration, n.registryLock.Name()
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record pb.Identity
	if json.Unmarshal(want, &record) != nil || record.NodeId != n.local().NodeId || record.Endpoint != n.local().Endpoint || record.SessionId != "" {
		t.Fatalf("registration record: %s", want)
	}
	restored := func() bool { got, err := os.ReadFile(path); return err == nil && bytes.Equal(got, want) }
	for _, damage := range []func() error{
		func() error { return os.Remove(path) },
		func() error { return writeRegistration(path, []byte(`{"node_id":"other"}`)) },
	} {
		if err := damage(); err != nil {
			t.Fatal(err)
		}
		gatewayUntil(t, restored)
	}
	closeNode()
	if exists(path) || exists(lockPath) {
		t.Fatal("close left registration files")
	}
	// A refresh racing shutdown must not resurrect the registration.
	n.healRegistration()
	if exists(path) {
		t.Fatal("closed node resurrected its registration")
	}
	if l, err := localstate.Lock(lockPath, false); err != nil {
		t.Fatalf("closed node still holds its lock: %v", err)
	} else {
		l.Close()
	}
}

// TestRegistrationLockHeldWhileLive proves the lock reflects liveness.
func TestRegistrationLockHeldWhileLive(t *testing.T) {
	n := node(t)
	if err := n.start(Options{Runtime: filepath.Join(t.TempDir(), "run")}); err != nil {
		t.Fatal(err)
	}
	if l, err := localstate.Lock(n.registryLock.Name(), false); !errors.Is(err, localstate.ErrBusy) {
		if l != nil {
			l.Close()
		}
		t.Fatalf("live registration lock acquirable: %v", err)
	}
}

// TestBootstrapProbesConcurrentlyOutsideRegistryLock proves that several live
// but unresponsive peers cost about one probe timeout, not one per peer, that
// the registry lock is free while they are probed, and that the failures are
// cached so later bootstraps in this process do not pay for them again.
func TestBootstrapProbesConcurrentlyOutsideRegistryLock(t *testing.T) {
	runtime := filepath.Join(t.TempDir(), "run")
	if err := localstate.SecureDir(runtime); err != nil {
		t.Fatal(err)
	}
	var peers []string
	for range bootstrapProbes {
		nodeID := id()
		owner, err := localstate.Lock(registrationLockPath(runtime, nodeID), false)
		if err != nil {
			t.Fatal(err)
		}
		defer owner.Close()
		writeFakeRegistration(t, runtime, nodeID, silentEndpoint(t), false)
		peers = append(peers, nodeID)
	}
	n := node(t)
	self := n.local().NodeId // start mutates the identity; read it first
	done := make(chan error, 1)
	began := time.Now()
	go func() { done <- n.start(Options{Runtime: runtime}) }()
	// The node registers under the registry lock and then probes without it.
	gatewayUntil(t, func() bool { return exists(registrationPath(runtime, self)) })
	gatewayUntil(t, func() bool {
		l, err := localstate.Lock(filepath.Join(runtime, "registry.lock"), false)
		if err != nil {
			return false
		}
		l.Close()
		return true
	})
	select {
	case err := <-done:
		t.Fatalf("start finished before the registry lock was observed free: %v", err)
	default:
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Serial probing would take bootstrapProbes x 700ms.
	if elapsed := time.Since(began); elapsed > 3*time.Second {
		t.Fatalf("bootstrap took %v", elapsed)
	}
	for _, nodeID := range peers {
		if !exists(registrationPath(runtime, nodeID)) {
			t.Fatal("live registration removed")
		}
		if !recentlyUnreachable(nodeID) {
			t.Fatal("probe failure not cached")
		}
	}
}

// TestUnreachableCacheBounds checks expiry-free bounding of the negative cache.
func TestUnreachableCacheBounds(t *testing.T) {
	unreachable.Lock()
	saved := unreachable.at
	unreachable.at = map[string]time.Time{}
	unreachable.Unlock()
	t.Cleanup(func() { unreachable.Lock(); unreachable.at = saved; unreachable.Unlock() })
	for range maxUnreachable + 10 {
		markUnreachable(id(), true)
	}
	unreachable.Lock()
	size := len(unreachable.at)
	unreachable.Unlock()
	if size != maxUnreachable {
		t.Fatalf("cache size %d", size)
	}
	nodeID := id()
	unreachable.Lock()
	unreachable.at = map[string]time.Time{nodeID: time.Now().Add(-unreachableTTL)}
	unreachable.Unlock()
	if recentlyUnreachable(nodeID) {
		t.Fatal("expired entry still skipped")
	}
	markUnreachable(nodeID, true)
	markUnreachable(nodeID, false)
	if recentlyUnreachable(nodeID) {
		t.Fatal("successful probe did not clear the entry")
	}
}
