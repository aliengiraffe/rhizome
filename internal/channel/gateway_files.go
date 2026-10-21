package channel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"rhizome/internal/localstate"
	pb "rhizome/proto"
)

const fileLimit = 16 << 20
const fileChunk = 128 << 10
const fileCount = 32

// FileTransfer describes an offer, never a peer-selected local path.
type FileTransfer struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Size             int64     `json:"size"`
	SHA256           string    `json:"sha256"`
	Sender           string    `json:"sender"`
	Recipient        string    `json:"recipient"`
	State            string    `json:"state"`
	Expires          time.Time `json:"expires"`
	Path             string    `json:"path,omitempty"`
	ReceiptConfirmed bool      `json:"receipt_confirmed"`
}
type fileRecord struct {
	ctx            context.Context
	cancel         context.CancelFunc
	mu             sync.Mutex
	info           FileTransfer
	link           *gatewayLink
	source, target *pb.Identity // original sender and recipient wire identities
	outgoing       bool
	staged         string
}
type gatewayFiles struct {
	closed  bool
	mu      sync.Mutex
	records map[string]*fileRecord
	dir     string
	slots   chan struct{}
}
type fileWire struct {
	Action string       `json:"action"`
	Offer  FileTransfer `json:"offer,omitempty"`
	Source *pb.Identity `json:"source,omitempty"`
	Target *pb.Identity `json:"target,omitempty"`
	ID     string       `json:"id,omitempty"`
	Offset int64        `json:"offset,omitempty"`
	Data   []byte       `json:"data,omitempty"`
}

