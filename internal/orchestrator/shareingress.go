package orchestrator

// The tunnels. Each share has one control WebSocket from the CLI. When the
// proxy needs a connection it sends an open id, and the CLI answers by opening
// a data WebSocket naming it. Holding that id is what authorises the connection.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"

	"github.com/pilldrops-oss/pilldrops/internal/config"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// Vars so a test does not wait out the real intervals.
var (
	shareDialTimeout  = 10 * time.Second
	shareRefreshEvery = 5 * time.Second
)

const (
	sharePingEvery  = 30 * time.Second
	shareTouchEvery = 30 * time.Second
	shareReadWait   = 75 * time.Second
	shareHelloWait  = 10 * time.Second
)

var errShareBusy = errors.New("share has too many open connections")

// shareUpgrader refuses a browser page: only the CLI opens these, and it sends no Origin.
var shareUpgrader = websocket.Upgrader{
	ReadBufferSize: 32 << 10, WriteBufferSize: 32 << 10,
	CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" },
}

// shareHub is this replica's live tunnels and unanswered open ids. Zero value ready.
type shareHub struct {
	mu      sync.Mutex
	tunnels map[string]*shareTunnel
	pending map[string]chan net.Conn
}

func (h *shareHub) get(name string) *shareTunnel {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tunnels[name]
}

// put installs t and returns the tunnel it replaced. Newest wins: a CLI
// reconnecting after a network drop arrives before the old socket has timed out.
func (h *shareHub) put(t *shareTunnel) *shareTunnel {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.tunnels == nil {
		h.tunnels = map[string]*shareTunnel{}
	}
	old := h.tunnels[t.share.Name]
	h.tunnels[t.share.Name] = t
	return old
}

func (h *shareHub) drop(t *shareTunnel) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.tunnels[t.share.Name] == t {
		delete(h.tunnels, t.share.Name)
	}
}

// end stops a share's tunnel on this replica, if it is here.
func (h *shareHub) end(name, reason string) {
	if t := h.get(name); t != nil {
		t.end(reason)
	}
}

func (h *shareHub) addPending(ch chan net.Conn) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failing is not recoverable
	}
	id := hex.EncodeToString(b)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pending == nil {
		h.pending = map[string]chan net.Conn{}
	}
	h.pending[id] = ch
	return id
}

// takePending returns and removes an open id's waiter, nil when unknown or used.
func (h *shareHub) takePending(id string) chan net.Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := h.pending[id]
	delete(h.pending, id)
	return ch
}

// shareTunnel is one connected CLI.
type shareTunnel struct {
	hub       *shareHub
	share     store.Share
	localPort int
	ctrl      *websocket.Conn
	writeMu   sync.Mutex
	limiter   *rate.Limiter
	maxConns  int64
	transport *http.Transport
	proxy     *httputil.ReverseProxy

	open    atomic.Int64
	in, out atomic.Int64

	connMu sync.Mutex
	conns  map[*countedConn]struct{}

	done chan struct{}
	once sync.Once
}

func (t *shareTunnel) expired(now time.Time) bool { return now.Unix() >= t.share.ExpiresAt }

func (t *shareTunnel) send(msg map[string]any) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	_ = t.ctrl.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return t.ctrl.WriteJSON(msg)
}

// end stops the tunnel and cuts its open connections, so a takedown also stops
// a stream already running. An empty reason tells the CLI nothing: the only
// such caller is a newer tunnel replacing this one.
func (t *shareTunnel) end(reason string) {
	t.once.Do(func() {
		if reason != "" {
			_ = t.send(map[string]any{"type": "closed", "reason": reason})
		}
		close(t.done)
		_ = t.ctrl.Close()
		t.transport.CloseIdleConnections()
		t.connMu.Lock()
		defer t.connMu.Unlock()
		for c := range t.conns {
			_ = c.Conn.Close()
		}
	})
}

