package nodeboot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// NewPrivateDialer opens a sandbox's connection to a granted target through
// the control plane's connector hub, authenticated as this node.
func NewPrivateDialer(orchURL, nodeKey string) func(ctx context.Context, sandboxID, target string, port int) (net.Conn, error) {
	base := "ws" + strings.TrimPrefix(strings.TrimRight(orchURL, "/"), "http")
	return func(ctx context.Context, sandboxID, target string, port int) (net.Conn, error) {
		q := url.Values{"sandbox": {sandboxID}, "target": {target}, "port": {strconv.Itoa(port)}}
		d := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
		ws, resp, err := d.DialContext(ctx, base+"/internal/private/dial?"+q.Encode(), http.Header{"Authorization": {"Bearer " + nodeKey}})
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			if resp != nil {
				return nil, fmt.Errorf("private dial refused: %s", resp.Status)
			}
			return nil, err
		}
		return &wsStream{ws: ws}, nil
	}
}

// wsStream is a net.Conn over binary WebSocket frames, behaving as the
// orchestrator's wsConn does; the tier guard keeps the node from importing it.
type wsStream struct {
	ws      *websocket.Conn
	reader  io.Reader
	writeMu sync.Mutex
}

func (c *wsStream) Read(p []byte) (int, error) {
	for {
		if c.reader == nil {
			kind, r, err := c.ws.NextReader()
			if err != nil {
				var ce *websocket.CloseError
				if errors.As(err, &ce) {
					return 0, io.EOF
				}
				return 0, err
			}
			if kind != websocket.BinaryMessage {
				continue
			}
			c.reader = r
		}
		n, err := c.reader.Read(p)
		if errors.Is(err, io.EOF) {
			c.reader = nil
			err = nil
		}
		if n > 0 || err != nil {
			return n, err
		}
	}
}

func (c *wsStream) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *wsStream) Close() error {
	_ = c.ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	return c.ws.Close()
}

func (c *wsStream) LocalAddr() net.Addr  { return c.ws.LocalAddr() }
func (c *wsStream) RemoteAddr() net.Addr { return c.ws.RemoteAddr() }

func (c *wsStream) SetDeadline(t time.Time) error {
	if err := c.ws.SetReadDeadline(t); err != nil {
		return err
	}
	return c.ws.SetWriteDeadline(t)
}
func (c *wsStream) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *wsStream) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }
