package orchestrator

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// createFailureFixture wires a server to one fake node.
func createFailureFixture(t *testing.T, h http.HandlerFunc) (*Server, string) {
	t.Helper()
	node := httptest.NewServer(h)
	t.Cleanup(node.Close)

	s := NewServer("api-key", "node-token")
	upsert(s.reg, "node-a", node.URL, "node-key", "", nil, "",
		cluster.Capacity{Cores: 8, MemTotalMB: fleetNodeMemMB, MemAvailMB: fleetNodeMemMB}, s.now())
	return s, node.URL
}

// assertNoLeakedHold probes with the largest placeable request. Healthy() does
// not subtract reservations, so only PickAndReserve can observe a leaked hold,
// and any smaller probe fits in both worlds.
func assertNoLeakedHold(t *testing.T, s *Server) {
	t.Helper()
	const probe = fleetNodeMemMB - cluster.FitMarginMB
	got := s.reg.PickAndReserve(cluster.Request{VCPU: 1, MemMB: probe}, s.now(), cluster.NodeTTL)
	if got == nil {
		t.Errorf("the largest placeable request (%d MB) no longer fits: the capacity hold "+
			"from the failed create was not released. Under a burst of failing creates the "+
			"fleet under-reports capacity and refuses work it could run (self-heals after "+
			"ReservationTTL=%s)", probe, cluster.ReservationTTL)
		return
	}
	s.reg.Release(got.ID, probe) // leave the fixture clean
}

// fleetNodeMemMB is what the fake node advertises.
const fleetNodeMemMB = 8192

func doCreate(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer api-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)
	return w
}

func TestNodeRefusesCreate(t *testing.T) {
	s, _ := createFailureFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInsufficientStorage)
		_, _ = w.Write([]byte(`{"error":"no room"}`))
	})
	w := doCreate(t, s, `{"template":"base","vcpu":1,"mem_mb":512}`)

	if w.Code == http.StatusOK || w.Code == http.StatusCreated {
		t.Fatalf("status = %d, want a failure — the node refused", w.Code)
	}
	assertNoLeakedHold(t, s)
	if _, ok := s.reg.NodeFor("sb-anything"); ok {
		t.Error("a placement was recorded for a create the node refused")
	}
}