// dial asks the CLI for one data connection.
func (t *shareTunnel) dial(ctx context.Context) (net.Conn, error) {
	// Reserve the slot now: a dial can wait seconds for the CLI, and every
	// dial in that window would otherwise see the old count.
	if t.open.Add(1) > t.maxConns && t.maxConns > 0 {
		t.open.Add(-1)
		return nil, errShareBusy
	}
	tracked := false
	defer func() {
		if !tracked {
			t.open.Add(-1)
		}
	}()
	ch := make(chan net.Conn, 1)
	id := t.hub.addPending(ch)
	received := false
	defer func() {
		if received || t.hub.takePending(id) != nil {
			return
		}
		// The CLI claimed the id as this gave up; handleShareConn always sends
		// exactly one value, so close whatever it delivers.
		go func() {
			if c := <-ch; c != nil {
				_ = c.Close()
			}
		}()
	}()
	if err := t.send(map[string]any{"type": "open", "id": id}); err != nil {
		return nil, err
	}
	timer := time.NewTimer(shareDialTimeout)
	defer timer.Stop()
	select {
	case c := <-ch:
		received = true
		if c == nil {
			return nil, errors.New("the share client's data connection failed")
		}
		tracked = true
		return t.track(c), nil
	case <-timer.C:
		return nil, errors.New("the share client did not answer in time")
	case <-t.done:
		return nil, errors.New("the share tunnel ended")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *shareTunnel) track(c net.Conn) net.Conn {
	cc := &countedConn{Conn: c, t: t}
	t.connMu.Lock()
	if t.conns == nil {
		t.conns = map[*countedConn]struct{}{}
	}
	t.conns[cc] = struct{}{}
	t.connMu.Unlock()
	select {
	case <-t.done:
		// end() may already have swept t.conns; this one arrived too late for it.
		_ = cc.Close()
	default:
	}
	return cc
}

// countedConn keeps a tunnel's byte totals and its open-connection count.
// "in" is toward the local app, "out" is back to the visitor.
type countedConn struct {
	net.Conn
	t    *shareTunnel
	once sync.Once
}

func (c *countedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.t.out.Add(int64(n))
	return n, err
}

func (c *countedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.t.in.Add(int64(n))
	return n, err
}

func (c *countedConn) Close() error {
	c.once.Do(func() {
		c.t.open.Add(-1)
		c.t.connMu.Lock()
		delete(c.t.conns, c)
		c.t.connMu.Unlock()
	})
	return c.Conn.Close()
}

// handleShareControl runs one CLI's control channel until it ends.
func (s *Server) handleShareControl(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("share")
	if !s.validShareToken(bearer(r), id) {
		writeErr(w, http.StatusUnauthorized, "missing or invalid tunnel token")
		return
	}
	st := s.st.Load()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable, "store not ready")
		return
	}
	sh, ok, err := st.ShareByID(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusUnauthorized, "missing or invalid tunnel token")
		return
	}
	if state := sh.State(s.now().Unix()); state != "open" {
		writeJSON(w, http.StatusGone, map[string]string{"error": "this share link has ended: " + state, "reason": state})
		return
	}
	// Checked again here, or links created and left unconnected until they stop counting could all connect.
	if limit := config.Int("SHARE_MAX_PER_ORG", 5); limit > 0 {
		now := s.now()
		n, err := st.CountConnectedShares(r.Context(), sh.OrgID, sh.ID, now.Unix(), now.Add(-shareStaleAfter).Unix())
		if err != nil {
			writeErr(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		if n >= limit {
			writeJSONErr(w, http.StatusTooManyRequests, "share_limit",
				fmt.Sprintf("this org already has %d connected share links, the limit; stop one first", n))
			return
		}
	}
	ctrl, err := shareUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade has answered
	}
	ctrl.SetReadLimit(4 << 10)
	_ = ctrl.SetReadDeadline(time.Now().Add(shareHelloWait))
	var hello struct {
		Type string `json:"type"`
		Port int    `json:"port"`
	}
	if err := ctrl.ReadJSON(&hello); err != nil || hello.Type != "hello" || hello.Port < 1 || hello.Port > 65535 {
		_ = ctrl.Close()
		return
	}

	t := &shareTunnel{
		hub: &s.shares, share: sh, localPort: hello.Port, ctrl: ctrl,
		limiter:  shareLimiter(),
		maxConns: int64(config.Int("SHARE_MAX_CONNS", 64)),
		done:     make(chan struct{}),
	}
	t.proxy, t.transport = s.newShareProxy(t)
	refreshEvery := shareRefreshEvery // read before put, so a test's restore of it is ordered after
	if old := s.shares.put(t); old != nil {
		old.end("")
	}
	slog.Info("share connected", "component", "share", "name", sh.Name, "org", sh.OrgID)
	s.touchShare(st, id)

	go s.superviseShare(st, t, refreshEvery)

	// Reading is what notices a dead peer and delivers pongs; the CLI sends nothing else.
	_ = ctrl.SetReadDeadline(time.Now().Add(shareReadWait))
	ctrl.SetPongHandler(func(string) error { return ctrl.SetReadDeadline(time.Now().Add(shareReadWait)) })
	for {
		if _, _, err := ctrl.ReadMessage(); err != nil {
			break
		}
	}
	t.end("")
	s.shares.drop(t)
	in, out := t.in.Load(), t.out.Load()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := st.AddShareBytes(ctx, id, in, out); err != nil {
		slog.Warn("share totals not saved", "component", "share", "name", sh.Name, "err", err)
	}
	slog.Info("share disconnected", "component", "share", "name", sh.Name, "org", sh.OrgID, "bytes_in", in, "bytes_out", out)
}

