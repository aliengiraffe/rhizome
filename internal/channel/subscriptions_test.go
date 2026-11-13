package channel

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"rhizome/internal/config"
	pb "rhizome/proto"
)

func TestSubscriptionModes(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		allowed []string
	}{
		{"full", []string{"activity", "tool", "gap", "paused", "prompt", "tool_input", "tool_output", "session-ended"}},
		{"markers", []string{"turn", "gap", "paused", "position"}},
		{"tools", []string{"tool"}}, {"text", []string{"activity"}},
		{"digest", []string{"activity", "tool", "gap", "paused", "prompt", "tool_input", "tool_output", "session-ended"}},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			p, err := resolveSubscription(config.Subscriptions{}, nil, subscribeInput{Mode: tc.mode})
			if err != nil {
				t.Fatal(err)
			}
			for _, kind := range subscriptionKinds {
				want := false
				for _, k := range tc.allowed {
					want = want || k == kind
				}
				if p.allows(&pb.Event{Kind: kind}) != want {
					t.Fatalf("%s", kind)
				}
			}
			for _, kind := range []string{"announcement", "direct"} {
				if !p.allows(&pb.Event{Kind: kind}) {
					t.Fatalf("filtered explicit %s", kind)
				}
			}
		})
	}
}

func TestSubscriptionPrecedenceAndValidation(t *testing.T) {
	defaults := config.Subscriptions{DefaultMode: "markers", Window: "5m", MaxPerMinute: 20}
	p, err := resolveSubscription(defaults, nil, subscribeInput{})
	if err != nil || p.mode != "markers" {
		t.Fatalf("%+v %v", p, err)
	}
	p, err = resolveSubscription(defaults, &p, subscribeInput{Mode: "tools"})
	if err != nil || p.mode != "tools" {
		t.Fatalf("%+v %v", p, err)
	}
	p, err = resolveSubscription(defaults, &p, subscribeInput{})
	if err != nil || p.mode != "tools" {
		t.Fatalf("session preference lost: %+v %v", p, err)
	}
	p, err = resolveSubscription(defaults, &p, subscribeInput{Mode: "text", Kinds: []string{"turn"}})
	if err != nil || !p.allows(&pb.Event{Kind: "turn"}) || p.allows(&pb.Event{Kind: "activity"}) {
		t.Fatalf("kinds precedence: %+v %v", p, err)
	}
	no := false
	p, err = resolveSubscription(config.Subscriptions{}, nil, subscribeInput{IncludeText: &no, Tools: []string{"Bash", "commandExecution"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind, text string
		want       bool
	}{
		{"activity", "secret", false}, {"gap", "gap", true}, {"turn", "Turn completed", true}, {"position", "", true}, {"tool", "Tool started: Bash", true},
		{"tool", "Tool completed: Edit", false}, {"tool", "commandExecution: completed (exit 0)", true},
		{"tool_input", "Edit: file", false},
	} {
		if p.allows(&pb.Event{Kind: tc.kind, Text: tc.text}) != tc.want {
			t.Fatal(tc)
		}
	}
	for _, in := range []subscribeInput{{Mode: "oops"}, {Kinds: []string{"oops"}}, {Tools: []string{"oops"}}} {
		if _, err := resolveSubscription(defaults, nil, in); err == nil || !strings.Contains(err.Error(), "valid values:") {
			t.Fatalf("%+v %v", in, err)
		}
	}
	for _, in := range []subscribeInput{{Window: "5m"}, {Mode: "digest", Window: "0s"}, {Mode: "digest", Window: "25h"}} {
		if _, err := resolveSubscription(defaults, nil, in); err == nil {
			t.Fatal(in)
		}
	}
}

func TestSubscriptionBudgetIsolation(t *testing.T) {
	n := node(t)
	bind(t, n, "local", t.TempDir())
	n.feeds["a"] = &feed{token: "a", policy: subscriptionPolicy{mode: "tools", maxPerMinute: 2}}
	n.feeds["b"] = &feed{token: "b", policy: subscriptionPolicy{mode: "tools", maxPerMinute: 2}}
	// No follower goroutines: exercise the shared queue boundary deterministically.
	for i := 0; i < 5; i++ {
		n.enqueueLocked(&pb.Event{NodeId: "peer", SessionId: "a", Kind: "activity", MessageId: fmt.Sprint("filtered", i)}, "a", "a", 1)
		n.enqueueLocked(&pb.Event{NodeId: "peer", SessionId: "a", Kind: "tool", MessageId: fmt.Sprint(i)}, "a", "a", 1)
	}
	n.enqueueLocked(&pb.Event{NodeId: "peer", SessionId: "b", Kind: "tool", MessageId: "other"}, "b", "b", 1)
	if n.feeds["a"].suppressed != 3 || n.feeds["b"].suppressed != 0 || len(n.incoming) != 3 || n.paused {
		t.Fatalf("budget isolation: %+v", n.status())
	}
	f := n.feeds["a"]
	if !f.admit(f.budgetStart.Add(time.Minute)) {
		t.Fatal("budget did not reset")
	}
	// node.close cancels feeds.
	n.feeds["a"].cancel = func() {}
	n.feeds["b"].cancel = func() {}
}

func TestSubscriptionReceiverFallbackAndResubscribe(t *testing.T) {
	a, b := pair(t)
	if err := b.subscribeWith(subscribeInput{SessionID: "session-a", Mode: "tools"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.feeds["session-a"].status == "following" })
	a.activity(a.local().Generation, "activity", "excluded earlier text")
	a.activity(a.local().Generation, "tool", "Tool started: Bash")
	eventually(t, func() bool { b.mu.Lock(); defer b.mu.Unlock(); return len(b.incoming) == 1 })
	b.mu.Lock()
	f := b.feeds["session-a"]
	if b.incoming[0].event.Kind != "tool" {
		t.Fatal("unfiltered old publisher")
	}
	b.mu.Unlock()
	if err := b.subscribeWith(subscribeInput{SessionID: "session-a", Kinds: []string{"turn"}}); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	if b.feeds["session-a"] != f || len(b.incoming) != 0 {
		t.Fatal("re-subscribe replaced follower or retained excluded queue")
	}
	b.mu.Unlock()
	a.activity(a.local().Generation, "turn", "Turn completed")
	eventually(t, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.incoming) == 1 && b.incoming[0].event.Kind == "turn"
	})
	if err := b.subscribeWith(subscribeInput{SessionID: "session-a", Mode: "text"}); err != nil {
		t.Fatal(err)
	}
	a.activity(a.local().Generation, "activity", "new text")
	eventually(t, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.incoming) == 1 && b.incoming[0].event.Text == "new text"
	})
}

