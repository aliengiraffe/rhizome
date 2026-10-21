package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"rhizome/internal/channel"
	"rhizome/internal/config"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	pb "rhizome/proto"
)

func main() {
	if err := entry(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func entry() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: rhizome mcp | setup | service | config | token | files | invite | join | peers | demo [flags]")
	}
	mode := os.Args[1]
	os.Args = append(os.Args[:1], os.Args[2:]...)
	switch mode {
	case "mcp":
		opts := flag.NewFlagSet("mcp", flag.ContinueOnError)
		agent := opts.String("agent", "claude", "delivery agent: claude or codex")
		config.Bind(opts, "codex-bin")
		o, err := optionsFromEnv()
		if err != nil {
			return err
		}
		opts.BoolVar(&o.LAN, "lan", o.LAN, "peer with other devices on the local network (RHIZOME_LAN=1)")
		opts.BoolVar(&o.Tailscale, "tailscale", o.Tailscale, "peer with devices on this tailnet via the local Tailscale daemon; implies the LAN transport (RHIZOME_TAILSCALE=1)")
		opts.StringVar(&o.BindAddr, "bind-addr", o.BindAddr, "LAN listen address, default 0.0.0.0, or the Tailscale IP with --tailscale (RHIZOME_BIND_ADDR)")
		opts.StringVar(&o.AdvertiseAddr, "advertise-addr", o.AdvertiseAddr, "LAN address peers dial, default first private interface, or the Tailscale IP with --tailscale (RHIZOME_ADVERTISE_ADDR)")
		opts.Var((*listFlag)(&o.Join), "join", "gossip host:port of a node on another device, MagicDNS names allowed; repeatable (RHIZOME_JOIN, comma-separated)")
		opts.BoolVar(&o.MDNS, "mdns", o.MDNS, "advertise and browse _rhizome._tcp in LAN mode; off by default with --tailscale (RHIZOME_MDNS=0 disables)")
		opts.StringVar(&o.TokenFile, "token-file", o.TokenFile, "file holding the cluster token to adopt (RHIZOME_TOKEN_FILE; RHIZOME_TOKEN holds it inline)")
		opts.IntVar(&o.GossipPort, "gossip-port", o.GossipPort, "Tailscale mode: well-known gossip port tried first so other devices can find this one, default 7946 (RHIZOME_GOSSIP_PORT)")
		opts.DurationVar(&o.Discovery, "discovery-interval", o.Discovery, "Tailscale mode: how often to enumerate tailnet peers, default 20s (RHIZOME_DISCOVERY_INTERVAL)")
		if err := opts.Parse(os.Args[1:]); err != nil {
			return err
		}
		if opts.NArg() != 0 {
			return fmt.Errorf("unexpected mcp arguments")
		}
		c, err := config.FromFlags(opts)
		if err != nil {
			return err
		}
		// Precedence is flag > env > file > default; the file only fills what
		// neither a flag nor an environment variable set.
		visited := map[string]bool{}
		opts.Visit(func(f *flag.Flag) { visited[f.Name] = true })
		applyNetwork(&o, c.Network, visited, os.LookupEnv)
		if c.Gateway.Mode != "" && c.Gateway.Mode != "off" && (o.LAN || o.Tailscale) {
			return fmt.Errorf("gateway pairing requires local-mode MCP; configure gateway.mode for host networking")
		}
		// mDNS cannot cross the tunnel; in Tailscale mode it is opt-in.
		_, mdnsSet := os.LookupEnv("RHIZOME_MDNS")
		mdnsSet = mdnsSet || visited["mdns"] || c.Network.MDNS != nil
		if o.Tailscale && !mdnsSet {
			o.MDNS = false
		}
		switch *agent {
		case "claude":
			return channel.Run(c, o)
		case "codex":
			return channel.RunCodex(c, o)
		default:
			return fmt.Errorf("unsupported agent %q", *agent)
		}
	case "config":
		fs := flag.NewFlagSet("config", flag.ContinueOnError)
		config.Bind(fs, "runtime-dir", "codex-bin", "claude-root", "codex-root")
		if err := fs.Parse(os.Args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected config arguments")
		}
		c, err := config.FromFlags(fs)
		if err != nil {
			return err
		}
		return c.Write(os.Stdout)
	case "files":
		return filesCommand(os.Args[1:])
	case "invite", "join", "peers":
		return gatewayCommand(mode, os.Args[1:])
	case "service":
		return serviceCommand(os.Args[1:])
	case "setup":
		return setup(os.Args[1:])
	case "token":
		opts := flag.NewFlagSet("token", flag.ContinueOnError)
		config.Bind(opts, "runtime-dir")
		if err := opts.Parse(os.Args[1:]); err != nil {
			return err
		}
		if opts.NArg() != 0 {
			return fmt.Errorf("unexpected token arguments")
		}
		c, err := config.FromFlags(opts)
		if err != nil {
			return err
		}
		token, err := channel.Token(c.RuntimeDir)
		if err != nil {
			return err
		}
		fmt.Println(token)
		return nil
	case "demo":
		return run()
	default:
		return fmt.Errorf("unknown command %q", mode)
	}
}

