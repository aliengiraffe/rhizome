package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "gossipsub/proto"
)

type testStream struct {
	grpc.ServerStream
	ctx      context.Context
	received chan *pb.Message
}

func (s *testStream) Context() context.Context { return s.ctx }
func (s *testStream) Send(m *pb.Message) error {
	select {
	case s.received <- m:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition timed out")
		}
		time.Sleep(time.Millisecond)
	}
}
func TestFanoutAndCancellation(t *testing.T) {
	ctx, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	s := newServer(ctx, log.New(io.Discard, "", 0))
	done := make(chan error, 2)
	streams := make([]*testStream, 2)
	cancels := make([]context.CancelFunc, 2)
	for i := range streams {
		streamCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cancels[i] = cancel
		streams[i] = &testStream{ctx: streamCtx, received: make(chan *pb.Message, 1)}
		go func(stream *testStream) { done <- s.Subscribe(&pb.SubscribeRequest{Channel: "sessions"}, stream) }(streams[i])
	}
	count := func(n int) bool { s.mu.Lock(); defer s.mu.Unlock(); return len(s.subscribers) == n }
	waitFor(t, func() bool { return count(2) })
	message := &pb.Message{Channel: "sessions", Text: "hello"}
	s.publish(message)
	for _, stream := range streams {
		select {
		case got := <-stream.received:
			if got != message {
				t.Fatal("wrong message")
			}
		case <-time.After(time.Second):
			t.Fatal("missing message")
		}
	}
	cancels[0]()
	waitFor(t, func() bool { return count(1) })
	shutdown()
	for range streams {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("handler did not exit")
		}
	}
	if !count(0) {
		t.Fatal("subscribers leaked")
	}
}
func TestSlowSubscriberDropsWithoutBlockingOthers(t *testing.T) {
	var logs bytes.Buffer
	s := newServer(context.Background(), log.New(&logs, "", 0))
	slow, fast := make(chan *pb.Message, 32), make(chan *pb.Message, 32)
	for i := 0; i < cap(slow); i++ {
		slow <- &pb.Message{}
	}
	s.subscribers[slow], s.subscribers[fast] = "slow", "fast"
	done := make(chan struct{})
	message := &pb.Message{Text: "new"}
	go func() { s.publish(message); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publish blocked")
	}
	if len(slow) != 32 || len(fast) != 1 || <-fast != message {
		t.Fatal("incorrect fan-out")
	}
	if !strings.Contains(logs.String(), "WARN DROP") {
		t.Fatal("missing drop warning")
	}
}
func TestUnknownChannel(t *testing.T) {
	s := newServer(context.Background(), log.New(io.Discard, "", 0))
	if status.Code(s.Subscribe(&pb.SubscribeRequest{Channel: "other"}, nil)) != codes.NotFound {
		t.Fatal("expected NotFound")
	}
}