func newGatewayFiles(runtime string) (*gatewayFiles, error) {
	dir := filepath.Join(runtime, "transfers")
	if err := localstate.SecureDir(dir); err != nil {
		return nil, err
	}
	// A gateway owns this ephemeral inbox under its service lock. Completed files
	// must be copied out before shutdown; leftovers after a crash are removed.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "transfer-") {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				return nil, err
			}
		}
	}
	return &gatewayFiles{records: map[string]*fileRecord{}, dir: dir, slots: make(chan struct{}, 2)}, nil
}
func (f *gatewayFiles) close() {
	f.mu.Lock()
	f.closed = true
	records := make([]*fileRecord, 0, len(f.records))
	for _, r := range f.records {
		r.cancel()
		records = append(records, r)
	}
	f.mu.Unlock()
	for _, r := range records {
		r.mu.Lock()
		os.RemoveAll(filepath.Dir(r.staged))
		r.mu.Unlock()
	}
}
func (f *gatewayFiles) expire() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, r := range f.records {
		if !r.mu.TryLock() {
			continue
		}
		if time.Now().After(r.info.Expires) || r.link.ctx.Err() != nil {
			r.cancel()
			os.RemoveAll(filepath.Dir(r.staged))
			delete(f.records, id)
		}
		r.mu.Unlock()
	}
}
func safeFileName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 200 && utf8.ValidString(name) && !strings.ContainsAny(name, "/\\:") && strings.IndexFunc(name, unicode.IsControl) < 0
}
func (f *gatewayFiles) add(r *fileRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("gateway stopped")
	}
	if len(f.records) >= fileCount {
		return errors.New("file transfer quota reached (32 files, 512 MiB maximum)")
	}
	if f.records[r.info.ID] != nil {
		return errors.New("duplicate transfer")
	}
	dir, err := os.MkdirTemp(f.dir, "transfer-")
	if err != nil {
		return err
	}
	r.ctx, r.cancel = context.WithCancel(r.link.ctx)
	r.staged = filepath.Join(dir, "payload")
	f.records[r.info.ID] = r
	return nil
}
func (f *gatewayFiles) get(id string) *fileRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.records[id]
}
func (g *gateway) fileLink(host string) (*gatewayLink, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	l := g.links[host]
	p, ok := g.state.Peers[host]
	if !ok || p.Pending || l == nil || l.grant != p.Grant || l.ctx.Err() != nil {
		return nil, errors.New("peer disconnected or revoked")
	}
	return l, nil
}
func (l *gatewayLink) fileRequest(ctx context.Context, in fileWire) (fileWire, error) {
	var out fileWire
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(l.ctx, cancel)
	defer stop()
	l.g.mu.Lock()
	p := l.g.state.Peers[l.host]
	l.g.mu.Unlock()
	if p.Grant != l.grant || p.Pending {
		return out, errors.New("peer revoked")
	}
	b, err := json.Marshal(in)
	if err != nil {
		return out, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "https://"+p.Endpoint+"/v1/files", bytes.NewReader(b))
	if err != nil {
		return out, err
	}
	tr := &http.Transport{TLSClientConfig: l.g.clientTLS(l.host), Proxy: nil}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}
	res, err := client.Do(req)
	if err != nil {
		return out, errors.New("file request failed")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return out, fmt.Errorf("file request rejected (HTTP %d); both hosts need file-transfer support", res.StatusCode)
	}
	err = json.NewDecoder(io.LimitReader(res.Body, 256<<10)).Decode(&out)
	return out, err
}
func (g *gateway) fileHTTP(w http.ResponseWriter, r *http.Request) {
	l, err := g.fileLink(gatewayCaller(r))
	if err != nil {
		http.Error(w, "untrusted peer", 403)
		return
	}
	var in fileWire
	if r.Method != "POST" || json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in) != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	stop := context.AfterFunc(l.ctx, cancel)
	defer stop()
	out, err := l.serveFile(ctx, in)
	if err != nil {
		http.Error(w, "file request rejected", 403)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
func (l *gatewayLink) serveFile(ctx context.Context, in fileWire) (fileWire, error) {
	var out fileWire
	if in.Action == "offer" {
		o := in.Offer
		if !validFingerprint(o.ID) || !validFingerprint(o.SHA256) || !safeFileName(o.Name) || o.Size < 0 || o.Size > fileLimit || o.Expires.Before(time.Now()) || o.Expires.After(time.Now().Add(time.Hour)) {
			return out, errors.New("invalid offer")
		}
		src, dst, err := l.localRoute(ctx, in.Source, in.Target)
		if err != nil {
			return out, err
		}
		if src.ObservationOnly || dst.ObservationOnly || o.Sender != src.SessionId || o.Recipient != dst.SessionId {
			return out, errors.New("invalid file sessions")
		}
		o.State = "offered"
		o.Path = ""
		o.ReceiptConfirmed = false
		rec := &fileRecord{info: o, link: l, source: cloneIdentity(in.Source), target: cloneIdentity(in.Target)}
		if err := l.g.files.add(rec); err != nil {
			return out, err
		}
		out.Offer = o
		conn, e := l.g.base.dial(dst)
		if e == nil {
			defer conn.Close()
			text := fmt.Sprintf("File offered: %s (%d bytes), SHA-256 %s, transfer %s. Use list_files and accept_file only with user approval. Expires %s.", o.Name, o.Size, o.SHA256, o.ID, o.Expires.Format(time.RFC3339))
			_, e = pb.NewChannelClient(conn).Send(l.g.base.outgoing(ctx), &pb.SendRequest{Sender: src, Target: dst, Event: &pb.Event{SessionId: src.SessionId, NodeId: src.NodeId, Generation: src.Generation, Kind: "direct", MessageId: id(), StreamId: id(), Sequence: 1, UnixMillis: time.Now().UnixMilli(), Text: text, GatewayHost: l.host, GatewayGrant: l.grant}})
		}
		// The offer remains listable even when the recipient's notification queue is full.
		return out, nil
	}
	rec := l.g.files.get(in.ID)
	if rec == nil {
		return out, errors.New("unknown transfer")
	}
	if in.Action == "cancel" && rec.link == l {
		rec.cancel()
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.link != l || time.Now().After(rec.info.Expires) || l.ctx.Err() != nil {
		return out, errors.New("expired transfer")
	}
	source, target := rec.target, rec.source
	if !rec.outgoing {
		source, target = rec.source, rec.target
	}
	if _, _, err := l.localRoute(ctx, source, target); err != nil {
		return out, err
	}
	switch in.Action {
	case "chunk":
		if rec.ctx.Err() != nil {
			return out, errors.New("transfer cancelled")
		}
		if !rec.outgoing || rec.info.State != "offered" || in.Offset < 0 || in.Offset > rec.info.Size {
			return out, errors.New("invalid download")
		}
		f, err := os.Open(rec.staged)
		if err != nil {
			return out, err
		}
		defer f.Close()
		size := min(int64(fileChunk), rec.info.Size-in.Offset)
		out.Data = make([]byte, size)
		_, err = f.ReadAt(out.Data, in.Offset)
		if err != nil && err != io.EOF {
			return out, err
		}
	case "verified":
		if !rec.outgoing || rec.info.State != "offered" {
			return out, errors.New("invalid receipt")
		}
		rec.info.State = "verified"
		os.Remove(rec.staged)
	case "cancel":
		rec.info.Path = ""
		rec.info.State = "cancelled"
		os.RemoveAll(filepath.Dir(rec.staged))
	default:
		return out, errors.New("unknown file operation")
	}
	return out, nil
}
func (g *gateway) fileCommand(ctx context.Context, cmd GatewayCommand) (any, error) {
	// Local CLI is a trusted local-user API. MCP always supplies its verified
	// bound identity, never a caller-selected source session.
	local, err := g.base.resolve(ctx, cmd.Session)
	if err != nil || !gatewayExportable(local) || local.ObservationOnly {
		return nil, errors.New("exact live local session required")
	}
	if cmd.Action == "files-list" {
		g.files.mu.Lock()
		records := make([]*fileRecord, 0, len(g.files.records))
		for _, r := range g.files.records {
			records = append(records, r)
		}
		g.files.mu.Unlock()
		out := []FileTransfer{}
		for _, r := range records {
			r.mu.Lock()
			if (r.outgoing && r.info.Sender == cmd.Session) || (!r.outgoing && r.info.Recipient == cmd.Session) {
				out = append(out, r.info)
			}
			r.mu.Unlock()
		}
		return out, nil
	}
	if cmd.Action == "files-send" || cmd.Action == "files-accept" {
		select {
		case g.files.slots <- struct{}{}:
			defer func() { <-g.files.slots }()
		default:
			return nil, errors.New("file transfer busy")
		}
	}
	if cmd.Action == "files-send" {
		return g.offerFile(ctx, cmd, local)
	}
	rec := g.files.get(cmd.ID)
	if rec == nil {
		return nil, errors.New("unknown transfer")
	}
	if cmd.Action == "files-cancel" && ((rec.outgoing && rec.source.SessionId == cmd.Session) || (!rec.outgoing && rec.target.SessionId == cmd.Session)) {
		rec.cancel()
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	owner := rec.target
	if rec.outgoing {
		owner = rec.source
	}
	if owner.SessionId != cmd.Session || !matches(local, owner) || time.Now().After(rec.info.Expires) || rec.link.ctx.Err() != nil {
		return nil, errors.New("transfer unavailable for this session")
	}
	if cmd.Action == "files-cancel" {
		rec.info.Path = ""
		rec.info.State = "cancelled"
		os.RemoveAll(filepath.Dir(rec.staged))
		_, err := rec.link.fileRequest(ctx, fileWire{Action: "cancel", ID: cmd.ID})
		if err != nil {
			return rec.info, fmt.Errorf("cancelled locally; remote cancellation unconfirmed: %w", err)
		}
		return rec.info, nil
	}
	if cmd.Action != "files-accept" || rec.outgoing {
		return nil, errors.New("invalid file operation")
	}
	if rec.info.State == "verified" {
		return rec.info, nil
	}
	if rec.info.State != "offered" {
		return nil, errors.New("transfer is not offered")
	}
	return g.acceptFile(ctx, rec)
}
func (g *gateway) offerFile(ctx context.Context, cmd GatewayCommand, local *pb.Identity) (any, error) {
	dst, err := g.base.resolve(ctx, cmd.Target)
	if err != nil || dst.GatewayHost == "" || dst.ObservationOnly {
		return nil, errors.New("exact live paired session required")
	}
	l, err := g.fileLink(dst.GatewayHost)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	var proxy *gatewayProxy
	for _, p := range l.proxies {
		if matches(p.node.local(), dst) {
			proxy = p
			break
		}
	}
	l.mu.Unlock()
	if proxy == nil {
		return nil, errors.New("remote session unavailable")
	}
	src, err := proxy.source(ctx, local, dst)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(cmd.Path)
	if !filepath.IsAbs(cmd.Path) || !safeFileName(name) {
		return nil, errors.New("absolute regular-file path with safe basename required")
	}
	// OpenRoot confines the selected basename and refuses symlinks escaping its parent.
	root, err := os.OpenRoot(filepath.Dir(cmd.Path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	st, err := root.Lstat(name)
	if err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("source must be a regular file, not a symlink")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|sourceOpenFlags, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !actual.Mode().IsRegular() || !os.SameFile(st, actual) || actual.Size() > fileLimit {
		return nil, errors.New("source changed or exceeds 16 MiB")
	}
	id, err := gatewayRandom()
	if err != nil {
		return nil, err
	}
	rec := &fileRecord{info: FileTransfer{ID: id, Name: name, Sender: src.SessionId, Recipient: proxy.remote.SessionId, State: "staging", Expires: time.Now().Add(59 * time.Minute)}, link: l, source: src, target: cloneIdentity(proxy.remote), outgoing: true}
	// Reserve disk quota before reading. Hold record lock until staging is complete.
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if err := g.files.add(rec); err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			rec.info.State = "failed"
			os.RemoveAll(filepath.Dir(rec.staged))
		}
	}()
	stage, err := os.OpenFile(rec.staged, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(stage, hash), io.LimitReader(f, fileLimit+1))
	closeErr := stage.Close()
	if copyErr != nil {
		return nil, copyErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if n > fileLimit {
		return nil, errors.New("file exceeds 16 MiB")
	}
	rec.info.Size = n
	rec.info.SHA256 = hex.EncodeToString(hash.Sum(nil))
	rec.info.State = "offered"
	_, err = l.fileRequest(ctx, fileWire{Action: "offer", Offer: rec.info, Source: rec.source, Target: rec.target})
	if err != nil {
		return nil, err
	}
	success = true
	return rec.info, nil
}
func (g *gateway) acceptFile(ctx context.Context, rec *fileRecord) (any, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(rec.ctx, cancel)
	defer stop()
	f, err := os.OpenFile(rec.staged, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		f.Close()
		if !success {
			os.Remove(rec.staged)
		}
	}()
	hash := sha256.New()
	for offset := int64(0); offset < rec.info.Size; {
		out, err := rec.link.fileRequest(ctx, fileWire{Action: "chunk", ID: rec.info.ID, Offset: offset})
		if err != nil {
			return nil, err
		}
		expected := min(int64(fileChunk), rec.info.Size-offset)
		if int64(len(out.Data)) != expected {
			return nil, errors.New("invalid file chunk")
		}
		if _, err = io.MultiWriter(f, hash).Write(out.Data); err != nil {
			return nil, err
		}
		offset += expected
	}
	if hex.EncodeToString(hash.Sum(nil)) != rec.info.SHA256 {
		return nil, errors.New("file checksum mismatch")
	}
	if err = f.Sync(); err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	if rec.link.ctx.Err() != nil || ctx.Err() != nil {
		return nil, errors.New("transfer interrupted")
	}
	// Recheck both identities and the shared access policy before exposing bytes.
	if _, _, err = rec.link.localRoute(ctx, rec.source, rec.target); err != nil {
		return nil, err
	}
	path := filepath.Join(filepath.Dir(rec.staged), rec.info.Name)
	if path == rec.staged {
		path = filepath.Join(filepath.Dir(rec.staged), "download-"+rec.info.Name)
	}
	// Link is atomic and refuses an existing destination; never overwrite.
	if err = os.Link(rec.staged, path); err != nil {
		return nil, err
	}
	os.Remove(rec.staged)
	rec.info.Path = path
	rec.info.State = "verified"
	success = true
	_, err = rec.link.fileRequest(ctx, fileWire{Action: "verified", ID: rec.info.ID})
	if err != nil {
		return rec.info, nil
	}
	rec.info.ReceiptConfirmed = true
	return rec.info, nil
}
