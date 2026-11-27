package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

const (
	maxDNSAnswers           = 32
	maxDNSReplyBytes        = 4 << 10
	maxDNSLearnedPerSandbox = 1024
	dnsMinTTL, dnsMaxTTL    = 30 * time.Second, 300 * time.Second
)

// Vars so tests can lower them.
var (
	privateDNSWait      = 3 * time.Second
	privateDNSPerSecond = 50
)

// clampTTL is how long an answered address stays dialable: the node's set
// keeps it exactly as long.
func clampTTL(ttl uint32) time.Duration {
	return max(dnsMinTTL, min(time.Duration(ttl)*time.Second, dnsMaxTTL))
}

// dnsLearned is one address a split-DNS answer let a sandbox dial.
type dnsLearned struct {
	connectorID, instanceID, suffix, name string
	added, expires                        time.Time
}

// privateDNS holds relayed lookups in flight and each sandbox's live answers,
// which are what let a sandbox dial an address it learned by name.
type privateDNS struct {
	mu      sync.Mutex
	pending map[string]chan []byte
	learned map[string]map[netip.Addr]dnsLearned
	limits  map[string]*rate.Limiter
	swept   time.Time
	// Counted, not logged per lookup.
	refused atomic.Int64
	warned  time.Time
}

// allow takes one lookup from key's budget; key is node and sandbox, so one
// node's key cannot spend another node's sandbox's budget.
func (d *privateDNS) allow(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.limits == nil {
		d.limits = map[string]*rate.Limiter{}
	}
	if now := time.Now(); now.Sub(d.swept) >= time.Second {
		d.swept = now
		for id, l := range d.limits {
			if l.Tokens() >= float64(l.Burst()) {
				delete(d.limits, id)
			}
		}
	}
	l := d.limits[key]
	if l == nil {
		l = rate.NewLimiter(rate.Limit(privateDNSPerSecond), privateDNSPerSecond)
		d.limits[key] = l
	}
	return l.Allow()
}

// refuse counts one limited lookup and returns how many to warn about: the
// count since the last warning, or 0 within a minute of it.
func (d *privateDNS) refuse(now time.Time) int {
	n := d.refused.Add(1)
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.warned.IsZero() && now.Sub(d.warned) < time.Minute {
		return 0
	}
	d.warned = now
	d.refused.Store(0)
	return int(n)
}

func (d *privateDNS) addPending() (string, chan []byte) {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	id, ch := hex.EncodeToString(b), make(chan []byte, 1)
	d.mu.Lock()
	if d.pending == nil {
		d.pending = map[string]chan []byte{}
	}
	d.pending[id] = ch
	d.mu.Unlock()
	return id, ch
}

// deliver hands an instance's dns_answer to the lookup waiting on id, if any.
func (d *privateDNS) deliver(id string, raw []byte) {
	d.mu.Lock()
	ch := d.pending[id]
	delete(d.pending, id)
	d.mu.Unlock()
	if ch != nil {
		ch <- raw
	}
}

func (d *privateDNS) drop(id string) {
	d.mu.Lock()
	delete(d.pending, id)
	d.mu.Unlock()
}

// learn remembers an answered address, sweeping the sandbox's expired entries
// and keeping at most the limit by dropping the oldest.
func (d *privateDNS) learn(sandboxID string, a netip.Addr, e dnsLearned) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.learned == nil {
		d.learned = map[string]map[netip.Addr]dnsLearned{}
	}
	m := d.learned[sandboxID]
	if m == nil {
		m = map[netip.Addr]dnsLearned{}
		d.learned[sandboxID] = m
	}
	now := time.Now()
	// ponytail: O(n) sweep per learn; a heap if 1,024 ever grows.
	for k, v := range m {
		if !v.expires.After(now) {
			delete(m, k)
		}
	}
	if _, ok := m[a]; !ok && len(m) >= maxDNSLearnedPerSandbox {
		var oldest netip.Addr
		for k, v := range m {
			if !oldest.IsValid() || v.added.Before(m[oldest].added) {
				oldest = k
			}
		}
		delete(m, oldest)
	}
	m[a] = e
}

func (d *privateDNS) lookup(sandboxID string, a netip.Addr, now time.Time) (dnsLearned, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.learned[sandboxID][a]
	return e, ok && e.expires.After(now)
}

func (d *privateDNS) forget(sandboxID string) {
	d.mu.Lock()
	delete(d.learned, sandboxID)
	d.mu.Unlock()
}

var errBadDNSReply = errors.New("the connector's answer was too large or malformed")

