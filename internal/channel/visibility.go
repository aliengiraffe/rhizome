package channel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/fsnotify/fsnotify"
	"rhizome/internal/localstate"
	pb "rhizome/proto"
)

var errPrivate = errors.New("session is private or its visibility policy is unavailable")

type visibilityRecord struct {
	Session     string `json:"session_id"`
	Visibility  string `json:"visibility"`
	Revision    string `json:"revision"`
	Applied     bool   `json:"applied"`
	PublicAfter int64  `json:"public_after,omitempty"`
}

func visibilityPath(runtime, session string) string {
	return filepath.Join(runtime, "visibility", fmt.Sprintf("%x.json", sha256.Sum256([]byte(session))))
}

// Missing choices preserve public-by-default behavior. Every other read error
// denies access; neither malformed records nor permission errors are defaults.
func loadVisibility(runtime, session string) (visibilityRecord, error) {
	r := visibilityRecord{Session: session, Visibility: "public"}
	if runtime == "" || session == "" {
		return r, nil
	}
	path := visibilityPath(runtime, session)
	err := localstate.PrivateFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	f, err := os.Open(path)
	if err != nil {
		return r, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 1025))
	if err != nil || len(raw) > 1024 {
		return r, errPrivate
	}
	r = visibilityRecord{}
	if json.Unmarshal(raw, &r) != nil || r.Session != session || (r.Visibility != "public" && r.Visibility != "private") || len(r.Revision) != 32 {
		return r, errPrivate
	}
	if _, err := hex.DecodeString(r.Revision); err != nil {
		return r, errPrivate
	}
	if r.Visibility == "public" && (!r.Applied || r.PublicAfter <= 0) {
		return r, errPrivate
	}
	return r, nil
}
func sessionPublic(runtime, session string) bool {
	r, err := loadVisibility(runtime, session)
	return err == nil && r.Visibility == "public"
}
func (n *Node) publicLocked() bool {
	return n.bridge != nil || sessionPublic(n.runtime, n.identity.SessionId)
}
func (n *Node) exposed() *pb.Identity {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.publicLocked() {
		return registrationRecord(n.identity)
	}
	return cloneIdentity(n.identity)
}

// VisibilityStatus reports durable intent and whether the revocation barrier
// completed. A timeout leaves private policy in place; retry to finish.
type VisibilityStatus struct {
	SessionID  string `json:"session_id"`
	Visibility string `json:"visibility"`
	Applied    bool   `json:"applied"`
}

// SessionVisibility inspects or changes a policy in this runtime only. Exact
// IDs may be preconfigured before a session starts; it never addresses a peer.
func SessionVisibility(ctx context.Context, runtime, session, value string) (out VisibilityStatus, resultErr error) {
	out = VisibilityStatus{SessionID: session}
	defer func() {
		if value != "" && resultErr != nil && out.Visibility == "private" && !out.Applied {
			resultErr = fmt.Errorf("private policy persisted; application incomplete: %w", resultErr)
		}
	}()
	if session == "" || len(session) > 128 || !utf8.ValidString(session) {
		return out, errors.New("session ID must contain 1 to 128 UTF-8 bytes")
	}
	if value != "" && value != "public" && value != "private" {
		return out, errors.New("visibility must be public or private")
	}
	if _, err := localstate.Runtime(runtime); err != nil {
		return out, err
	}
	if value == "" {
		r, err := loadVisibility(runtime, session)
		out.Visibility = r.Visibility
		if err != nil {
			out.Visibility = "private"
			return out, err
		}
		out.Applied = r.Revision == "" || r.Applied
		return out, nil
	}
	if err := localstate.SecureDir(filepath.Join(runtime, "visibility")); err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	lock, err := visibilityLock(ctx, runtime)
	if err != nil {
		return out, err
	}
	defer lock.Close()
	// Both transitions first revoke. In particular public cannot race past an
	// unacknowledged private interval and revive old feeds or retained events.
	r := visibilityRecord{Session: session, Visibility: "private", Revision: id()}
	raw, _ := json.Marshal(r)
	if err := writeRegistration(visibilityPath(runtime, session), raw); err != nil {
		return out, err
	}
	out.Visibility = "private"
	// One serialized request wakes every local node, including receivers holding
	// this origin's queued content. Registrations contain no session identity.
	if err := writeRegistration(filepath.Join(runtime, "visibility", "request"), raw); err != nil {
		return out, err
	}
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for !visibilityAcknowledged(runtime, r.Revision) {
		select {
		case <-ctx.Done():
			return out, fmt.Errorf("privacy persisted but revocation is not fully acknowledged (older or unresponsive local process): %w", ctx.Err())
		case <-tick.C:
		}
	}
	r.Visibility = value
	if value == "public" {
		r.PublicAfter = time.Now().UnixMilli()
	}
	r.Applied = true
	raw, _ = json.Marshal(r)
	if err := writeRegistration(visibilityPath(runtime, session), raw); err != nil {
		return out, err
	}
	out.Visibility, out.Applied = value, true
	return out, nil
}

