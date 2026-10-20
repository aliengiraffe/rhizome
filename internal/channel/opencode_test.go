package channel

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func toolNames(t *testing.T, s *mcp.Server) []string {
	t.Helper()
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := s.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

func TestMakeServerToolsByAgent(t *testing.T) {
	none := func(*mcp.CallToolRequest) (*Node, error) { return nil, errUnbound }
	history := newHistoryService(historyScope{Runtime: t.TempDir()})
	shared := []string{"accept_file", "cancel_file", "list_files", "list_sessions", "publish", "read_session_history", "search_sessions", "send", "send_file", "set_session_name", "subscribe", "unsubscribe"}
	withObserve := append([]string{"observe_session"}, shared...)
	sort.Strings(withObserve)
	for _, tc := range []struct {
		kind agentKind
		want []string
	}{{agentClaude, withObserve}, {agentCodex, withObserve}, {agentOpenCode, shared}} {
		names := toolNames(t, makeServer(none, tc.kind, func() {}, history))
		if strings.Join(names, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("kind %d tools %v, want %v", tc.kind, names, tc.want)
		}
	}
}

func TestPeerEnvelope(t *testing.T) {
	got := peerEnvelope("hello", map[string]string{"session_id": "s"})
	for _, want := range []string{"untrusted", "<rhizome-peer-event>", `"content":"hello"`, `"session_id":"s"`, "</rhizome-peer-event>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("envelope %q missing %q", got, want)
		}
	}
}
