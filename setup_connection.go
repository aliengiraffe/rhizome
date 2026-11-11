package main

import (
	"context"
	"fmt"
	"time"

	"rhizome/internal/channel"
	"rhizome/internal/config"
)

// Connection models setup offers. The labels double as the stored wizard
// answers; only this device is the default of a fresh setup.
const (
	connectLocal  = "only this device"
	connectPaired = "paired devices"
	connectDirect = "direct network"
)

// connectChoices lists the connection models in the order setup presents them.
var connectChoices = []string{connectLocal, connectPaired, connectDirect}

// tailscaleProbeTimeout bounds the advisory detection in both setup flows.
const tailscaleProbeTimeout = 15 * time.Second

// tailscaleProbe is the injectable read-only detection; production uses
// channel.ProbeTailscale.
type tailscaleProbe func(context.Context) channel.TailscaleReport

// probeTailscaleBounded runs probe with the setup time bound on top of ctx.
func probeTailscaleBounded(ctx context.Context, probe tailscaleProbe) channel.TailscaleReport {
	ctx, cancel := context.WithTimeout(ctx, tailscaleProbeTimeout)
	defer cancel()
	return probe(ctx)
}

// connectExplanation is the help shown at the decision point: what the model
// connects, and whether it needs the service, pairing or a shared token.
func connectExplanation(choice string) string {
	switch choice {
	case connectPaired:
		return "Connect this device with your other machines, or a colleague's, over Tailscale or a LAN. The background service pairs hosts through an explicit invitation per device and keeps each host's token private; trust is per host and revocable. Requires the service; MCP networking stays local."
	case connectDirect:
		return "Advanced: every MCP process on this device joins a shared-token gossip network on the LAN or tailnet. Any process holding the cluster token sees every session; there is no per-host pairing or service. Separate from paired devices; the two cannot be combined."
	default:
		return "Sessions on this machine only. MCP networking stays on loopback, nothing listens on the network and no service or pairing is needed. To pair devices later, run setup again."
	}
}

// transportExplanation describes the transport under paired devices.
func transportExplanation(transport string, r *channel.TailscaleReport) string {
	switch transport {
	case "tailscale":
		s := "Gateway listens on this device's Tailscale IPv4 (port 7947), resolved when the service starts; no address entry needed. Both hosts must share a tailnet whose ACLs allow the connection."
		if r != nil && !r.Usable() {
			s += " Tailscale is not usable here yet: the service will refuse to start until it is."
		}
		return s
	default:
		return "Gateway listens on a private LAN interface (port 7947) chosen when the service starts; set custom endpoints when the automatic choice is wrong. Both hosts must reach each other on the LAN."
	}
}

// tailscaleSummary renders the probe for display, with the recommendation
// when Tailscale is usable and the next step when it is not.
func tailscaleSummary(r channel.TailscaleReport) string {
	if r.Usable() {
		return fmt.Sprintf("Tailscale: usable (this device: %s). Recommended for connecting your own devices: paired devices over Tailscale.", safeText(r.Address))
	}
	return "Tailscale: " + safeText(r.Detail) + ". Local-only setup and LAN options are unaffected."
}

// recommendedTransport is the pairing transport setup suggests: Tailscale
// when it is usable now, otherwise the LAN.
func recommendedTransport(r *channel.TailscaleReport) string {
	if r != nil && r.Usable() {
		return "tailscale"
	}
	return "lan"
}

// storedConnection derives the connection model and transport a stored
// configuration already expresses, so prompts default to it.
func storedConnection(c config.Config) (choice, transport string) {
	switch c.Gateway.Mode {
	case "lan", "tailscale":
		return connectPaired, c.Gateway.Mode
	}
	switch c.Network.Mode {
	case config.ModeLAN, config.ModeTailscale:
		return connectDirect, c.Network.Mode
	}
	return connectLocal, ""
}

// connectionPlan is the staged connection decision as the review and the
// plain summary describe it: the model, the transport, gateway endpoints and
// every change from the stored configuration.
type connectionPlan struct {
	Choice, Transport string
	Listen, Advertise string // gateway endpoints; blank is automatic
	Stored            config.Config
	Tailscale         *channel.TailscaleReport
}

