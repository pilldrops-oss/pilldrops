package orchestrator

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// Rejections assert == 401, not != 200, so a 503 from an unconfigured server
// cannot pass as a rejection.
func TestOneNodeCannotHeartbeatAsAnother(t *testing.T) {
	s := NewServer("k", "fleet-token")
	s.nodeKeySecret = "control-plane-only"

	beat := func(bearer, id, url string) int {
		body := `{"id":"` + id + `","url":"` + url + `","zone":"par1",` +
			`"capacity":{"cores":8,"mem_total_mb":64000,"mem_avail_mb":64000}}`
		req := httptest.NewRequest(http.MethodPost, "/internal/heartbeat", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, req)
		return w.Code
	}

	nodeA := s.nodeHeartbeatToken("node-a", "par1")
	nodeB := s.nodeHeartbeatToken("node-b", "par1")

	// Positive control: without it the rejections below prove nothing.
	if code := beat(nodeA, "node-a", "http://a.invalid:18080"); code != http.StatusOK {
		t.Fatalf("node-a heartbeating as itself = %d, want 200", code)
	}
	if code := beat(nodeB, "node-b", "http://b.invalid:18080"); code != http.StatusOK {
		t.Fatalf("node-b heartbeating as itself = %d, want 200", code)
	}

	if code := beat(nodeA, "node-b", "http://attacker.invalid:18080"); code != http.StatusUnauthorized {
		t.Errorf("node-a heartbeat AS node-b = %d, want 401 — one node can hijack another's registration", code)
	}
	if code := beat("fleet-token", "node-a", "http://a.invalid:18080"); code != http.StatusUnauthorized {
		t.Errorf("the fleet-wide shared token authenticated a heartbeat = %d, want 401 — deriving bought nothing", code)
	}
	if nodeA == nodeB {
		t.Error("two nodes derive the same heartbeat token")
	}

	// A 401 after writing the record would be the same breach.
	if n, ok := s.reg.Node("node-b"); !ok || n.URL != "http://b.invalid:18080" {
		t.Errorf("node-b url = %+v, want it unchanged at http://b.invalid:18080", n)
	}
}

// Even a caller holding node-b's heartbeat token cannot re-point a live node-b.
func TestUrlTakeoverIsRefusedForALiveNode(t *testing.T) {
	s := fleetServer("k", "fleet-token")
	now := time.Now()
	s.now = func() time.Time { return now }

	beat := func(url string) (int, string) {
		body := `{"id":"node-b","url":"` + url + `","zone":"par1",` +
			`"capacity":{"cores":8,"mem_total_mb":64000,"mem_avail_mb":64000}}`
		req := httptest.NewRequest(http.MethodPost, "/internal/heartbeat", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+nodeBearer("node-b", "par1"))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}

	const real, attacker = "http://b.invalid:18080", "http://attacker.invalid:18080"

	if code, body := beat(real); code != http.StatusOK {
		t.Fatalf("node-b registering = %d (%s), want 200", code, body)
	}
	if code, body := beat(attacker); code != http.StatusConflict {
		t.Errorf("url takeover of a live node = %d (%s), want 409", code, body)
	}
	if n, _ := s.reg.Node("node-b"); n.URL != real {
		t.Fatalf("node-b url = %q after a refused takeover, want %q — the registry was written anyway", n.URL, real)
	}

	// A refused attempt must not refresh LastSeen, or an attacker could hold
	// the takeover window shut forever.
	before, _ := s.reg.Node("node-b")
	now = now.Add(3 * time.Second)
	_, _ = beat(attacker)
	if after, _ := s.reg.Node("node-b"); !after.LastSeen.Equal(before.LastSeen) {
		t.Errorf("a REFUSED heartbeat advanced LastSeen %v → %v", before.LastSeen, after.LastSeen)
	}

	for i := 0; i < 10; i++ {
		// Reset the real-time rate limiter so a 429 cannot pass as a takeover refusal.
		s.hbIPLimit = newIPLimiter(heartbeatIPRPS, heartbeatIPBurst)
		s.hbNodeLimit = newIPLimiter(heartbeatNodeRPS, heartbeatNodeBurst)
		now = now.Add(5 * time.Second)
		if code, body := beat(real); code != http.StatusOK {
			t.Fatalf("victim heartbeat %d = %d (%s), want 200", i, code, body)
		}
		if code, _ := beat(attacker); code != http.StatusConflict {
			t.Fatalf("takeover attempt %d = %d, want 409 while the real node is alive", i, code)
		}
	}
	if n, _ := s.reg.Node("node-b"); n.URL != real {
		t.Errorf("node-b url = %q after 10 rounds, want %q", n.URL, real)
	}
}

