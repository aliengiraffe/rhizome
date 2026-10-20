package config

import (
	"errors"
	"net"
	"strconv"
)

// Gateway configures opt-in host pairing, independently of MCP gossip.
type Gateway struct {
	Mode      string `toml:"mode"`
	Listen    string `toml:"listen"`
	Advertise string `toml:"advertise"`
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
		host, port, err := net.SplitHostPort(address)
		n, e := strconv.Atoi(port)
		if err != nil || e != nil || n < 1 || n > 65535 || net.ParseIP(host) == nil || net.ParseIP(host).IsUnspecified() {
			return errors.New("gateway addresses must be concrete IP:port endpoints (1-65535)")
		}
	}
	return nil
}
