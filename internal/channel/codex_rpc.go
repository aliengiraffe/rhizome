package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Deliberately read-only. This client cannot load a thread, start a turn,
// steer work, answer approvals, or manage the user's daemon.
type appRPCError struct {
	code            int
	message, method string
}

func (e *appRPCError) Error() string { return fmt.Sprintf("App Server %s error %d", e.method, e.code) }

type codexReader interface {
	call(context.Context, string, any, any) error
	close()
}
type appReader struct {
	mu     sync.Mutex
	socket string
	conn   *websocket.Conn
	seq    uint64
}

func (a *appReader) close() { a.mu.Lock(); defer a.mu.Unlock(); a.disconnect() }
func (a *appReader) disconnect() {
	if a.conn != nil {
		_ = a.conn.Close()
		a.conn = nil
	}
}
func (a *appReader) call(ctx context.Context, method string, params, out any) error {
	switch method {
	case "thread/read", "thread/turns/list", "thread/items/list":
	default:
		return errors.New("App Server method is not read-only")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if a.conn == nil {
		d := websocket.Dialer{HandshakeTimeout: 5 * time.Second, NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", a.socket)
		}}
		c, resp, err := d.DialContext(ctx, "ws://localhost/", http.Header{})
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			return err
		}
		c.SetReadLimit(8 * 1024 * 1024)
		a.conn = c
		if err := a.request(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "rhizome", "version": "0.3.0"}, "capabilities": map[string]any{"experimentalApi": true}}, nil); err != nil {
			a.disconnect()
			return err
		}
		if err := a.conn.WriteJSON(map[string]any{"method": "initialized"}); err != nil {
			a.disconnect()
			return err
		}
	}
	err := a.request(ctx, method, params, out)
	if err != nil {
		a.disconnect()
	}
	return err
}
func (a *appReader) request(ctx context.Context, method string, params, out any) error {
	deadline, _ := ctx.Deadline()
	_ = a.conn.SetWriteDeadline(deadline)
	_ = a.conn.SetReadDeadline(deadline)
	// Cancellation interrupts the socket without waiting for the deadline.
	c := a.conn
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	a.seq++
	if err := a.conn.WriteJSON(map[string]any{"id": a.seq, "method": method, "params": params}); err != nil {
		return err
	}
	for {
		var r struct {
			ID     uint64          `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := a.conn.ReadJSON(&r); err != nil {
			return err
		}
		if r.ID != a.seq {
			continue
		}
		if r.Error != nil {
			return &appRPCError{r.Error.Code, r.Error.Message, method}
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(r.Result, out)
	}
}