func visibilityLock(ctx context.Context, runtime string) (*os.File, error) {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		f, err := localstate.Lock(filepath.Join(runtime, "visibility.lock"), false)
		if !errors.Is(err, localstate.ErrBusy) {
			return f, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
}

func visibilityAcknowledged(runtime, revision string) bool {
	if revision == "" {
		return true
	}
	entries, err := os.ReadDir(runtime)
	if err != nil {
		return false
	}
	for _, e := range entries {
		node := registryNodeID(e.Name(), ".lock")
		if node == "" {
			continue
		}
		f, err := localstate.Lock(registrationLockPath(runtime, node), false)
		if err == nil {
			f.Close()
			continue
		}
		if !errors.Is(err, localstate.ErrBusy) {
			return false
		}
		raw, err := os.ReadFile(filepath.Join(runtime, "visibility", node+".ack"))
		if err != nil || string(raw) != revision {
			return false
		}
	}
	f, err := localstate.Lock(filepath.Join(runtime, "observer.lock"), false)
	if err == nil {
		f.Close()
	} else {
		if !errors.Is(err, localstate.ErrBusy) {
			return false
		}
		raw, err := os.ReadFile(filepath.Join(runtime, "visibility", "observer.ack"))
		if err != nil || string(raw) != revision {
			return false
		}
	}
	return true
}

// startVisibility watches only the bounded runtime policy directory. The timer
// reconciles missed notifications, never agent log trees. Startup fails if the
// watcher cannot be installed; content boundaries additionally read the policy.
func (n *Node) startVisibility() error {
	dir := filepath.Join(n.runtime, "visibility")
	if err := localstate.SecureDir(dir); err != nil {
		return err
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := w.Add(dir); err != nil {
		w.Close()
		return err
	}
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		defer w.Close()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			n.syncVisibility()
			select {
			case <-n.ctx.Done():
				return
			case _, ok := <-w.Events:
				if !ok {
					return
				}
			case <-w.Errors:
			case <-tick.C:
			}
		}
	}()
	return nil
}

func (n *Node) syncVisibility() {
	defer n.reconcileVisibility()
	raw, err := os.ReadFile(filepath.Join(n.runtime, "visibility", "request"))
	var r visibilityRecord
	if err != nil || len(raw) > 1024 || json.Unmarshal(raw, &r) != nil || len(r.Revision) != 32 || r.Session == "" {
		return
	}
	n.gate.Lock()
	n.collectionGate.Lock()
	n.mu.Lock()
	if n.visibilityAck == r.Revision {
		n.mu.Unlock()
		n.gate.Unlock()
		n.collectionGate.Unlock()
		return
	}
	own := n.bridge == nil && n.identity.SessionId == r.Session
	var draining []*listener
	for l := range n.listeners {
		if own || l.subscriber == r.Session {
			draining = append(draining, l)
			close(l.done)
			delete(n.listeners, l)
		}
	}
	if own {
		n.revokeVisibilityLocked()
	} else {
		if f := n.feeds[r.Session]; f != nil {
			f.cancel()
			delete(n.feeds, r.Session)
		}
		kept := n.incoming[:0]
		for _, p := range n.incoming {
			if p.event.SessionId != r.Session {
				kept = append(kept, p)
			}
		}
		n.incoming = kept
		held, origins := n.hookInbox[:0], n.hookOrigins[:0]
		bytes := 0
		for i, envelope := range n.hookInbox {
			origin := ""
			if i < len(n.hookOrigins) {
				origin = n.hookOrigins[i]
			}
			if origin == r.Session {
				continue
			}
			held = append(held, envelope)
			origins = append(origins, origin)
			bytes += len(envelope)
		}
		n.hookInbox, n.hookOrigins, n.hookInboxBytes = held, origins, bytes
	}
	node := n.identity.NodeId
	n.mu.Unlock()
	n.gate.Unlock()
	n.collectionGate.Unlock()
	for _, l := range draining {
		l.egress.Lock()
		l.egress.Unlock()
	}
	if own {
		n.advertise()
	}
	if writeRegistration(filepath.Join(n.runtime, "visibility", node+".ack"), []byte(r.Revision)) == nil {
		n.mu.Lock()
		n.visibilityAck = r.Revision
		n.mu.Unlock()
	}
}