func TestNodeDiesMidCreate(t *testing.T) {
	s, _ := createFailureFixture(t, func(w http.ResponseWriter, r *http.Request) {
		// Hijack and close so the client sees no response at all.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Skip("no hijacker")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	})
	w := doCreate(t, s, `{"template":"base","vcpu":1,"mem_mb":512}`)

	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 for an unanswered create", w.Code)
	}
	assertNoLeakedHold(t, s)
}

// The node creates the sandbox but the answer is lost: the caller gets a 502
// and the reconciler must adopt the real sandbox.
func TestOrphanFromALostCreateResponseIsAdopted(t *testing.T) {
	var created atomic.Bool
	s, _ := createFailureFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			created.Store(true)
			hj, ok := w.(http.Hijacker)
			if !ok {
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			_ = conn.Close()
		case r.Method == http.MethodGet && r.URL.Path == "/v1/admin/sandboxes":
			if created.Load() {
				_, _ = w.Write([]byte(`{"sandboxes":[{"id":"sb-orphan"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"sandboxes":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	w := doCreate(t, s, `{"template":"base","vcpu":1,"mem_mb":512}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 — the orchestrator could not confirm the create", w.Code)
	}
	if !created.Load() {
		t.Fatal("fixture never reached the node; this test is not exercising the case it describes")
	}
	if _, ok := s.reg.NodeFor("sb-orphan"); ok {
		t.Fatal("precondition: the orphan should not be placed yet")
	}

	if n := s.RebuildPlacements(s.now()); n != 1 {
		t.Fatalf("reconciler adopted %d sandboxes, want 1", n)
	}
	node, ok := s.reg.NodeFor("sb-orphan")
	if !ok || node.ID != "node-a" {
		t.Errorf("after reconcile NodeFor(sb-orphan) = %+v ok=%v, want node-a — "+
			"a sandbox the customer is running would stay unreachable through the control plane", node, ok)
	}
}

// The reconciler is additive: a node that misses one pass must not have its
// sandboxes forgotten.
func TestReconcilerDoesNotOrphanOnATransientNodeOutage(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	s, _ := createFailureFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			hj, _ := w.(http.Hijacker)
			if hj != nil {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
				}
			}
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/admin/sandboxes" {
			_, _ = w.Write([]byte(`{"sandboxes":[{"id":"sb-live"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	if n := s.RebuildPlacements(s.now()); n != 1 {
		t.Fatalf("first pass adopted %d, want 1", n)
	}

	up.Store(false)
	s.RebuildPlacements(s.now())

	if _, ok := s.reg.NodeFor("sb-live"); !ok {
		t.Error("a placement was dropped because the node missed ONE reconcile pass — " +
			"a brief outage would make live sandboxes unreachable for their owner")
	}

	up.Store(true)
	if n := s.RebuildPlacements(s.now()); n != 1 {
		t.Errorf("recovery pass adopted %d, want 1", n)
	}
}

func TestReconcilerSkipsUnhealthyNodes(t *testing.T) {
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadURL := "http://" + dead.Addr().String()
	_ = dead.Close() // nothing is listening now

	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/admin/sandboxes" {
			_, _ = w.Write([]byte(`{"sandboxes":[{"id":"sb-ok"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer live.Close()

	s := NewServer("api-key", "node-token")
	now := s.now()
	upsert(s.reg, "node-dead", deadURL, "k", "", nil, "", cluster.Capacity{MemAvailMB: 1024}, now)
	upsert(s.reg, "node-live", live.URL, "k", "", nil, "", cluster.Capacity{MemAvailMB: 1024}, now)

	done := make(chan int, 1)
	go func() { done <- s.RebuildPlacements(now) }()
	select {
	case n := <-done:
		if n != 1 {
			t.Errorf("adopted %d, want 1 (only the live node has a sandbox)", n)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("RebuildPlacements did not finish: one unreachable node stalled the whole pass")
	}
	if _, ok := s.reg.NodeFor("sb-ok"); !ok {
		t.Error("the healthy node's sandbox was not adopted because another node was down")
	}
}

// A node that refuses as busy (its heartbeat has not caught up) must not fail
// the create while another node has room; one refusing for any other reason must.
func TestBusyNodeCreateGoesElsewhere(t *testing.T) {
	var busyHits, okHits atomic.Int32
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		busyHits.Add(1)
		w.Header().Set("X-Node-Busy", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"sandbox capacity reached"}`))
	}))
	t.Cleanup(busy.Close)
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okHits.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"sb-elsewhere"}`))
	}))
	t.Cleanup(ok.Close)

	s := NewServer("api-key", "node-token")
	// Placement bin-packs, so the tighter busy node is tried first.
	upsert(s.reg, "node-busy", busy.URL, "node-key", "", nil, "",
		cluster.Capacity{Cores: 8, MemTotalMB: fleetNodeMemMB, MemAvailMB: fleetNodeMemMB / 2}, s.now())
	upsert(s.reg, "node-ok", ok.URL, "node-key", "", nil, "",
		cluster.Capacity{Cores: 8, MemTotalMB: fleetNodeMemMB, MemAvailMB: fleetNodeMemMB}, s.now())

	w := doCreate(t, s, `{"template":"base","vcpu":1,"mem_mb":512}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s), want 201 from the node with room", w.Code, w.Body.String())
	}
	if busyHits.Load() != 1 || okHits.Load() != 1 {
		t.Fatalf("busy node hit %d, ok node hit %d; want 1 and 1", busyHits.Load(), okHits.Load())
	}
	if n, _ := s.reg.NodeFor("sb-elsewhere"); n == nil || n.ID != "node-ok" {
		t.Errorf("placed on %v, want node-ok", n)
	}
}
