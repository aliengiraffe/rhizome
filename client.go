package main

import (
	"context"
	"fmt"
	"math/rand"

	pb "gossipsub/proto"
)

type subscription struct{ cancel context.CancelFunc }

func (c *cluster) reevaluate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.ctx.Err() != nil {
		return
	}
	for name, p := range c.peers {
		if rand.Intn(2) == 0 {
			c.unsubscribe(name, p)
			continue
		}
		if p.subscription != nil {
			continue
		}
		ctx, cancel := context.WithCancel(c.ctx)
		sub := &subscription{cancel: cancel}
		p.subscription = sub
		c.log.Printf("SUBSCRIBE %s/sessions", name)
		c.wg.Add(1)
		go c.receive(ctx, name, p, sub)
	}
}

// Caller holds mu. Clear immediately so a later selection can resubscribe.
func (c *cluster) unsubscribe(name string, p *peer) {
	if p.subscription == nil {
		return
	}
	c.log.Printf("UNSUBSCRIBE %s/sessions", name)
	p.subscription.cancel()
	p.subscription = nil
}
func (c *cluster) receive(ctx context.Context, name string, p *peer, sub *subscription) {
	defer c.wg.Done()
	defer sub.cancel()
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		// An old stream must not remove a replacement after a leave/rejoin.
		if c.peers[name] == p && p.subscription == sub {
			p.subscription = nil
		}
	}()
	stream, err := pb.NewPubSubClient(p.conn).Subscribe(ctx, &pb.SubscribeRequest{Channel: "sessions", Subscriber: c.name})
	if err != nil {
		c.streamError(ctx, name, err)
		return
	}
	for {
		message, err := stream.Recv()
		if err != nil {
			c.streamError(ctx, name, err)
			return
		}
		fmt.Printf("[%s] <- %s/sessions: %s\n", c.name, name, message.GetText())
	}
}
func (c *cluster) streamError(ctx context.Context, name string, err error) {
	if ctx.Err() == nil {
		c.log.Printf("STREAM ERROR %s/sessions: %v", name, err)
	}
}
