package channel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	pb "rhizome/proto"
)

func matches(a, b *pb.Identity) bool {
	return a != nil && b != nil && a.SessionId != "" && a.SessionId == b.SessionId && a.NodeId == b.NodeId && a.Generation == b.Generation
}
func (n *Node) Subscribe(req *pb.FollowRequest, ss pb.Channel_SubscribeServer) error {
	if n.bridge != nil {
		return n.bridge.subscribe(req, ss)
	}
	if req.Subscriber == nil || req.Subscriber.SessionId == "" || req.Subscriber.NodeId == n.local().NodeId {
		return status.Error(codes.InvalidArgument, "bound, non-self subscriber required")
	}
	// Verify live identities, including ambiguity, before opening the stream.
	peer, err := n.resolve(ss.Context(), req.Subscriber.SessionId)
	if err != nil || !matches(peer, req.Subscriber) {
		return status.Error(codes.FailedPrecondition, "subscriber identity is stale or ambiguous")
	}
	target, err := n.resolve(ss.Context(), req.Target.GetSessionId())
	if err != nil || !matches(target, req.Target) {
		return status.Error(codes.FailedPrecondition, "target is stale or ambiguous")
	}
	n.mu.Lock()
	if !matches(n.identity, req.Target) {
		n.mu.Unlock()
		return status.Error(codes.FailedPrecondition, "target binding changed")
	}
	n.evictLocked(time.Now())
	l := &listener{queue: make(chan *pb.Event, 64), done: make(chan struct{}), details: req.Details}
	n.listeners[l] = true
	position := &pb.Event{SessionId: n.identity.SessionId, NodeId: n.identity.NodeId, Generation: n.identity.Generation, StreamId: n.stream, Sequence: n.sequence, Kind: "position", UnixMillis: time.Now().UnixMilli()}
	var replay []*pb.Event
	if req.StreamId != "" {
		expired := req.StreamId != n.stream || req.AfterSequence > n.sequence || (len(n.history) > 0 && req.AfterSequence+1 < n.history[0].Sequence) || (len(n.history) == 0 && req.AfterSequence < n.sequence)
		if expired {
			gap := proto.Clone(position).(*pb.Event)
			gap.Kind = "gap"
			gap.Text = "Replay unavailable: publisher restarted or retained cursor expired"
			if len(n.history) > 0 {
				gap.Sequence = n.history[0].Sequence - 1
			}
			replay = append(replay, gap)
		}
		for _, e := range n.history {
			if detailKind(e.Kind) && !req.Details {
				continue
			}
			if req.StreamId != n.stream || e.Sequence > req.AfterSequence {
				replay = append(replay, e)
			}
		}
	}
	n.mu.Unlock()
	defer func() { n.mu.Lock(); delete(n.listeners, l); n.mu.Unlock() }()
	for _, e := range replay {
		if err := ss.Send(e); err != nil {
			return err
		}
	}
	if err := ss.Send(position); err != nil {
		return err
	}
	for {
		select {
		case <-n.ctx.Done():
			return status.Error(codes.Unavailable, "node stopped")
		case <-ss.Context().Done():
			return ss.Context().Err()
		case <-l.done:
			if !matches(n.local(), req.Target) {
				ended := proto.Clone(position).(*pb.Event)
				ended.Kind = "session-ended"
				ended.Text = "Publisher session binding ended"
				_ = ss.Send(ended)
			}
			return status.Error(codes.ResourceExhausted, "stream ended or subscriber queue overflow; reconnect with cursor")
		case e := <-l.queue:
			if err := ss.Send(e); err != nil {
				return err
			}
		}
	}
}
func (n *Node) Send(ctx context.Context, req *pb.SendRequest) (*pb.Acceptance, error) {
	if n.bridge != nil {
		return n.bridge.send(ctx, req)
	}
	if n.local().ObservationOnly {
		return nil, status.Error(codes.FailedPrecondition, "session is observation-only; configure a Rhizome MCP/channel integration for delivery")
	}
	if req.Event == nil || req.Event.Kind != "direct" || req.Event.MessageId == "" || len(req.Event.MessageId) > 128 || len(req.Event.Text) > maxText || strings.TrimSpace(req.Event.Text) == "" {
		return nil, status.Error(codes.InvalidArgument, "invalid direct message")
	}
	sender, err := n.resolve(ctx, req.Sender.GetSessionId())
	if err != nil || !matches(sender, req.Sender) {
		return nil, status.Error(codes.FailedPrecondition, "sender is stale or ambiguous")
	}
	target, err := n.resolve(ctx, req.Target.GetSessionId())
	if err != nil || !matches(target, req.Target) {
		return nil, status.Error(codes.FailedPrecondition, "receiver is stale or ambiguous")
	}
	e := req.Event
	if e.SessionId != sender.SessionId || e.NodeId != sender.NodeId || e.Generation != sender.Generation || e.StreamId == "" {
		return nil, status.Error(codes.InvalidArgument, "event origin does not match sender")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if !matches(n.identity, req.Target) {
		return nil, status.Error(codes.FailedPrecondition, "receiver binding changed")
	}
	if !n.enqueueLocked(e, "", "", n.identity.Generation) {
		return nil, status.Error(codes.ResourceExhausted, "receiver queue full")
	}
	return &pb.Acceptance{Accepted: true, MessageId: e.MessageId}, nil
}
func (n *Node) send(ctx context.Context, session, text, reply string) (*pb.Acceptance, error) {
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("text must not be empty")
	}
	n.mu.Lock()
	if n.identity.SessionId == "" {
		n.mu.Unlock()
		return nil, errUnbound
	}
	if session == n.identity.SessionId {
		n.mu.Unlock()
		return nil, errors.New("cannot send to self")
	}
	if err := n.rateLocked(); err != nil {
		n.mu.Unlock()
		return nil, err
	}
	sender := cloneIdentity(n.identity)
	n.directSequence++
	e := &pb.Event{SessionId: sender.SessionId, NodeId: sender.NodeId, Generation: sender.Generation, StreamId: n.stream + "/direct", Sequence: n.directSequence, Kind: "direct", UnixMillis: time.Now().UnixMilli(), Text: clip(text, maxText), MessageId: id(), ReplyTo: clip(reply, 128)}
	n.mu.Unlock()
	target, err := n.resolve(ctx, session)
	if err != nil {
		// Recent-only sessions have no live route; nothing is queued or deferred.
		if n.recentOnly(session) {
			return nil, fmt.Errorf("session %q is recent, not live: it recorded activity within the recent window but no process currently owns it, so the message was not queued and will not be delivered later", session)
		}
		return nil, err
	}
	conn, err := n.dial(target)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return pb.NewChannelClient(conn).Send(n.outgoing(ctx), &pb.SendRequest{Target: target, Sender: sender, Event: e})
}
func (n *Node) subscribe(session string) error {
	n.gate.Lock()
	defer n.gate.Unlock()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.identity.SessionId == "" {
		return errUnbound
	}
	if session == "" || session == n.identity.SessionId {
		return errors.New("choose a non-self exact session_id")
	}
	n.rearmed[session] = true
	if f := n.feeds[session]; f != nil {
		f.status = "reconnecting"
		return nil
	}
	ctx, cancel := context.WithCancel(n.ctx)
	f := &feed{cancel: cancel, status: "connecting", token: id()}
	n.feeds[session] = f
	generation := n.identity.Generation
	n.wg.Add(1)
	go func() { defer n.wg.Done(); n.follow(ctx, session, f, generation) }()
	return nil
}
func (n *Node) unsubscribe(session string) error {
	n.gate.Lock()
	defer n.gate.Unlock()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.identity.SessionId == "" {
		return errUnbound
	}
	if f := n.feeds[session]; f != nil {
		f.cancel()
		delete(n.feeds, session)
	}
	delete(n.rearmed, session)
	kept := n.incoming[:0]
	for _, p := range n.incoming {
		if p.subscription != session {
			kept = append(kept, p)
		} else {
			n.stats.Dropped++
		}
	}
	n.incoming = kept
	return nil
}
func (n *Node) follow(ctx context.Context, session string, f *feed, generation uint64) {
	var streamID string
	var previous *pb.Identity
	var gatewayHost, gatewayGrant string
	var sequence uint64
	backoff := 250 * time.Millisecond
	for ctx.Err() == nil {
		if gatewayHost != "" && (gatewayGrant == "" || gatewayDeliveryGrants(n.runtime)[gatewayHost] != gatewayGrant) {
			n.mu.Lock()
			if n.feeds[session] == f {
				delete(n.feeds, session)
				delete(n.rearmed, session)
			}
			n.mu.Unlock()
			return
		}
		err := func() error {
			target, err := n.resolve(ctx, session)
			if err != nil {
				return err
			}
			if target.GatewayHost != "" && gatewayHost == "" {
				gatewayHost = target.GatewayHost
				gatewayGrant = gatewayDeliveryGrants(n.runtime)[gatewayHost]
				if gatewayGrant == "" {
					return errors.New("gateway trust revoked")
				}
			}
			if previous != nil && !matches(previous, target) && (previous.ObservationOnly || target.ObservationOnly) {
				// Independent collectors do not share replay cursors. Starting
				// live prevents replay of activity already delivered by the old owner.
				streamID = ""
				sequence = 0
				n.mu.Lock()
				f.gaps++
				n.enqueueLocked(&pb.Event{SessionId: session, NodeId: target.NodeId, Generation: target.Generation, StreamId: "handoff", MessageId: id(), Kind: "gap", Text: "Collection owner changed; subscription resumed live", UnixMillis: time.Now().UnixMilli()}, session, f.token, generation)
				n.mu.Unlock()
			}
			previous = target
			sender := n.local()
			if sender.Generation != generation {
				return context.Canceled
			}
			conn, err := n.dial(target)
			if err != nil {
				return err
			}
			defer conn.Close()
			streamCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			stream, err := pb.NewChannelClient(conn).Subscribe(n.outgoing(streamCtx), &pb.FollowRequest{Target: target, Subscriber: sender, StreamId: streamID, AfterSequence: sequence, Details: true})
			if err != nil {
				return err
			}
			// Periodically fence streams if gossip discovers a duplicate or changed binding.
			done := make(chan struct{})
			defer close(done)
			go func() {
				ticker := time.NewTicker(time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-done:
						return
					case <-ctx.Done():
						return
					case <-ticker.C:
						p, e := n.resolve(streamCtx, session)
						if e != nil || !matches(p, target) {
							cancel()
							return
						}
					}
				}
			}()
			for {
				e, err := stream.Recv()
				if err != nil {
					return err
				}
				if e.SessionId != target.SessionId || e.NodeId != target.NodeId || e.Generation != target.Generation || e.StreamId == "" || len(e.Text) > maxText {
					return errors.New("invalid event origin")
				}
				switch e.Kind {
				case "position", "activity", "tool", "turn", "announcement", "gap", "session-ended", "prompt", "tool_input", "tool_output":
				default:
					return errors.New("unknown event kind")
				}
				n.mu.Lock()
				if n.feeds[session] != f || n.identity.Generation != generation {
					n.mu.Unlock()
					return context.Canceled
				}
				f.status = "following"
				accepted := true
				// Turn markers are never queued; consume them so the cursor advances
				// instead of replaying the same marker on every reconnect.
				if e.Kind != "position" && e.Kind != "turn" {
					accepted = n.enqueueLocked(e, session, f.token, generation)
				}
				if e.Kind == "gap" {
					f.gaps++
					slog.Warn("replay_gap", "session_id", session)
				}
				n.mu.Unlock()
				if !accepted {
					return errors.New("local delivery queue full")
				}
				streamID = e.StreamId
				sequence = e.Sequence
				backoff = 250 * time.Millisecond
			}
		}()
		if ctx.Err() != nil {
			return
		}
		n.mu.Lock()
		f.status = "reconnecting"
		f.reconnects++
		n.mu.Unlock()
		slog.Info("reconnect", "session_id", session, "error", fmt.Sprint(err))
		delay := backoff + time.Duration(rand.Int63n(int64(backoff/2)))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}
