package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

const maxPrivateConnsPerSandbox = 100

// privateOpensPerSecond is a var so tests can lower it.
var privateOpensPerSecond = 50

// privateConn is one open private connection a sandbox holds through a connector.
type privateConn struct {
	sandboxID, connectorID, target string
	close                          func()
}

// privateConnSet tracks open private connections so a revoke or destroy can cut
// them at once, and holds each sandbox's connection limits.
type privateConnSet struct {
	mu         sync.Mutex
	conns      map[*privateConn]struct{}
	perSandbox map[string]int
	limits     map[string]*rate.Limiter
	refused    map[string]*privateRefusals
	swept      time.Time
}

// privateRefusals counts a sandbox's rate-limited opens since its last warning.
type privateRefusals struct {
	n      int
	warned time.Time
}

// add registers c, or reports false when its sandbox already holds the maximum.
func (p *privateConnSet) add(c *privateConn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conns == nil {
		p.conns, p.perSandbox = map[*privateConn]struct{}{}, map[string]int{}
	}
	if p.perSandbox[c.sandboxID] >= maxPrivateConnsPerSandbox {
		return false
	}
	p.conns[c] = struct{}{}
	p.perSandbox[c.sandboxID]++
	return true
}

func (p *privateConnSet) remove(c *privateConn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.conns[c]; !ok {
		return
	}
	delete(p.conns, c)
	if p.perSandbox[c.sandboxID]--; p.perSandbox[c.sandboxID] > 0 {
		return
	}
	delete(p.perSandbox, c.sandboxID)
}

// allow takes one of the sandbox's new-connection tokens.
func (p *privateConnSet) allow(sandboxID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.limits == nil {
		p.limits = map[string]*rate.Limiter{}
	}
	// A refilled limiter with no open connection carries no history, so it is
	// dropped; sweeping at most once a second keeps allow cheap.
	if now := time.Now(); now.Sub(p.swept) >= time.Second {
		p.swept = now
		for id, l := range p.limits {
			if p.perSandbox[id] == 0 && l.Tokens() >= float64(l.Burst()) {
				delete(p.limits, id)
			}
		}
		for id, f := range p.refused {
			if now.Sub(f.warned) >= time.Minute {
				delete(p.refused, id)
			}
		}
	}
	l := p.limits[sandboxID]
	if l == nil {
		l = rate.NewLimiter(rate.Limit(privateOpensPerSecond), privateOpensPerSecond)
		p.limits[sandboxID] = l
	}
	return l.Allow()
}

// refuse counts one rate-limited open and returns how many to warn about:
// the count since the sandbox's last warning, or 0 within a minute of it.
func (p *privateConnSet) refuse(sandboxID string, now time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.refused == nil {
		p.refused = map[string]*privateRefusals{}
	}
	f := p.refused[sandboxID]
	if f == nil {
		f = &privateRefusals{}
		p.refused[sandboxID] = f
	}
	f.n++
	if !f.warned.IsZero() && now.Sub(f.warned) < time.Minute {
		return 0
	}
	n := f.n
	f.n, f.warned = 0, now
	return n
}

// closeFor closes the sandbox's connections to target; an empty target means all of them.
func (p *privateConnSet) closeFor(sandboxID, target string) {
	p.mu.Lock()
	var hit []*privateConn
	for c := range p.conns {
		if c.sandboxID == sandboxID && (target == "" || c.target == target) {
			hit = append(hit, c)
		}
	}
	p.mu.Unlock()
	for _, c := range hit {
		c.close()
	}
}

// closeUngranted closes the sandbox's connections no grant in gs still covers.
func (p *privateConnSet) closeUngranted(sandboxID string, gs []store.Grant) {
	keep := map[[2]string]bool{}
	for _, g := range gs {
		keep[[2]string{g.ConnectorID, g.Value}] = true
	}
	p.mu.Lock()
	var hit []*privateConn
	for c := range p.conns {
		if c.sandboxID == sandboxID && !keep[[2]string{c.connectorID, c.target}] {
			hit = append(hit, c)
		}
	}
	p.mu.Unlock()
	for _, c := range hit {
		c.close()
	}
}

// privateSlot is a sandbox's place in privateConnSet, taken before the upstream
// exists so a full sandbox gets 429 rather than a dial. A cut before the
// upstream arrives closes it as soon as it does.
type privateSlot struct {
	mu  sync.Mutex
	up  net.Conn
	cut bool
}

func (s *privateSlot) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cut = true
	if s.up != nil {
		_ = s.up.Close()
	}
}

// set hands the slot its upstream, and reports false when it was cut first.
func (s *privateSlot) set(up net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.up = up
	return !s.cut
}

type byteCounter struct {
	net.Conn
	read, written *atomic.Int64
}

func (b byteCounter) Read(p []byte) (int, error) {
	n, err := b.Conn.Read(p)
	b.read.Add(int64(n))
	return n, err
}

func (b byteCounter) Write(p []byte) (int, error) {
	n, err := b.Conn.Write(p)
	b.written.Add(int64(n))
	return n, err
}

