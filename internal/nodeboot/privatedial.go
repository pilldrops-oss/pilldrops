package nodeboot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

// NewPrivateDialer opens a sandbox's connection to a granted target through
// the control plane's connector hub, authenticated as this node.
// An unusable URL yields a dialer that always fails, logged once here, so a
// misconfigured node refuses private connections instead of failing to boot.
func NewPrivateDialer(orchURL, nodeKey string) func(ctx context.Context, sandboxID, target string, port int) (net.Conn, error) {
	dial := privateWSDialer(orchURL, nodeKey)
	return func(ctx context.Context, sandboxID, target string, port int) (net.Conn, error) {
		return dial(ctx, url.Values{"sandbox": {sandboxID}, "target": {target}, "port": {strconv.Itoa(port)}})
	}
}

// NewPrivateDestDialer opens a sandbox's connection to an address under a
// subnet or split-DNS grant, the same way.
func NewPrivateDestDialer(orchURL, nodeKey string) func(ctx context.Context, sandboxID string, dest netip.AddrPort) (net.Conn, error) {
	dial := privateWSDialer(orchURL, nodeKey)
	return func(ctx context.Context, sandboxID string, dest netip.AddrPort) (net.Conn, error) {
		return dial(ctx, url.Values{"sandbox": {sandboxID}, "dest": {dest.String()}})
	}
}

// privateWSDialer is the WebSocket dial to /internal/private/dial both share.
func privateWSDialer(orchURL, nodeKey string) func(ctx context.Context, q url.Values) (net.Conn, error) {
	base, err := privateDialBase(orchURL)
	if err != nil {
		slog.Error("private access disabled", "component", "node", "err", err)
		return func(context.Context, url.Values) (net.Conn, error) { return nil, err }
	}
	return func(ctx context.Context, q url.Values) (net.Conn, error) {
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

// NewPrivateResolver relays a split-DNS lookup to the control plane's hub.
func NewPrivateResolver(orchURL, nodeKey string) func(ctx context.Context, sandboxID, name, qtype string) (sandbox.PrivateDNSReply, error) {
	endpoint := strings.TrimRight(orchURL, "/") + "/internal/private/dns"
	client := &http.Client{Timeout: 4 * time.Second}
	return func(ctx context.Context, sandboxID, name, qtype string) (sandbox.PrivateDNSReply, error) {
		body, _ := json.Marshal(map[string]string{"sandbox": sandboxID, "name": name, "qtype": qtype})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return sandbox.PrivateDNSReply{}, err
		}
		req.Header.Set("Authorization", "Bearer "+nodeKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return sandbox.PrivateDNSReply{}, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return sandbox.PrivateDNSReply{}, fmt.Errorf("private dns refused: %s", resp.Status)
		}
		var out sandbox.PrivateDNSReply
		err = json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&out)
		return out, err
	}
}

// privateDialBase is ORCHESTRATOR_URL with its scheme moved to WebSocket.
func privateDialBase(orchURL string) (string, error) {
	u, err := url.Parse(strings.TrimRight(orchURL, "/"))
	if err != nil {
		return "", fmt.Errorf("ORCHESTRATOR_URL: %w", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("ORCHESTRATOR_URL %q: want an http or https URL", orchURL)
	}
	if u.Host == "" {
		return "", fmt.Errorf("ORCHESTRATOR_URL %q has no host", orchURL)
	}
	return u.String(), nil
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
