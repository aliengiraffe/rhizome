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
	"syscall"
	"time"

	"google.golang.org/grpc"
	pb "gossipsub/proto"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

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