func shareLimiter() *rate.Limiter {
	rps := config.Int("SHARE_RATE_PER_SEC", 50)
	if rps <= 0 {
		return rate.NewLimiter(rate.Inf, 0)
	}
	return rate.NewLimiter(rate.Limit(rps), rps*2)
}

func (s *Server) touchShare(st *store.Store, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := st.TouchShare(ctx, id, s.now().Unix()); err != nil {
		slog.Warn("share not touched", "component", "share", "id", id, "err", err)
	}
}

// superviseShare pings, records that the tunnel is alive, and re-reads the row,
// because a close or takedown may have been handled by another replica. A store
// error leaves the tunnel up: an outage should not cut every link.
func (s *Server) superviseShare(st *store.Store, t *shareTunnel, refreshEvery time.Duration) {
	refresh := time.NewTicker(refreshEvery)
	defer refresh.Stop()
	ping := time.NewTicker(sharePingEvery)
	defer ping.Stop()
	touch := time.NewTicker(shareTouchEvery)
	defer touch.Stop()
	for {
		select {
		case <-t.done:
			return
		case <-ping.C:
			_ = t.ctrl.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
		case <-touch.C:
			s.touchShare(st, t.share.ID)
		case <-refresh.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			sh, ok, err := st.ShareByID(ctx, t.share.ID)
			cancel()
			if err != nil || !ok {
				continue
			}
			if state := sh.State(s.now().Unix()); state != "open" {
				t.end(state)
				return
			}
		}
	}
}

// handleShareConn accepts the CLI's answer to one open id.
func (s *Server) handleShareConn(w http.ResponseWriter, r *http.Request, id string) {
	ch := s.shares.takePending(id)
	if ch == nil {
		writeErr(w, http.StatusNotFound, "unknown connection id")
		return
	}
	ws, err := shareUpgrader.Upgrade(w, r, nil)
	if err != nil {
		ch <- nil // Upgrade has answered; free the waiter
		return
	}
	ch <- &wsConn{ws: ws}
}

// serveShareApex routes the bare share domain.
func (s *Server) serveShareApex(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/tunnel" && r.Method == http.MethodGet:
		s.handleShareControl(w, r)
	case strings.HasPrefix(r.URL.Path, "/tunnel/conn/") && r.Method == http.MethodGet:
		s.handleShareConn(w, r, strings.TrimPrefix(r.URL.Path, "/tunnel/conn/"))
	case r.URL.Path == "/health":
		s.handleLive(w, r)
	default:
		http.NotFound(w, r)
	}
}
