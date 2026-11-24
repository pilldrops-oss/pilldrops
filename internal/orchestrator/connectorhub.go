package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

const (
	connectorOpenWait    = 10 * time.Second
	maxConnsPerConnector = 1000
	connectorHelloWait   = 10 * time.Second
)

// connectorPingEvery is how often the hub pings each instance. A var so a test can shorten it.
var connectorPingEvery = 30 * time.Second

type connectorInstance struct {
	connectorID, instanceID string
	keyID                   string // the connector key its session came from; empty for an API key
	targets                 []store.ConnectorTarget
	subnets                 []netip.Prefix
	suffixes                []string
	ctrl                    *websocket.Conn
	writeMu                 sync.Mutex
	conns                   sync.Map // net.Conn -> struct{}
	done                    chan struct{}
	once                    sync.Once
}

func (in *connectorInstance) send(v any) error {
	in.writeMu.Lock()
	defer in.writeMu.Unlock()
	_ = in.ctrl.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return in.ctrl.WriteJSON(v)
}

func (in *connectorInstance) end(reason string) {
	in.once.Do(func() {
		_ = in.send(map[string]any{"type": "closed", "reason": reason})
		_ = in.ctrl.Close()
		in.conns.Range(func(k, _ any) bool { _ = k.(net.Conn).Close(); return true })
		close(in.done)
	})
}

func (in *connectorInstance) serves(target string, port int) bool {
	for _, t := range in.targets {
		if t.Name == target && t.Port == port {
			return true
		}
	}
	return false
}

func (in *connectorInstance) coversAddr(a netip.Addr) bool {
	for _, p := range in.subnets {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// servesSuffix reports whether the instance announced s or a suffix above it.
func (in *connectorInstance) servesSuffix(s string) bool {
	for _, a := range in.suffixes {
		if sandbox.UnderSuffix(s, a) {
			return true
		}
	}
	return false
}

type connectorHub struct {
	mu        sync.Mutex
	instances map[string]map[string]*connectorInstance
	pending   map[string]chan net.Conn
	active    map[string]*atomic.Int64
	next      atomic.Uint64
}

func (h *connectorHub) init() {
	if h.instances == nil {
		h.instances = map[string]map[string]*connectorInstance{}
		h.pending = map[string]chan net.Conn{}
		h.active = map[string]*atomic.Int64{}
	}
}

// add registers an instance; a reconnect under the same instance id replaces
// the old socket. ok is false when the connector is at its instance limit.
func (h *connectorHub) add(in *connectorInstance) (old *connectorInstance, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.init()
	set := h.instances[in.connectorID]
	if set == nil {
		set = map[string]*connectorInstance{}
		h.instances[in.connectorID] = set
	}
	old = set[in.instanceID]
	if old == nil && len(set) >= maxConnectorInstances {
		return nil, false
	}
	set[in.instanceID] = in
	return old, true
}

func (h *connectorHub) remove(in *connectorInstance) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set := h.instances[in.connectorID]; set != nil && set[in.instanceID] == in {
		delete(set, in.instanceID)
		if len(set) == 0 {
			delete(h.instances, in.connectorID)
		}
	}
}

func (h *connectorHub) count(connectorID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.instances[connectorID])
}

// union is what the connector serves now, or the stored list when no
// instance is online.
func (h *connectorHub) union(connectorID string, stored []store.ConnectorTarget) []store.ConnectorTarget {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.instances[connectorID]
	if len(set) == 0 {
		return stored
	}
	seen := map[string]store.ConnectorTarget{}
	for _, in := range set {
		for _, t := range in.targets {
			seen[t.Name] = t
		}
	}
	out := make([]store.ConnectorTarget, 0, len(seen))
	for _, t := range seen {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// routes is what the connector's online instances announce now, or the
// stored lists when none is online.
func (h *connectorHub) routes(connectorID string, storedSubnets, storedSuffixes []string) (subnets, suffixes []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.instances[connectorID]
	if len(set) == 0 {
		return storedSubnets, storedSuffixes
	}
	nets, names := map[string]bool{}, map[string]bool{}
	for _, in := range set {
		for _, p := range in.subnets {
			nets[p.String()] = true
		}
		for _, d := range in.suffixes {
			names[d] = true
		}
	}
	return slices.Sorted(maps.Keys(nets)), slices.Sorted(maps.Keys(names))
}

// candidates returns instances serving the target, rotated so load spreads.
func (h *connectorHub) candidates(connectorID, target string, port int) []*connectorInstance {
	h.mu.Lock()
	var out []*connectorInstance
	for _, in := range h.instances[connectorID] {
		if in.serves(target, port) {
			out = append(out, in)
		}
	}
	h.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].instanceID < out[j].instanceID })
	if n := len(out); n > 1 {
		k := int(h.next.Add(1) % uint64(n))
		out = append(out[k:], out[:k]...)
	}
	return out
}

