package main

import (
	"context"
	"log"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "gossipsub/proto"
)

type server struct {
	pb.UnimplementedPubSubServer
	ctx         context.Context
	log         *log.Logger
	mu          sync.Mutex
	subscribers map[chan *pb.Message]string
}

func newServer(ctx context.Context, logger *log.Logger) *server {
	return &server{ctx: ctx, log: logger, subscribers: make(map[chan *pb.Message]string)}
}

func (s *server) Subscribe(req *pb.SubscribeRequest, stream pb.PubSub_SubscribeServer) error {
	if req.GetChannel() != "sessions" {
		return status.Error(codes.NotFound, "unknown channel; use sessions")
	}
	messages := make(chan *pb.Message, 32)
	s.mu.Lock()
	s.subscribers[messages] = req.GetSubscriber()
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.subscribers, messages); s.mu.Unlock() }()
	for {
		select {
		case <-s.ctx.Done():
			return status.Error(codes.Unavailable, "node shutting down")
		case <-stream.Context().Done():
			return stream.Context().Err()
		case message := <-messages:
			if err := stream.Send(message); err != nil {
				return err
			}
		}
	}
}

// Messages are immutable after publication; queues are never closed, avoiding
// a send/close race when a subscriber leaves during fan-out.
func (s *server) publish(message *pb.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for queue, subscriber := range s.subscribers {
		select {
		case queue <- message:
		default:
			s.log.Printf("WARN DROP sessions subscriber=%q buffer full", subscriber)
		}
	}
}
