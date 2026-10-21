package config

import (
	"errors"
	"net"
	"net/netip"
	"strconv"
)

// Gateway configures opt-in host pairing, independently of MCP gossip.
type Gateway struct {
	Mode      string `toml:"mode"`
	Listen    string `toml:"listen"`
	Advertise string `toml:"advertise"`
	// History is the global peer-history policy. It is read only from the file
	// and is inert while mode is off.
	History PeerHistory `toml:"history"`
}

// Validate rejects invalid gateway configuration before opening listeners.
func (g Gateway) Validate() error {
	switch g.Mode {
	case "", "off":
		if g.Listen != "" || g.Advertise != "" {
			return errors.New("gateway addresses require mode lan or tailscale")
		}
	case "lan", "tailscale":
	default:
		return errors.New("gateway.mode must be off, lan or tailscale")
	}
	for _, address := range []string{g.Listen, g.Advertise} {
		if address == "" {
			continue
		}
		if err := ValidateGatewayEndpoint(address, g.Mode); err != nil {
			return err
		}
	}
	return nil
}

// ValidateGatewayEndpoint enforces the same concrete endpoint policy at file
// validation and at the gateway transport boundary.
func ValidateGatewayEndpoint(address, mode string) error {
	host, port, err := net.SplitHostPort(address)
	n, e := strconv.Atoi(port)
	ip := net.ParseIP(host)
	if err != nil || e != nil || n < 1 || n > 65535 || ip == nil || ip.IsUnspecified() || len(address) > 128 {
		return errors.New("gateway addresses must be concrete IP:port endpoints (1-65535)")
	}
	allowed := false
	switch mode {
	case "lan":
		allowed = ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	case "tailscale":
		addr, err := netip.ParseAddr(host)
		allowed = err == nil && (ip.IsLoopback() || netip.MustParsePrefix("100.64.0.0/10").Contains(addr.Unmap()) || netip.MustParsePrefix("fd7a:115c:a1e0::/48").Contains(addr))
	}
	if !allowed {
		return errors.New("gateway endpoint outside selected address policy")
	}
	return nil
}
