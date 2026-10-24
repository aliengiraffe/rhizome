package channel

import (
	"testing"

	pb "rhizome/proto"
)

func TestSessionStatusDerivation(t *testing.T) {
	const hostA, hostB = "host-a", "host-b"
	direct := func(node, host string) *pb.Identity {
		return &pb.Identity{NodeId: node, SessionId: "s", GatewayHost: host}
	}
	observer := func(node, host string) *pb.Identity {
		return &pb.Identity{NodeId: node, SessionId: "s", GatewayHost: host, ObservationOnly: true}
	}
	for _, tc := range []struct {
		name          string
		peers         []*pb.Identity
		plain, debug  string
		routable      bool
		historyRoutes bool
	}{
		{"no peers", nil, "not_discovered", "not_discovered", false, false},
		{"one observer", []*pb.Identity{observer("o1", "")}, "recent", "recent (observer)", false, false},
		{"two observers", []*pb.Identity{observer("o1", ""), observer("o2", "")}, "recent", "recent (observer x2)", false, false},
		{"one direct", []*pb.Identity{direct("d1", "")}, "live", "live (direct)", true, true},
		{"two direct", []*pb.Identity{direct("d1", ""), direct("d2", "")}, "live", "live (direct x2)", false, false},
		{"direct hides observer", []*pb.Identity{direct("d1", ""), observer("o1", "")}, "live", "live (direct)", true, true},
		{"self", []*pb.Identity{direct("me", "")}, "live", "live (direct, self)", true, false},
		{"self and duplicate", []*pb.Identity{direct("me", ""), direct("d1", "")}, "live", "live (direct x2, self)", false, false},
		{"paired host", []*pb.Identity{direct("p1", hostA)}, "live", "live (direct)", true, true},
		{"two hosts", []*pb.Identity{direct("p1", hostA), direct("p2", hostB)}, "live", "live (direct x2, hosts x2)", false, false},
		{"direct and remote observer", []*pb.Identity{direct("d1", ""), observer("o1", hostA)}, "live", "live (direct, observer, hosts x2)", false, false},
		{"observers on two hosts", []*pb.Identity{observer("o1", ""), observer("o2", hostA)}, "recent", "recent (observer x2, hosts x2)", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := liveInfos(logicalPeers(tc.peers), "me")
			for _, debug := range []bool{false, true} {
				want := tc.plain
				if debug {
					want = tc.debug
				}
				got, routable := info["s"].status(debug)
				if got != want || routable != tc.routable {
					t.Fatalf("debug=%v: %q/%v, want %q/%v", debug, got, routable, want, tc.routable)
				}
				view := &liveView{info: info, debug: debug}
				if got, capable := view.status("s"); got != want || capable != tc.historyRoutes {
					t.Fatalf("history debug=%v: %q/%v, want %q/%v", debug, got, capable, want, tc.historyRoutes)
				}
			}
		})
	}
	if recentCatalogStatus(false) != "recent" || recentCatalogStatus(true) != "recent (catalog)" {
		t.Fatal("catalog status")
	}
	// Paired-host history rows never infer live state.
	h := &historyService{}
	if r := h.row(&historySession{SessionID: "s", AgentType: "claude"}, nil, nil); r.LiveStatus != liveNotReported || r.DeliveryCapable {
		t.Fatalf("paired-host row %+v", r)
	}
}