func (n *Node) setSessionVisibility(ctx context.Context, value string) (VisibilityStatus, error) {
	if value != "private" && value != "public" {
		return VisibilityStatus{}, errors.New("visibility must be public or private")
	}
	n.mu.Lock()
	session := n.identity.SessionId
	n.mu.Unlock()
	if session == "" {
		return VisibilityStatus{}, errUnbound
	}
	return SessionVisibility(ctx, n.runtime, session, value)
}

// The observer acknowledges only after its serial indexer has removed private
// rows from disk. No log tree is walked here: known paths are requeued on public.
func (o *observer) syncVisibility() {
	// Reconcile the bounded in-memory catalog, including corrupt policy changes
	// that have no valid control request. Never enumerate native log trees here.
	for path, f := range o.index.files {
		if f.Session != "" && !sessionPublic(o.opts.Runtime, f.Session) {
			o.index.hidden[path] = f.Session
			delete(o.index.files, path)
			o.index.dirty = true
		}
	}
	for _, f := range o.files {
		if f.session != "" && !sessionPublic(o.opts.Runtime, f.session) {
			if f.n != nil {
				f.n.close()
				f.n = nil
			}
			f.c.privacyRevision, f.c.more = "private", false
		}
	}
	if o.index.dirty {
		o.index.flush()
	}

	raw, err := os.ReadFile(filepath.Join(o.opts.Runtime, "visibility", "request"))
	var r visibilityRecord
	if err != nil || len(raw) > 1024 || json.Unmarshal(raw, &r) != nil || len(r.Revision) != 32 {
		return
	}
	for path, session := range o.index.hidden {
		if sessionPublic(o.opts.Runtime, session) {
			agent, _ := o.index.scope.root(path)
			if agent == "opencode" {
				o.index.ocDirty = true
			} else {
				o.index.enqueue(path)
			}
			delete(o.index.hidden, path)
		}
	}
	if o.visibilityAck == r.Revision {
		if o.visibilityPublished != r.Revision && sessionPublic(o.opts.Runtime, r.Session) {
			o.index.ocDirty = true
			o.visibilityPublished = r.Revision
		}
		return
	}

	for _, f := range o.files {
		if f.session == r.Session {
			if f.n != nil {
				f.n.close()
				f.n = nil
			}
			f.c.privacyRevision = "private"
			f.c.more = false
		}
	}
	for path, f := range o.index.files {
		if f.Session == r.Session {
			o.index.hidden[path] = f.Session
			delete(o.index.files, path)
		}
	}
	o.index.dirty = true
	o.index.flush()
	if o.index.dirty {
		return
	}
	if writeRegistration(filepath.Join(o.opts.Runtime, "visibility", "observer.ack"), []byte(r.Revision)) == nil {
		o.visibilityAck = r.Revision
	}
}

// reconcileVisibility also fences malformed policy, independently of the
// control request, and refreshes gossip after the final public write.
func (n *Node) reconcileVisibility() {
	n.gate.Lock()
	n.collectionGate.Lock()
	n.mu.Lock()
	public := n.publicLocked()
	var draining []*listener
	if !public && !n.visibilityBlocked {
		for l := range n.listeners {
			draining = append(draining, l)
		}
		n.revokeVisibilityLocked()
	}
	n.visibilityBlocked = !public
	changed := public != n.visibilityAdvertised
	n.visibilityAdvertised = public
	n.mu.Unlock()
	n.collectionGate.Unlock()
	n.gate.Unlock()
	for _, l := range draining {
		l.egress.Lock()
		l.egress.Unlock()
	}
	if changed {
		n.advertise()
	}
}
func (n *Node) revokeVisibilityLocked() {
	for l := range n.listeners {
		close(l.done)
		delete(n.listeners, l)
	}
	for _, f := range n.feeds {
		f.cancel()
	}
	n.feeds = map[string]*feed{}
	n.incoming, n.history, n.seenOrder, n.hookInbox = nil, nil, nil, nil
	n.historyBytes, n.hookInboxBytes = 0, 0
	n.seen, n.rearmed = map[string]bool{}, map[string]bool{}
	n.stream, n.sequence, n.directSequence = id(), 0, 0
	n.hookOrigins = nil
	n.visibilityBlocked = true
}
