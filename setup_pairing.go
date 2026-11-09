package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"rhizome/internal/channel"
)

// gatewayAdmin is the local service control call setup uses for pairing;
// production is channel.GatewayAdmin and tests inject a fake.
type gatewayAdmin func(ctx context.Context, runtime string, cmd channel.GatewayCommand) (json.RawMessage, error)

const (
	// gatewayReadyWait bounds how long setup waits for the freshly started
	// service's gateway to answer its private control endpoint.
	gatewayReadyWait = 10 * time.Second
	// setupInviteExpiry is the validity of an invitation created by setup.
	setupInviteExpiry = "10m"
	// maxInvitation matches the CLI's bound on a pasted invitation.
	maxInvitation = 2048
)

// pairingJoin is the authoritative result of a join: the paired host and
// whether the gateway reports a live connection to it right now.
type pairingJoin struct {
	HostID    string
	Connected bool
}

// waitGatewayReady confirms the running service's gateway answers a peers
// query, polling until it does or the bound expires. A confirmed observer
// lock is not enough: the gateway starts after it and only in pairing modes.
func waitGatewayReady(ctx context.Context, runtime string, admin gatewayAdmin, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	var last error
	for {
		if _, err := admin(ctx, runtime, channel.GatewayCommand{Action: "peers"}); err == nil {
			return nil
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("gateway not ready: %w", last)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// pairingInvite creates an invitation through the service and returns the
// exact command to share plus the invitation ID for revocation. The secret is
// shown once by the caller and never logged.
func pairingInvite(ctx context.Context, runtime string, admin gatewayAdmin) (command, id string, err error) {
	out, err := admin(ctx, runtime, channel.GatewayCommand{Action: "invite", Expires: setupInviteExpiry})
	if err != nil {
		return "", "", err
	}
	var v map[string]string
	if err := json.Unmarshal(out, &v); err != nil || v["invitation"] == "" {
		return "", "", errors.New("invalid invitation response from the service")
	}
	return "rhizome join " + v["invitation"], v["id"], nil
}

// normalizeInvitation accepts the bare code or the whole pasted
// `rhizome join <code>` line and applies the CLI's bounds.
func normalizeInvitation(code string) (string, error) {
	code = strings.TrimSpace(code)
	code = strings.TrimSpace(strings.TrimPrefix(code, "rhizome join"))
	if code == "" {
		return "", errors.New("invitation is empty")
	}
	if len(code) > maxInvitation || strings.ContainsAny(code, " \t\r\n") {
		return "", errors.New("invitation must be the single code printed by `rhizome invite`")
	}
	return code, nil
}

// pairingJoinHost redeems an invitation through the service's existing join
// path (its trust checks are unchanged) and then reads the peers list, so the
// reported connection state comes from the gateway rather than the join reply.
func pairingJoinHost(ctx context.Context, runtime string, admin gatewayAdmin, code string) (pairingJoin, error) {
	code, err := normalizeInvitation(code)
	if err != nil {
		return pairingJoin{}, err
	}
	out, err := admin(ctx, runtime, channel.GatewayCommand{Action: "join", Code: code})
	if err != nil {
		return pairingJoin{}, err
	}
	var joined map[string]string
	if err := json.Unmarshal(out, &joined); err != nil || joined["host_id"] == "" {
		return pairingJoin{}, errors.New("invalid join response from the service")
	}
	result := pairingJoin{HostID: joined["host_id"]}
	peers, err := admin(ctx, runtime, channel.GatewayCommand{Action: "peers"})
	if err != nil {
		return result, nil
	}
	var rows []peerRow
	if json.Unmarshal(peers, &rows) == nil {
		for _, p := range rows {
			if p.HostID == result.HostID {
				result.Connected = p.Connected && !p.Pending
			}
		}
	}
	return result, nil
}

// inviteLines is the one-time display of a created invitation, worded so it
// is not mistaken for a connected peer.
func inviteLines(command, id string) []string {
	return []string{
		"Share this command privately with the other device (valid " + setupInviteExpiry + "):",
		"  " + command,
		"Invitation " + safeText(id) + " created; no peer is connected yet. After the other device runs it, `rhizome peers list` shows the host as connected (revoke with `rhizome peers revoke-invite " + safeText(id) + "`).",
	}
}

// joinLines reports a redeemed invitation with the gateway's current view.
func joinLines(j pairingJoin) []string {
	if j.Connected {
		return []string{"Paired with host " + safeText(j.HostID) + ": connected."}
	}
	return []string{"Paired with host " + safeText(j.HostID) + ": trust saved, connection not established yet; the service retries and `rhizome peers list` shows the state."}
}

// offerPairing is the plain-prompt pairing phase after the service and its
// gateway were confirmed ready. Declining prints the commands for later; a
// failed invitation or join is reported with the retry path and returned.
func offerPairing(ctx context.Context, p prompter, runtime, path string, out io.Writer, admin gatewayAdmin) error {
	fmt.Fprint(out, `
Pair this device now? The service is running and ready for pairing.
  1) invite   create an invitation for another device to join this one
  2) join     paste an invitation created on another device
  3) later
`)
	for {
		ans, err := p.ask("Pairing", "3")
		if err != nil {
			return err
		}
		switch strings.ToLower(ans) {
		case "1", "invite":
			command, id, err := pairingInvite(ctx, runtime, admin)
			if err != nil {
				return fmt.Errorf("invitation not created (configuration, agents and service steps above completed): %w; retry with `rhizome invite`", err)
			}
			for _, line := range inviteLines(command, id) {
				fmt.Fprintln(out, line)
			}
			return nil
		case "2", "join":
			code, err := p.ask("Invitation (the code printed by `rhizome invite` on the other device)", "")
			if err != nil {
				return err
			}
			j, err := pairingJoinHost(ctx, runtime, admin, code)
			if err != nil {
				return fmt.Errorf("join failed (configuration, agents and service steps above completed): %w; retry with `rhizome join <invitation>` or a fresh invitation", err)
			}
			for _, line := range joinLines(j) {
				fmt.Fprintln(out, line)
			}
			return nil
		case "3", "later", "":
			for _, line := range pairingNextSteps(path, true) {
				fmt.Fprintln(out, line)
			}
			return nil
		}
		fmt.Fprintln(out, "Choose 1, 2 or 3 (or invite, join, later).")
	}
}
