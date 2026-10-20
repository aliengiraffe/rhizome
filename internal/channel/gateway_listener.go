package channel

import (
	"net"
	"sync"
)

// gatewayListener bounds accepted sockets, including unauthenticated TLS handshakes.
type gatewayListener struct {
	net.Listener
	slots chan struct{}
}
type gatewayConn struct {
	net.Conn
	slots chan struct{}
	once  sync.Once
}

func (l *gatewayListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &gatewayConn{Conn: c, slots: l.slots}, nil
		default:
			_ = c.Close()
		}
	}
}
func (c *gatewayConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { <-c.slots })
	return err
}