func (h *connectorHub) closeConnector(connectorID, reason string) {
	h.mu.Lock()
	var ins []*connectorInstance
	for _, in := range h.instances[connectorID] {
		ins = append(ins, in)
	}
	h.mu.Unlock()
	endAll(ins, reason)
}

// endAll ends instances at once and waits for all: each end may wait out a
// send deadline on a stalled socket.
func endAll(ins []*connectorInstance, reason string) {
	var wg sync.WaitGroup
	for _, in := range ins {
		wg.Go(func() { in.end(reason) })
	}
	wg.Wait()
}

// closeKey ends the instances whose session came from one connector key.
func (h *connectorHub) closeKey(connectorID, keyID, reason string) {
	h.mu.Lock()
	var ins []*connectorInstance
	for _, in := range h.instances[connectorID] {
		if in.keyID == keyID {
			ins = append(ins, in)
		}
	}
	h.mu.Unlock()
	endAll(ins, reason)
}

func (h *connectorHub) addPending(ch chan net.Conn) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	h.mu.Lock()
	h.init()
	h.pending[id] = ch
	h.mu.Unlock()
	return id
}

func (h *connectorHub) takePending(id string) chan net.Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := h.pending[id]
	delete(h.pending, id)
	return ch
}

// abandonPending gives up on a pending open. If the data socket was already
// taken, it is on its way down ch and must be closed, not leaked.
func (h *connectorHub) abandonPending(id string, ch chan net.Conn) {
	if h.takePending(id) != nil {
		return
	}
	go func() {
		if c := <-ch; c != nil {
			_ = c.Close()
		}
	}()
}

func (h *connectorHub) counter(connectorID string) *atomic.Int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.init()
	c := h.active[connectorID]
	if c == nil {
		c = &atomic.Int64{}
		h.active[connectorID] = c
	}
	return c
}

var errPrivateUnavailable = errors.New("connector unavailable")

// openPrivate asks up to two instances to open target and returns the data
// connection. The result string is the log code on failure.
func (s *Server) openPrivate(ctx context.Context, connectorID, target string, port int) (net.Conn, string, error) {
	cands := s.connectors.candidates(connectorID, target, port)
	if len(cands) == 0 {
		return nil, "offline", errPrivateUnavailable
	}
	active := s.connectors.counter(connectorID)
	if active.Add(1) > maxConnsPerConnector {
		active.Add(-1)
		return nil, "rate_limited", errPrivateUnavailable
	}
	result := "offline"
	for _, in := range cands[:min(2, len(cands))] {
		ch := make(chan net.Conn, 1)
		id := s.connectors.addPending(ch)
		if err := in.send(map[string]any{"type": "open", "id": id, "proto": "tcp", "target": target, "port": port}); err != nil {
			s.connectors.abandonPending(id, ch)
			continue
		}
		select {
		case c := <-ch:
			if c == nil {
				result = "dial_failed"
				continue
			}
			in.conns.Store(c, struct{}{})
			return &connectorConn{Conn: c, onClose: func() { active.Add(-1); in.conns.Delete(c) }}, "", nil
		case <-time.After(connectorOpenWait):
			s.connectors.abandonPending(id, ch)
		case <-ctx.Done():
			s.connectors.abandonPending(id, ch)
			active.Add(-1)
			return nil, "offline", ctx.Err()
		case <-in.done:
			s.connectors.abandonPending(id, ch)
		}
	}
	active.Add(-1)
	return nil, result, errPrivateUnavailable
}

type connectorConn struct {
	net.Conn
	once    sync.Once
	onClose func()
}

func (c *connectorConn) Close() error {
	c.once.Do(c.onClose)
	return c.Conn.Close()
}

func (s *Server) tunnelStoreError(w http.ResponseWriter, r *http.Request, id string, err error) {
	s.log().ErrorContext(r.Context(), "connector unreadable", "component", "private", "connector", id, "err", err)
	writeErr(w, http.StatusServiceUnavailable, "private access is unavailable right now; retry shortly")
}

