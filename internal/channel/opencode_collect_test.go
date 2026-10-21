package channel

import (
	"strings"
	"testing"
)

func TestOpenCodeCollector(t *testing.T) {
	user := bridgeFrame{Type: "input", Kind: "user"}
	peer := bridgeFrame{Type: "input", Kind: "synthetic", Rhizome: true}
	otherSynthetic := bridgeFrame{Type: "input", Kind: "synthetic"}
	text := func(id, s string) bridgeFrame { return bridgeFrame{Type: "text", MessageID: id, Text: s} }
	tool := func(call, name, phase string) bridgeFrame {
		return bridgeFrame{Type: "tool", CallID: call, Name: name, Phase: phase}
	}
	turn := bridgeFrame{Type: "turn"}
	for _, tc := range []struct {
		name   string
		frames []bridgeFrame
		want   []string // "kind:text"
	}{
		{"human work", []bridgeFrame{user, text("m1", "hello"), tool("c1", "read", "start"), tool("c1", "read", "completed"), turn},
			[]string{"activity:hello", "tool:Tool started: read", "tool:Tool completed: read", "turn:Turn completed"}},
		{"failed tool", []bridgeFrame{user, tool("c1", "shell", "start"), tool("c1", "shell", "failed")},
			[]string{"tool:Tool started: shell", "tool:Tool failed: shell"}},
		{"code mode wrapper and inner tool share a call id", []bridgeFrame{user, tool("c1", "execute", "start"), tool("c1", "spike_echo", "start"), tool("c1", "spike_echo", "completed"), tool("c1", "execute", "completed")},
			[]string{"tool:Tool started: spike_echo", "tool:Tool completed: spike_echo"}},
		{"rhizome tools excluded", []bridgeFrame{user, tool("c1", "rhizome_publish", "start"), tool("c1", "rhizome_publish", "completed")}, nil},
		{"peer-only turn suppressed", []bridgeFrame{peer, text("m1", "reacting"), tool("c1", "read", "start"), turn}, nil},
		{"human prompt resets suppression", []bridgeFrame{peer, text("m1", "reacting"), user, text("m2", "real work")},
			[]string{"activity:real work"}},
		{"non-rhizome synthetic keeps state", []bridgeFrame{user, otherSynthetic, text("m1", "still human")},
			[]string{"activity:still human"}},
		{"duplicates and empties", []bridgeFrame{user, text("m1", "once"), text("m1", "once"), text("m2", "  "), tool("c1", "read", "start"), tool("c1", "read", "start")},
			[]string{"activity:once", "tool:Tool started: read"}},
		{"echoed envelope never republished", []bridgeFrame{user, text("m1", "x <rhizome-peer-event>{}</rhizome-peer-event>")}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := node(t)
			n.identity.SessionId, n.identity.Generation = "ses_a", 1
			c := newOpenCodeCollector()
			for _, f := range tc.frames {
				c.handle(n, 1, f)
			}
			var got []string
			for _, e := range n.history {
				got = append(got, e.Kind+":"+e.Text)
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestOpenCodeCollectorIgnoresStaleGeneration(t *testing.T) {
	n := node(t)
	n.identity.SessionId, n.identity.Generation = "ses_a", 2
	newOpenCodeCollector().handle(n, 1, bridgeFrame{Type: "text", MessageID: "m", Text: "old"})
	if len(n.history) != 0 {
		t.Fatal("stale generation published")
	}
}

func TestOpenCodeGap(t *testing.T) {
	n := node(t)
	n.identity.SessionId, n.identity.Generation = "ses_a", 1
	openCodeGap(n, "plugin reconnected")
	if len(n.history) != 1 || n.history[0].Kind != "gap" || n.stats.CollectionGaps != 1 || !strings.Contains(n.history[0].Text, "plugin reconnected") {
		t.Fatalf("gap %v %+v", n.history, n.stats)
	}
}