func TestSubscriptionDigest(t *testing.T) {
	n := node(t)
	bind(t, n, "local", t.TempDir())
	notifications := make(chan string, 8)
	n.ready = true
	n.notify = func(_ context.Context, text string, meta map[string]string) error {
		if meta["kind"] != "digest" {
			return fmt.Errorf("unexpected kind %s", meta["kind"])
		}
		notifications <- text
		return nil
	}
	n.feeds["a"] = &feed{token: "a", cancel: func() {}, policy: subscriptionPolicy{mode: "digest", window: time.Second}}
	for i := 0; i < 3; i++ {
		n.enqueueLocked(&pb.Event{NodeId: "peer", SessionId: "a", Kind: "activity", Text: "untrusted text", MessageId: fmt.Sprint(i)}, "a", "a", 1)
	}
	n.wg.Add(1)
	go func() { defer n.wg.Done(); n.deliver(n.ctx) }()
	select {
	case <-notifications:
		t.Fatal("digest delivered before window")
	case <-time.After(300 * time.Millisecond):
	}
	select {
	case text := <-notifications:
		if !strings.Contains(text, "activity: 3") {
			t.Fatal(text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no digest")
	}
	n.mu.Lock()
	if len(n.incoming) != 0 || n.stats.Written != 3 {
		t.Fatal("digest left events behind")
	}
	n.mu.Unlock()
}

func TestSubscriptionLegacyNotification(t *testing.T) {
	n := node(t)
	bind(t, n, "local", t.TempDir())
	policy, err := resolveSubscription(config.Subscriptions{}, nil, subscribeInput{})
	if err != nil {
		t.Fatal(err)
	}
	n.feeds["remote"] = &feed{token: "feed", cancel: func() {}, policy: policy}
	n.ready = true
	type notification struct {
		text string
		meta map[string]string
	}
	got := make(chan notification, 1)
	n.notify = func(_ context.Context, text string, meta map[string]string) error {
		got <- notification{text, meta}
		return nil
	}
	for i, kind := range []string{"position", "activity", "turn"} {
		e := &pb.Event{SessionId: "remote", NodeId: "peer", StreamId: "stream", Generation: 1, Sequence: uint64(i + 1), MessageId: fmt.Sprint(i), Kind: kind, Text: "hello"}
		if !n.enqueueLocked(e, "remote", "feed", 1) {
			t.Fatal("event rejected")
		}
	}
	n.wg.Add(1)
	go func() { defer n.wg.Done(); n.deliver(n.ctx) }()
	select {
	case v := <-got:
		if v.text != "[activity] hello" {
			t.Fatal(v.text)
		}
		want := map[string]string{"session_id": "remote", "node_id": "peer", "stream_id": "stream", "sequence": "2", "kind": "activity", "generation": "1", "message_id": "1"}
		if !reflect.DeepEqual(v.meta, want) {
			t.Fatalf("metadata changed: %#v", v.meta)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("no legacy notification")
	}
}

func TestSubscriptionDigestOverflow(t *testing.T) {
	n := node(t)
	bind(t, n, "local", t.TempDir())
	n.feeds["remote"] = &feed{token: "feed", cancel: func() {}, policy: subscriptionPolicy{mode: "digest", window: time.Minute}}
	for i := 0; i < maxQueue+3; i++ {
		if !n.enqueueLocked(&pb.Event{SessionId: "remote", NodeId: "peer", MessageId: fmt.Sprint(i), Kind: "activity"}, "remote", "feed", 1) {
			t.Fatal("overflow must advance cursor")
		}
	}
	if len(n.incoming) != maxQueue || n.feeds["remote"].suppressed != 3 {
		t.Fatalf("queue=%d suppressed=%d", len(n.incoming), n.feeds["remote"].suppressed)
	}
}

func TestSubscriptionMCPArguments(t *testing.T) {
	a, n := pair(t)
	n.opts.Subscriptions = config.Subscriptions{DefaultMode: "markers"}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ss, err := n.mcpServer().Connect(n.ctx, &channelTransport{base: serverTransport}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(n.ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	for _, tc := range []struct {
		args map[string]any
		mode string
		fail bool
	}{
		{map[string]any{}, "markers", false},
		{map[string]any{"mode": "tools", "tools": []string{"Bash"}, "include_text": false, "max_per_minute": 10}, "tools", false},
		{map[string]any{"mode": "digest", "window": "10m", "kinds": []string{"tool"}}, "digest", false},
		{map[string]any{"mode": "bad"}, "digest", true},
		{map[string]any{"kinds": []string{"bad"}}, "digest", true},
		{map[string]any{"tools": []string{"bad"}}, "digest", true},
	} {
		tc.args["session_id"] = a.local().SessionId
		r, err := cs.CallTool(n.ctx, &mcp.CallToolParams{Name: "subscribe", Arguments: tc.args})
		if err != nil || r.IsError != tc.fail {
			t.Fatalf("%v: %v %v", tc.args, r, err)
		}
		n.mu.Lock()
		mode := n.feeds[a.local().SessionId].policy.mode
		n.mu.Unlock()
		if mode != tc.mode {
			t.Fatalf("got %s want %s", mode, tc.mode)
		}
	}
}