// networkMode is the MCP networking the plan writes.
func (p connectionPlan) networkMode() string {
	if p.Choice == connectDirect {
		return p.Transport
	}
	return config.ModeLocal
}

// gatewayMode is the gateway pairing mode the plan writes.
func (p connectionPlan) gatewayMode() string {
	if p.Choice == connectPaired {
		return p.Transport
	}
	return "off"
}

func (p connectionPlan) lines() []string {
	var out []string
	switch p.Choice {
	case connectPaired:
		out = append(out, fmt.Sprintf("Connection: paired devices over %s (gateway pairing through the background service; MCP networking stays local)", p.Transport))
		switch {
		case p.Listen == "" && p.Advertise == "" && p.Transport == "tailscale":
			out = append(out, "Gateway endpoints: automatic (this device's Tailscale IPv4, port 7947, resolved when the service starts)")
		case p.Listen == "" && p.Advertise == "":
			out = append(out, "Gateway endpoints: automatic (a private LAN interface, port 7947, chosen when the service starts)")
		default:
			out = append(out, "Gateway listen: "+defaultString(safeText(p.Listen), "automatic"), "Gateway advertise: "+defaultString(safeText(p.Advertise), "same as listen"))
		}
		if p.Transport == "tailscale" && p.Tailscale != nil {
			out = append(out, tailscaleSummary(*p.Tailscale))
		}
	case connectDirect:
		out = append(out, fmt.Sprintf("Connection: direct %s network (shared-token gossip in every MCP process; gateway pairing off)", p.Transport))
		if p.Transport == "tailscale" && p.Tailscale != nil {
			out = append(out, tailscaleSummary(*p.Tailscale))
		}
	default:
		out = append(out, "Connection: only this device (loopback MCP networking; gateway pairing off; no service required)")
	}
	oldNetwork, newNetwork := defaultString(p.Stored.Network.Mode, config.ModeLocal), p.networkMode()
	switch {
	case oldNetwork != config.ModeLocal && newNetwork == config.ModeLocal:
		out = append(out, fmt.Sprintf("Change: MCP networking switches from %s gossip to local; its %s options are dropped.", oldNetwork, oldNetwork))
	case oldNetwork == config.ModeLocal && newNetwork != config.ModeLocal:
		out = append(out, fmt.Sprintf("Change: MCP networking switches from local to %s gossip; peers holding the cluster token can reach this device.", newNetwork))
	case oldNetwork != newNetwork:
		out = append(out, fmt.Sprintf("Change: MCP networking switches from %s to %s gossip; the %s options are dropped.", oldNetwork, newNetwork, oldNetwork))
	}
	oldGateway, newGateway := defaultString(p.Stored.Gateway.Mode, "off"), p.gatewayMode()
	switch {
	case oldGateway != "off" && newGateway == "off":
		out = append(out, fmt.Sprintf("Change: gateway pairing turns off (was %s); paired hosts stay stored but are unreachable until it is enabled again.", oldGateway))
	case oldGateway == "off" && newGateway != "off":
		out = append(out, fmt.Sprintf("Change: gateway pairing turns on over %s; the service must run before devices can pair.", newGateway))
	case oldGateway != newGateway:
		out = append(out, fmt.Sprintf("Change: gateway transport switches from %s to %s; existing pairings used %s endpoints and may need to be re-paired.", oldGateway, newGateway, oldGateway))
	}
	return out
}

// pairingNextSteps are the exact commands for pairing later, shown whenever
// setup leaves a paired-devices configuration without a verified pairing.
func pairingNextSteps(path string, serviceRunning bool) []string {
	var out []string
	if !serviceRunning {
		out = append(out, "Pair later: start the service first: rhizome service install --config "+safeText(path))
	} else {
		out = append(out, "Pair later with the running service:")
	}
	return append(out,
		"  on one device: rhizome invite            (prints the `rhizome join …` command; share it privately)",
		"  on the other:  rhizome join <invitation>  then `rhizome peers list` shows whether it is connected")
}

// connectionSummary describes a saved configuration in connection terms.
func connectionSummary(c config.Config) string {
	choice, transport := storedConnection(c)
	return connectionPlan{Choice: choice, Transport: transport, Listen: c.Gateway.Listen, Advertise: c.Gateway.Advertise, Stored: c}.lines()[0]
}