func (s *Server) handleConnectorControl(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// 403 not 401: this route is self-authenticated, and an unauthenticated probe must not look like a key-guarded route.
	keyID, valid := s.validConnectorToken(bearer(r), id)
	if !valid {
		writeErr(w, http.StatusForbidden, "missing or invalid tunnel token")
		return
	}
	if !privateIngressEnabled() {
		writeErr(w, http.StatusServiceUnavailable, "this replica does not run the connector hub")
		return
	}
	st := s.st.Load()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	// A store error is 503, not 410: the connector stops for good on 410.
	c, ok, err := st.ConnectorByID(r.Context(), "", id)
	if err != nil {
		s.tunnelStoreError(w, r, id, err)
		return
	}
	if !ok || c.RevokedAt != 0 {
		writeErr(w, http.StatusGone, "connector revoked")
		return
	}
	if keyID != "" {
		live, err := st.ConnectorKeyLive(r.Context(), id, keyID)
		if err != nil {
			s.tunnelStoreError(w, r, id, err)
			return
		}
		if !live {
			writeErr(w, http.StatusGone, "connector key revoked")
			return
		}
	}
	ctrl, err := shareUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ctrl.SetReadLimit(64 << 10)
	_ = ctrl.SetReadDeadline(time.Now().Add(connectorHelloWait))
	var hello struct {
		Type         string `json:"type"`
		InstanceID   string `json:"instance_id"`
		Capabilities struct {
			Targets []store.ConnectorTarget `json:"targets"`
			Subnets []string                `json:"subnets"`
			DNS     struct {
				Suffixes []string `json:"suffixes"`
			} `json:"dns"`
		} `json:"capabilities"`
	}
	in := &connectorInstance{connectorID: id, keyID: keyID, ctrl: ctrl, done: make(chan struct{})}
	if err := ctrl.ReadJSON(&hello); err != nil || hello.Type != "hello" || len(hello.InstanceID) < 16 || len(hello.InstanceID) > 64 {
		in.end("expected a hello with an instance_id")
		return
	}
	pts := make([]sandbox.PrivateTarget, 0, len(hello.Capabilities.Targets))
	for _, t := range hello.Capabilities.Targets {
		pts = append(pts, sandbox.PrivateTarget{Name: t.Name, Host: t.Host, Port: t.Port})
		if t.Host != "" {
			if err := validConnectorHost(t.Host, platformDomains()); err != nil {
				in.end(err.Error())
				return
			}
		}
	}
	if err := sandbox.ValidPrivateTargets(pts); err != nil {
		in.end(err.Error())
		return
	}
	subnets, suffixes, err := validAnnouncement(hello.Capabilities.Subnets, hello.Capabilities.DNS.Suffixes, platformDomains())
	if err != nil {
		in.end(err.Error())
		return
	}
	in.instanceID, in.targets, in.subnets, in.suffixes = hello.InstanceID, hello.Capabilities.Targets, subnets, suffixes
	old, ok := s.connectors.add(in)
	if !ok {
		in.end("this connector already has the maximum number of instances")
		return
	}
	if old != nil {
		old.end("replaced by a reconnect of the same instance")
	}
	defer func() {
		s.connectors.remove(in)
		in.end("disconnected")
	}()
	now := s.now().Unix()
	nets, names := s.connectors.routes(id, nil, nil)
	if err := st.SetConnectorAnnounced(r.Context(), id, s.connectors.union(id, nil), nets, names, now); err != nil {
		s.log().Warn("connector announcement not saved", "component", "private", "connector", id, "err", err)
	}
	go s.superviseConnector(st, in)
	ctrl.SetPongHandler(func(string) error { return ctrl.SetReadDeadline(time.Now().Add(shareReadWait)) })
	for {
		var m struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		_ = ctrl.SetReadDeadline(time.Now().Add(shareReadWait))
		if err := ctrl.ReadJSON(&m); err != nil {
			return
		}
		if m.Type == "fail" {
			if ch := s.connectors.takePending(m.ID); ch != nil {
				ch <- nil
			}
		}
	}
}

// superviseConnector pings, refreshes last_seen, and ends the instance when the
// connector, or the key its session came from, is revoked on another replica.
func (s *Server) superviseConnector(st *store.Store, in *connectorInstance) {
	t := time.NewTicker(connectorPingEvery)
	defer t.Stop()
	for {
		select {
		case <-in.done:
			return
		case <-t.C:
			in.writeMu.Lock()
			err := in.ctrl.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
			in.writeMu.Unlock()
			// A Cloudflare Worker in front answers the ping itself and never
			// forwards it, so the connector also needs a message it can see.
			if err == nil {
				err = in.send(map[string]any{"type": "keepalive"})
			}
			if err != nil {
				in.end("ping failed")
				return
			}
			c, ok, err := st.ConnectorByID(context.Background(), "", in.connectorID)
			if err == nil && (!ok || c.RevokedAt != 0) {
				in.end("revoked")
				return
			}
			if in.keyID != "" {
				if live, err := st.ConnectorKeyLive(context.Background(), in.connectorID, in.keyID); err == nil && !live {
					in.end("key revoked")
					return
				}
			}
			_ = st.TouchConnector(context.Background(), in.connectorID, s.now().Unix())
		}
	}
}

func (s *Server) handleConnectorConn(w http.ResponseWriter, r *http.Request) {
	ch := s.connectors.takePending(r.PathValue("conn"))
	if ch == nil {
		writeErr(w, http.StatusNotFound, "unknown connection id")
		return
	}
	ws, err := shareUpgrader.Upgrade(w, r, nil)
	if err != nil {
		ch <- nil
		return
	}
	ch <- &wsConn{ws: ws}
}