// relayDNS asks one instance to resolve name and waits for its answer.
func (s *Server) relayDNS(ctx context.Context, in *connectorInstance, name, qtype string) (sandbox.PrivateDNSReply, error) {
	id, ch := s.privateDNS.addPending()
	defer s.privateDNS.drop(id)
	if err := in.send(map[string]any{"type": "dns", "id": id, "name": name, "qtype": qtype}); err != nil {
		return sandbox.PrivateDNSReply{}, err
	}
	select {
	case raw := <-ch:
		var a sandbox.PrivateDNSReply
		if len(raw) > maxDNSReplyBytes || json.Unmarshal(raw, &a) != nil || len(a.Answers) > maxDNSAnswers || a.RCode < 0 || a.RCode > 15 {
			return sandbox.PrivateDNSReply{}, errBadDNSReply
		}
		return a, nil
	case <-time.After(privateDNSWait):
		return sandbox.PrivateDNSReply{}, errPrivateUnavailable
	case <-ctx.Done():
		return sandbox.PrivateDNSReply{}, ctx.Err()
	case <-in.done:
		return sandbox.PrivateDNSReply{}, errPrivateUnavailable
	}
}

// keepAnswers drops A answers in the platform's own ranges and CNAMEs that are
// not valid hosts, caps every TTL at
// what the node's set keeps, and remembers each A address before the reply
// leaves, so the sandbox's first connect to it is already authorised.
func (s *Server) keepAnswers(sandboxID, connectorID, instanceID, suffix, name string, a sandbox.PrivateDNSReply) sandbox.PrivateDNSReply {
	out := sandbox.PrivateDNSReply{RCode: a.RCode, Answers: []sandbox.PrivateDNSAnswer{}}
	now := s.now()
	for _, ans := range a.Answers {
		ans.TTL = min(ans.TTL, uint32(dnsMaxTTL/time.Second))
		switch ans.Type {
		case "CNAME":
			if validConnectorHost(ans.Value, platformDomains()) != nil {
				continue
			}
			out.Answers = append(out.Answers, ans)
		case "A":
			ip, err := netip.ParseAddr(ans.Value)
			if err != nil || !ip.Is4() || len(sandbox.Shadowed(netip.PrefixFrom(ip, 32))) > 0 {
				continue
			}
			s.privateDNS.learn(sandboxID, ip, dnsLearned{connectorID: connectorID, instanceID: instanceID,
				suffix: suffix, name: name, added: now, expires: now.Add(clampTTL(ans.TTL))})
			out.Answers = append(out.Answers, ans)
		}
	}
	return out
}

// validDNSQueryName reports whether name is at most 253 bytes of labels of 1 to
// 63 letters, digits, hyphens and underscores.
func validDNSQueryName(name string) bool {
	if len(name) > 253 {
		return false
	}
	for label := range strings.SplitSeq(name, ".") {
		if len(label) < 1 || len(label) > 63 || strings.Trim(label, "abcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
			return false
		}
	}
	return true
}

// handlePrivateDNS relays one split-DNS lookup from a sandbox's node to the
// connector its longest granted suffix names.
func (s *Server) handlePrivateDNS(w http.ResponseWriter, r *http.Request) {
	nodeID, st, ok := s.privateNode(w, r)
	if !ok {
		return
	}
	var q struct {
		Sandbox string `json:"sandbox"`
		Name    string `json:"name"`
		QType   string `json:"qtype"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&q); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if !s.privateDNS.allow(nodeID + "/" + q.Sandbox) {
		if n := s.privateDNS.refuse(s.now()); n > 0 {
			s.log().Warn("private dns lookups rate limited", "component", "private", "node", nodeID, "refused", n)
		}
		writeErr(w, http.StatusTooManyRequests, "too many private lookups from this sandbox")
		return
	}
	if q.QType != "A" && q.QType != "CNAME" {
		writeErr(w, http.StatusBadRequest, `"qtype" must be A or CNAME`)
		return
	}
	name := strings.TrimSuffix(strings.ToLower(q.Name), ".")
	if !validDNSQueryName(name) {
		writeErr(w, http.StatusBadRequest, `"name" is not a valid DNS name`)
		return
	}
	rec, found, err := st.SandboxByID(r.Context(), q.Sandbox)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if !found || rec.NodeID != nodeID {
		writeErr(w, http.StatusForbidden, "this node does not run that sandbox")
		return
	}
	gs, err := st.EffectiveGrants(r.Context(), rec.OrgID, rec.ID, recordTags(rec))
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	bySuffix := map[string]string{}
	var suffixes []string
	for _, g := range gs {
		if g.Kind == store.GrantKindDNS {
			bySuffix[g.Value] = g.ConnectorID
			suffixes = append(suffixes, g.Value)
		}
	}
	suffix, ok := sandbox.LongestSuffix(name, suffixes)
	if !ok {
		writeErr(w, http.StatusForbidden, "not granted")
		return
	}
	connectorID := bySuffix[suffix]
	cands := s.connectors.suffixCandidates(connectorID, suffix)
	if len(cands) == 0 {
		writeErr(w, http.StatusBadGateway, "connector offline")
		return
	}
	in := cands[0]
	reply, err := s.relayDNS(r.Context(), in, name, q.QType)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "connector: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.keepAnswers(rec.ID, connectorID, in.instanceID, suffix, name, reply))
}
