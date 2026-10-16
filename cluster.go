package main

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"strconv"
	"sync"

	"github.com/hashicorp/memberlist"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type metadata struct {
	GRPCPort int `json:"grpc_port"`
}
type peer struct {
	conn         *grpc.ClientConn
	address      string
	subscription *subscription
}
type cluster struct {
	ctx      context.Context
	name     string
	grpcPort int
	log      *log.Logger
	mu       sync.Mutex
	peers    map[string]*peer
	closed   bool
	wg       sync.WaitGroup
}

func newCluster(ctx context.Context, name string, port int, logger *log.Logger) *cluster {
	return &cluster{ctx: ctx, name: name, grpcPort: port, log: logger, peers: make(map[string]*peer)}
}
func (c *cluster) start(port int) (*memberlist.Memberlist, error) {
	config := memberlist.DefaultLocalConfig()
	config.Name = c.name
	config.BindAddr, config.AdvertiseAddr = "127.0.0.1", "127.0.0.1"
	config.BindPort, config.AdvertisePort = port, port
	config.Delegate, config.Events, config.Logger = c, c, c.log
	return memberlist.Create(config)
}
func (c *cluster) NodeMeta(limit int) []byte {
	data, _ := json.Marshal(metadata{GRPCPort: c.grpcPort})
	if len(data) > limit {
		return nil
	}
	return data
}
func (*cluster) NotifyMsg([]byte)                {}
func (*cluster) GetBroadcasts(int, int) [][]byte { return nil }
func (*cluster) LocalState(bool) []byte          { return nil }
func (*cluster) MergeRemoteState([]byte, bool)   {}
func (c *cluster) NotifyJoin(node *memberlist.Node) {
	c.log.Printf("JOIN %s", node.Name)
	c.upsert(node)
}
func (c *cluster) NotifyUpdate(node *memberlist.Node) {
	c.log.Printf("UPDATE %s", node.Name)
	c.upsert(node)
}
func (c *cluster) NotifyLeave(node *memberlist.Node) {
	c.log.Printf("LEAVE %s", node.Name)
	c.mu.Lock()
	defer c.mu.Unlock()
	if p := c.peers[node.Name]; p != nil {
		c.disconnect(node.Name, p)
		delete(c.peers, node.Name)
	}
}
func (c *cluster) upsert(node *memberlist.Node) {
	if node.Name == c.name {
		return
	}
	var meta metadata
	if err := json.Unmarshal(node.Meta, &meta); err != nil || meta.GRPCPort < 1 || meta.GRPCPort > 65535 || !node.Addr.Equal(net.ParseIP("127.0.0.1")) {
		c.log.Printf("WARN ignoring invalid/non-loopback peer %s", node.Name)
		return
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(meta.GRPCPort))
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if old := c.peers[node.Name]; old != nil {
		if old.address == address {
			return
		}
		c.disconnect(node.Name, old)
		delete(c.peers, node.Name)
	}
	// NewClient is nonblocking: memberlist callbacks must not wait for a dial.
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		c.log.Printf("CONNECT %s: %v", node.Name, err)
		return
	}
	conn.Connect()
	c.peers[node.Name] = &peer{conn: conn, address: address}
}

// Caller holds mu.
func (c *cluster) disconnect(name string, p *peer) {
	c.unsubscribe(name, p)
	if err := p.conn.Close(); err != nil {
		c.log.Printf("CLOSE %s: %v", name, err)
	}
}
func (c *cluster) close() {
	c.mu.Lock()
	c.closed = true
	for name, p := range c.peers {
		c.disconnect(name, p)
		delete(c.peers, name)
	}
	c.mu.Unlock()
	c.wg.Wait()
}
