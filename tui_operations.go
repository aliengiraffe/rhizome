package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"rhizome/internal/channel"
	"rhizome/internal/config"
	"rhizome/internal/service"
)

type peerRow struct {
	HostID    string `json:"host_id"`
	Endpoint  string `json:"endpoint"`
	Connected bool   `json:"connected"`
	Pending   bool   `json:"pending"`
}

func peersPanel(c config.Config) *panelModel {
	return &panelModel{work: newUIWork(), title: "Paired hosts (read-only)", readOnly: true, width: 80, height: 24, load: func(ctx context.Context) panelResult {
		b, err := channel.GatewayAdmin(ctx, c.RuntimeDir, channel.GatewayCommand{Action: "peers"})
		if err != nil {
			return panelResult{err: err}
		}
		var peers []peerRow
		if err = json.Unmarshal(b, &peers); err != nil {
			return panelResult{err: fmt.Errorf("invalid peers response: %w", err)}
		}
		sort.Slice(peers, func(i, j int) bool { return peers[i].HostID < peers[j].HostID })
		return panelResult{body: "No paired hosts.", peers: peers}
	}}
}
func servicePanel(c config.Config, readOnly bool) *panelModel {
	sc := service.FromConfig(c)
	return &panelModel{work: newUIWork(), title: "Rhizome service", readOnly: readOnly, width: 80, height: 24, act: func(ctx context.Context, action string) error { return service.ManageQuiet(ctx, action, sc) }, load: func(ctx context.Context) panelResult {
		s, err := service.Inspect(ctx, sc)
		body := fmt.Sprintf("Definition installed: %t\nObserver running: %t\nConfig: %s", s.Installed, s.Running, safeText(c.Path))
		if len(s.Health) > 0 {
			var health map[string]json.RawMessage
			if e := json.Unmarshal(s.Health, &health); e != nil {
				return panelResult{body: body, err: e}
			}
			body += "\nObserver health (last snapshot; may be stale):"
			for _, key := range []string{"updated_at", "observed_sessions", "watched_directories", "tracked_files", "degraded", "parser_skips", "suppressed_events", "metadata_unavailable"} {
				if value, ok := health[key]; ok {
					body += "\n" + key + ": " + safeText(strings.TrimSpace(string(value)))
				}
			}
		} else {
			body += "\nObserver health unavailable."
		}
		return panelResult{body: body, err: err}
	}}
}