func newPrivateConnID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "pc_" + hex.EncodeToString(b)
}

// handlePrivateDial carries one sandbox connection to a granted target. The
// node has already routed it by name; this re-checks everything, because a
// node key is not proof the sandbox may reach the target.
func (s *Server) handlePrivateDial(w http.ResponseWriter, r *http.Request) {
	nodeID, st, ok := s.privateNode(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	sbID, target := q.Get("sandbox"), q.Get("target")
	port, _ := strconv.Atoi(q.Get("port"))
	// Before any database read, and logged only in aggregate, so a connect
	// loop costs the control plane no queries and the log no rows.
	if !s.privateConns.allow(sbID) {
		if n := s.privateConns.refuse(sbID, s.now()); n > 0 {
			s.log().Warn("private opens rate limited", "component", "private", "sandbox", sbID, "node", nodeID, "refused", n)
		}
		writeErr(w, http.StatusTooManyRequests, "too many new private connections from this sandbox")
		return
	}
	rec, found, err := st.SandboxByID(r.Context(), sbID)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if !found || rec.NodeID != nodeID {
		writeErr(w, http.StatusForbidden, "this node does not run that sandbox")
		return
	}
	row := store.PrivateConn{ID: newPrivateConnID(), OrgID: rec.OrgID, SandboxID: sbID, Target: target, NodeID: nodeID, StartedAt: s.now().Unix()}
	logRow := func(result string) {
		row.Result, row.EndedAt = result, s.now().Unix()
		if err := st.InsertPrivateConn(context.WithoutCancel(r.Context()), row); err != nil {
			s.log().Warn("private connection not logged", "component", "private", "sandbox", sbID, "err", err)
		}
	}
	gs, err := st.EffectiveGrants(r.Context(), rec.OrgID, sbID, recordTags(rec))
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	var g store.Grant
	granted := false
	for _, x := range gs {
		if x.Kind == store.GrantKindTarget && x.Value == target {
			g, granted = x, true
			break
		}
	}
	if !granted {
		logRow("denied")
		writeErr(w, http.StatusForbidden, "not granted")
		return
	}
	row.ConnectorID = g.ConnectorID
	c, found, err := st.ConnectorByID(r.Context(), "", g.ConnectorID)
	if err != nil {
		logRow("offline")
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	portOK := false
	if found && c.RevokedAt == 0 {
		for _, t := range s.connectors.union(c.ID, c.Targets) {
			portOK = portOK || (t.Name == target && t.Port == port)
		}
	}
	if !portOK {
		logRow("denied")
		writeErr(w, http.StatusForbidden, "not granted")
		return
	}
	slot := &privateSlot{}
	pc := &privateConn{sandboxID: sbID, connectorID: g.ConnectorID, target: target, close: slot.close}
	if !s.privateConns.add(pc) {
		logRow("rate_limited")
		writeErr(w, http.StatusTooManyRequests, "this sandbox has too many open private connections")
		return
	}
	defer s.privateConns.remove(pc)
	up, result, err := s.openPrivate(r.Context(), g.ConnectorID, target, port)
	if err != nil {
		logRow(result)
		writeErr(w, http.StatusBadGateway, "connector "+c.Name+": "+result)
		return
	}
	defer func() { _ = up.Close() }()
	if !slot.set(up) {
		logRow("denied")
		writeErr(w, http.StatusForbidden, "not granted")
		return
	}
	ws, err := shareUpgrader.Upgrade(w, r, nil)
	if err != nil {
		logRow("offline")
		return
	}
	var in, out atomic.Int64
	splicePrivate(byteCounter{Conn: &wsConn{ws: ws}, read: &in, written: &out}, up)
	row.BytesIn, row.BytesOut = in.Load(), out.Load()
	logRow("ok")
}

// splicePrivate copies both ways until either side ends, then closes both.
func splicePrivate(a, b net.Conn) {
	done := make(chan struct{}, 2)
	half := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		_ = dst.Close()
		done <- struct{}{}
	}
	go half(a, b)
	go half(b, a)
	<-done
	<-done
}

// privateNode authenticates a node calling the hub's internal routes.
func (s *Server) privateNode(w http.ResponseWriter, r *http.Request) (string, *store.Store, bool) {
	if !privateIngressEnabled() {
		writeErr(w, http.StatusServiceUnavailable, "this replica does not run the connector hub")
		return "", nil, false
	}
	nodeID, ok := s.authNode(r)
	if !ok && nodeID != "" {
		writeErr(w, http.StatusForbidden, "node removed: this id was revoked by an operator")
		return "", nil, false
	}
	if !ok {
		writeErr(w, http.StatusUnauthorized, "node key required")
		return "", nil, false
	}
	if nodeID == "" {
		writeErr(w, http.StatusForbidden, "only a node may open private connections")
		return "", nil, false
	}
	st := s.st.Load()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable, "store unavailable")
		return "", nil, false
	}
	return nodeID, st, true
}