// A node whose IP genuinely changes must recover with no operator action.
func TestSameNodeChangingItsIPRecoversOnItsOwn(t *testing.T) {
	s := fleetServer("k", "fleet-token")
	now := time.Now()
	s.now = func() time.Time { return now }

	beat := func(url string) int {
		body := `{"id":"node-b","url":"` + url + `","zone":"par1",` +
			`"capacity":{"cores":8,"mem_total_mb":64000,"mem_avail_mb":64000}}`
		req := httptest.NewRequest(http.MethodPost, "/internal/heartbeat", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+nodeBearer("node-b", "par1"))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, req)
		return w.Code
	}

	const oldIP, newIP = "http://10.0.0.5:18080", "http://10.0.0.9:18080"
	if code := beat(oldIP); code != http.StatusOK {
		t.Fatalf("initial registration = %d, want 200", code)
	}

	now = now.Add(5 * time.Second)
	if code := beat(newIP); code != http.StatusConflict {
		t.Fatalf("first heartbeat from the new IP = %d, want 409 (record still live)", code)
	}
	// Accepted once the previous registration has aged past NodeTTL.
	now = now.Add(cluster.NodeTTL + time.Second)
	if code := beat(newIP); code != http.StatusOK {
		t.Fatalf("heartbeat from the new IP after NodeTTL = %d, want 200 — a node that "+
			"genuinely moved can never come back", code)
	}
	n, ok := s.reg.Node("node-b")
	if !ok || n.URL != newIP {
		t.Fatalf("node-b url = %+v, want %q", n, newIP)
	}
	if n.URLChanges != 1 {
		t.Errorf("URLChanges = %d, want 1 — the fleet view lost its churn signal", n.URLChanges)
	}
}

func TestHeartbeatIsRefusedWithoutNodeKeySecret(t *testing.T) {
	s := NewServer("k", "fleet-token") // nodeKeySecret unset

	body := `{"id":"node-a","url":"http://a.invalid:18080","zone":"par1",` +
		`"capacity":{"cores":8,"mem_total_mb":64000,"mem_avail_mb":64000}}`
	post := func(bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/internal/heartbeat", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, req)
		return w
	}

	w := post("fleet-token")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("heartbeat with the shared token and no NODE_KEY_SECRET = %d, want 503 "+
			"(body %q)", w.Code, w.Body.String())
	}
	// The refusal has to name the setting, or an operator reads it as an outage.
	if !strings.Contains(w.Body.String(), "NODE_KEY_SECRET") {
		t.Errorf("refusal does not name NODE_KEY_SECRET: %q", w.Body.String())
	}
	// The configuration is refused, not just this bearer.
	legacyDerived := (&Server{nodeKeySecret: "fleet-token"}).nodeHeartbeatToken("node-a", "par1")
	if code := post(legacyDerived).Code; code != http.StatusServiceUnavailable {
		t.Errorf("heartbeat with the OLD fallback-derived token = %d, want 503 — the "+
			"ORCHESTRATOR_TOKEN-derived credential still opens the door", code)
	}
}

// A hundred nodes behind one address (a NAT gateway, or this tier behind a
// proxy) all beat in the same interval and all get in; the old per-IP limit
// admitted ~25 and the fleet flapped. One node beating far too fast is still
// limited on its own.
func TestManyNodesBehindOneAddressAllHeartbeat(t *testing.T) {
	s := NewServer("api-key", "fleet-token")
	s.nodeKeySecret = "control-plane-only"
	h := s.Routes()
	ts := time.Now().UnixMilli()
	beat := func(id string) int {
		ts++ // strictly increasing, so replay protection never answers first
		body := `{"id":"` + id + `","url":"http://` + id + `.invalid:18080","zone":"nat","ts":` +
			strconv.FormatInt(ts, 10) + `}`
		req := httptest.NewRequest(http.MethodPost, "/internal/heartbeat", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+s.nodeHeartbeatToken(id, "nat"))
		req.RemoteAddr = "203.0.113.7:4000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w.Code
	}
	for i := 0; i < 100; i++ {
		if code := beat(fmt.Sprintf("n-%03d", i)); code != http.StatusOK {
			t.Fatalf("node %d of 100 behind one address = %d, want 200", i, code)
		}
	}
	limited := false
	for i := 0; i < 20; i++ {
		if beat("n-000") == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatal("one node beating 20 times at once was never limited")
	}
}
