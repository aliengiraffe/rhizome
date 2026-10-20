package channel

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"rhizome/internal/localstate"
	pb "rhizome/proto"
)

// Direct collectors hold this lock from before baselining until shutdown.
// Observers hold it only while scanning. The epoch fences missed short-lived
// direct integrations as well as normal handoffs; it contains no log contents.
func ownershipPaths(runtime, session string) (string, string, error) {
	dir := filepath.Join(runtime, "collection")
	if err := localstate.SecureDir(dir); err != nil {
		return "", "", err
	}
	name := fmt.Sprintf("%x", sha256.Sum256([]byte(session)))
	return filepath.Join(dir, name+".lock"), filepath.Join(dir, name+".epoch"), nil
}
func (n *Node) claimCollection(session string) (*os.File, error) {
	if n.runtime == "" {
		return nil, nil
	} // In-memory protocol/parser tests.
	path, epoch, err := ownershipPaths(n.runtime, session)
	if err != nil {
		return nil, err
	}
	lock, err := localstate.Lock(path, false)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(epoch, []byte(id()), 0600); err != nil {
		lock.Close()
		return nil, err
	}
	return lock, nil
}

// logicalPeers hides passive duplicates, preserving direct/direct conflicts.
func logicalPeers(peers []*pb.Identity) []*pb.Identity {
	direct := map[string]bool{}
	for _, p := range peers {
		if !p.ObservationOnly {
			direct[p.SessionId] = true
		}
	}
	out := make([]*pb.Identity, 0, len(peers))
	for _, p := range peers {
		if !p.ObservationOnly || !direct[p.SessionId] {
			out = append(out, p)
		}
	}
	return out
}
