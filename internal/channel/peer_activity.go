package channel

import "rhizome/internal/config"

func (n *Node) peerActivityPolicy() config.PeerActivityPolicy        { return n.peerActivity }
func (s *historySink) peerActivityPolicy() config.PeerActivityPolicy { return s.peerActivity }

// peerSink gates only peer-turn eligibility. The underlying sink still owns
// secret redaction, detail opt-in and live generation/ownership checks.
type peerSink struct {
	activitySink
	peer, unknown bool
}

func (s peerSink) activity(gen uint64, kind, text string) {
	if !s.unknown && s.peerActivityPolicy().Allows(s.peer, kind) {
		s.activitySink.activity(gen, kind, text)
	}
}
