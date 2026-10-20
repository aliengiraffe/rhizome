package channel

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/memberlist"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	pb "rhizome/proto"
)

// NodeMeta serializes the identity into memberlist's bounded metadata. Labels
// (session_name, project) give way rune by rune, longest first, so routing and
// authentication fields are never the reason an advertisement is dropped.
func (n *Node) NodeMeta(limit int) []byte {
	identity := n.local()
	b, _ := json.Marshal(identity)
	for len(b) > limit && (len(identity.Project) > 0 || len(identity.SessionName) > 0) {
		if len(identity.SessionName) >= len(identity.Project) {
			identity.SessionName = trimRune(identity.SessionName)
		} else {
			identity.Project = trimRune(identity.Project)
		}
		b, _ = json.Marshal(identity)
	}
	if len(b) > limit {
		return nil
	}
	return b
}
func (*Node) NotifyMsg([]byte)                {}
func (*Node) GetBroadcasts(int, int) [][]byte { return nil }
func (*Node) LocalState(bool) []byte          { return nil }
func (*Node) MergeRemoteState([]byte, bool)   {}
func loopback(endpoint string) bool {
	h, p, err := net.SplitHostPort(endpoint)
	port, e := strconv.Atoi(p)
	return err == nil && e == nil && h == "127.0.0.1" && port > 0 && port <= 65535
}
func (n *Node) authorized(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	v := md.Get("authorization")
	if len(v) != 1 || subtle.ConstantTimeCompare([]byte(v[0]), []byte("Bearer "+n.token)) != 1 {
		return status.Error(codes.Unauthenticated, "invalid cluster credential")
	}
	return nil
}
func (n *Node) outgoing(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+n.token)
}
func dial(endpoint string) (*grpc.ClientConn, error) {
	if !loopback(endpoint) {
		return nil, errors.New("non-loopback endpoint")
	}
	return grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
}
func (n *Node) probe(ctx context.Context, i *pb.Identity) (*pb.Identity, error) {
	c, err := dial(i.Endpoint)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	actual, err := pb.NewChannelClient(c).Info(n.outgoing(ctx), &pb.InfoRequest{})
	if err != nil {
		return nil, err
	}
	if actual.NodeId != i.NodeId || actual.Protocol != 1 || !loopback(actual.GossipEndpoint) {
		return nil, errors.New("registration identity mismatch")
	}
	return actual, nil
}
func secureDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Lstat(dir, &stat); err != nil {
		return err
	}
	if stat.Uid != uint32(os.Getuid()) {
		return errors.New("runtime directory must belong to current user")
	}
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return errors.New("runtime directory must be private (0700), not a symlink")
	}
	return nil
}
func (n *Node) start(runtime string) error {
	if runtime == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		runtime = filepath.Join(home, ".rhizome", "run")
	}
	if err := secureDir(runtime); err != nil {
		return err
	}
	n.runtime = runtime
	lock, err := os.OpenFile(filepath.Join(runtime, "registry.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	tokenPath := filepath.Join(runtime, "token")
	b, err := os.ReadFile(tokenPath)
	if os.IsNotExist(err) {
		b = []byte(id() + id())
		err = os.WriteFile(tokenPath, b, 0600)
	}
	if err != nil {
		return err
	}
	st, err := os.Lstat(tokenPath)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return errors.New("token must be a private regular file")
	}
	if len(b) != 64 {
		return errors.New("invalid cluster token length")
	}
	n.token = string(b)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	n.identity.Endpoint = listener.Addr().String()
	n.rpc = grpc.NewServer(grpc.MaxRecvMsgSize(64*1024), grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := n.authorized(ctx); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}), grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := n.authorized(ss.Context()); err != nil {
			return err
		}
		return handler(srv, ss)
	}))
	pb.RegisterChannelServer(n.rpc, n)
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		if err := n.rpc.Serve(listener); err != nil && n.ctx.Err() == nil {
			slog.Error("grpc_serve", "error", err)
		}
	}()
	config := memberlist.DefaultLocalConfig()
	config.Name = n.identity.NodeId
	config.BindAddr = "127.0.0.1"
	config.AdvertiseAddr = "127.0.0.1"
	config.BindPort = 0
	config.AdvertisePort = 0
	config.Delegate = n
	config.Events = n
	config.Logger = log.New(gossipLog{}, "", 0)
	key, err := hex.DecodeString(n.token[:32])
	if err != nil {
		return err
	}
	config.SecretKey = key
	n.members, err = memberlist.Create(config)
	if err != nil {
		return err
	}
	local := n.members.LocalNode()
	n.mu.Lock()
	n.identity.GossipEndpoint = net.JoinHostPort("127.0.0.1", strconv.Itoa(int(local.Port)))
	n.mu.Unlock()
	if err = n.members.UpdateNode(time.Second); err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(runtime, "*.json"))
	if err != nil {
		return err
	}
	for _, path := range files {
		raw, e := os.ReadFile(path)
		var peer pb.Identity
		if e != nil || json.Unmarshal(raw, &peer) != nil {
			_ = os.Remove(path)
			continue
		}
		actual, e := n.probe(n.ctx, &peer)
		if e != nil {
			_ = os.Remove(path)
			continue
		}
		if _, e = n.members.Join([]string{actual.GossipEndpoint}); e != nil {
			slog.Warn("bootstrap_join", "error", e)
		}
	}
	n.registration = filepath.Join(runtime, n.identity.NodeId+".json")
	raw, _ := json.Marshal(n.local())
	if err = os.WriteFile(n.registration, raw, 0600); err != nil {
		return err
	}
	slog.Info("ready", "binding", "UNBOUND", "node_id", n.identity.NodeId, "endpoint", n.identity.Endpoint)
	return nil
}
func (n *Node) unregister() {
	if n.registration != "" {
		_ = os.Remove(n.registration)
	}
}