// optionsFromEnv reads the LAN and Tailscale environment defaults that flags
// may override. The runtime directory comes from the resolved configuration,
// not from here.
func optionsFromEnv() (channel.Options, error) {
	truthy := func(v string) bool {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}
	o := channel.Options{LAN: truthy(os.Getenv("RHIZOME_LAN")), Tailscale: truthy(os.Getenv("RHIZOME_TAILSCALE")), BindAddr: os.Getenv("RHIZOME_BIND_ADDR"), AdvertiseAddr: os.Getenv("RHIZOME_ADVERTISE_ADDR"), MDNS: true, Token: os.Getenv("RHIZOME_TOKEN"), TokenFile: os.Getenv("RHIZOME_TOKEN_FILE")}
	if v, ok := os.LookupEnv("RHIZOME_MDNS"); ok {
		o.MDNS = truthy(v)
	}
	if v := strings.TrimSpace(os.Getenv("RHIZOME_GOSSIP_PORT")); v != "" {
		port, err := strconv.Atoi(v)
		if err != nil {
			return o, fmt.Errorf("RHIZOME_GOSSIP_PORT: %w", err)
		}
		o.GossipPort = port
	}
	if v := strings.TrimSpace(os.Getenv("RHIZOME_DISCOVERY_INTERVAL")); v != "" {
		interval, err := time.ParseDuration(v)
		if err != nil {
			return o, fmt.Errorf("RHIZOME_DISCOVERY_INTERVAL: %w", err)
		}
		o.Discovery = interval
	}
	for _, seed := range strings.Split(os.Getenv("RHIZOME_JOIN"), ",") {
		if seed = strings.TrimSpace(seed); seed != "" {
			o.Join = append(o.Join, seed)
		}
	}
	return o, nil
}

// applyNetwork fills o from the configuration file's [network] table for every
// setting that no flag and no environment variable already chose.
func applyNetwork(o *channel.Options, n config.Network, visited map[string]bool, lookup func(string) (string, bool)) {
	set := func(env, flagName string) bool {
		if visited[flagName] {
			return true
		}
		v, ok := lookup(env)
		return ok && strings.TrimSpace(v) != ""
	}
	if !set("RHIZOME_LAN", "lan") && !set("RHIZOME_TAILSCALE", "tailscale") {
		o.LAN, o.Tailscale = n.Mode == config.ModeLAN, n.Mode == config.ModeTailscale
	}
	if !set("RHIZOME_BIND_ADDR", "bind-addr") {
		o.BindAddr = n.BindAddr
	}
	if !set("RHIZOME_ADVERTISE_ADDR", "advertise-addr") {
		o.AdvertiseAddr = n.AdvertiseAddr
	}
	if !set("RHIZOME_JOIN", "join") {
		o.Join = append([]string(nil), n.Join...)
	}
	if !set("RHIZOME_MDNS", "mdns") && n.MDNS != nil {
		o.MDNS = *n.MDNS
	}
	if !set("RHIZOME_GOSSIP_PORT", "gossip-port") {
		o.GossipPort = n.GossipPort
	}
	if !set("RHIZOME_DISCOVERY_INTERVAL", "discovery-interval") && n.DiscoveryInterval != "" {
		o.Discovery, _ = time.ParseDuration(n.DiscoveryInterval) // validated by config
	}
}

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func run() error {
	name := flag.String("name", "", "unique node name (default node-<gossip port>)")
	gossipPort := flag.Int("gossip", 7946, "loopback gossip TCP/UDP port")
	grpcPort := flag.Int("grpc", 9000, "loopback gRPC TCP port")
	join := flag.String("join", "", "gossip host:port of an existing node")
	flag.Parse()
	if *gossipPort < 1 || *gossipPort > 65535 || *grpcPort < 1 || *grpcPort > 65535 || *gossipPort == *grpcPort {
		return fmt.Errorf("gossip and grpc must be distinct ports between 1 and 65535")
	}
	if *name == "" {
		*name = fmt.Sprintf("node-%d", *gossipPort)
	}
	logger := log.New(os.Stdout, "["+*name+"] ", log.Ltime|log.Lmicroseconds)
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(signals)
	defer cancel()
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *grpcPort))
	if err != nil {
		return err
	}
	defer listener.Close()
	broker := newServer(ctx, logger)
	rpc := grpc.NewServer()
	pb.RegisterPubSubServer(rpc, broker)
	serveErr := make(chan error, 1)
	go func() { serveErr <- rpc.Serve(listener) }()
	defer func() {
		cancel() // End incoming streams before waiting for GracefulStop.
		done := make(chan struct{})
		go func() { rpc.GracefulStop(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			rpc.Stop()
			<-done
		}
	}()
	cluster := newCluster(ctx, *name, *grpcPort, logger)
	defer cluster.close()
	membership, err := cluster.start(*gossipPort)
	if err != nil {
		return fmt.Errorf("start gossip: %w", err)
	}
	defer func() {
		logger.Print("SHUTDOWN leaving cluster")
		if err := membership.Leave(time.Second); err != nil {
			logger.Printf("Leave: %v", err)
		}
		if err := membership.Shutdown(); err != nil {
			logger.Printf("Shutdown: %v", err)
		}
	}()
	if *join != "" {
		if _, err := membership.Join([]string{*join}); err != nil {
			return fmt.Errorf("join %s: %w", *join, err)
		}
	}
	logger.Printf("READY gossip=127.0.0.1:%d grpc=127.0.0.1:%d", *gossipPort, *grpcPort)
	subscriptionTimer := time.NewTimer(randomDelay(10, 20))
	defer subscriptionTimer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-serveErr:
			return fmt.Errorf("gRPC server stopped: %v", err)
		case <-subscriptionTimer.C:
			cluster.reevaluate()
			subscriptionTimer.Reset(randomDelay(10, 20))
		}
	}
}

func randomDelay(minSeconds, maxSeconds int) time.Duration {
	return time.Duration(minSeconds)*time.Second + time.Duration(rand.Int63n(int64(time.Duration(maxSeconds-minSeconds)*time.Second)))
}