// memberlist owns mutable Node pointers. Copy metadata inside callbacks,
// while memberlist holds its lock; never retain pointers from Members().
func (n *Node) NotifyJoin(member *memberlist.Node)   { n.updatePeer(member) }
func (n *Node) NotifyUpdate(member *memberlist.Node) { n.updatePeer(member) }
func (n *Node) NotifyLeave(member *memberlist.Node) {
	n.mu.Lock()
	delete(n.discovered, member.Name)
	n.mu.Unlock()
}
func (n *Node) updatePeer(member *memberlist.Node) {
	if !member.Addr.Equal(net.ParseIP("127.0.0.1")) {
		return
	}
	var i pb.Identity
	if json.Unmarshal(member.Meta, &i) != nil || i.Protocol != 1 || i.NodeId != member.Name || !loopback(i.Endpoint) {
		return
	}
	n.mu.Lock()
	n.discovered[i.NodeId] = &i
	n.mu.Unlock()
}
func (n *Node) peers() []*pb.Identity {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := []*pb.Identity{cloneIdentity(n.identity)}
	for key, i := range n.discovered {
		if key != n.identity.NodeId {
			out = append(out, cloneIdentity(i))
		}
	}
	return out
}
func (n *Node) resolve(ctx context.Context, session string) (*pb.Identity, error) {
	var found *pb.Identity
	for _, i := range n.peers() {
		if i.SessionId != session {
			continue
		}
		actual, err := n.probe(ctx, i)
		if err != nil || actual.SessionId != session {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("ambiguous session %q: multiple live nodes", session)
		}
		found = actual
	}
	if found == nil {
		return nil, fmt.Errorf("session %q is not currently available", session)
	}
	return found, nil
}
func (n *Node) Info(context.Context, *pb.InfoRequest) (*pb.Identity, error) { return n.local(), nil }

type gossipLog struct{}

func (gossipLog) Write(p []byte) (int, error) {
	slog.Info("gossip", "message", strings.TrimSpace(string(p)))
	return len(p), nil
}
