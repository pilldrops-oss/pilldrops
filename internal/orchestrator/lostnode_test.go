package orchestrator

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// A reaped node's sandboxes stop reading as running and stop holding their
// org's concurrency; asking for one says it is gone (410), and destroying it
// clears the row and keeps a history record.
func TestReapedNodeSandboxesAreLost(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "lost.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := NewServer("k", "t")
	s.SetStore(st)

	const org = "u:lost"
	for id, node := range map[string]string{"sb-dead": "node-dead", "sb-alive": "node-alive"} {
		if err := st.SaveSandbox(t.Context(), store.SandboxRecord{
			ID: id, OrgID: org, NodeID: node, Template: "base", State: "running",
			CreatedAt: 1, UpdatedAt: 1, ExpiresAt: 9e9,
		}); err != nil {
			t.Fatal(err)
		}
	}
	s.markLost([]string{"node-dead"})

	if rec, _, _ := st.SandboxByID(t.Context(), "sb-dead"); rec.State != store.StateLost {
		t.Fatalf("dead node's sandbox state = %q, want lost", rec.State)
	}
	if rec, _, _ := st.SandboxByID(t.Context(), "sb-alive"); rec.State != "running" {
		t.Fatalf("a live node's sandbox was touched: state %q", rec.State)
	}
	// Cap 2 with two rows: only the live one may count.
	if ok, err := st.ReserveSlot(t.Context(), org, "slot-1", 2); err != nil || !ok {
		t.Fatalf("ReserveSlot = %v, %v: a lost sandbox still holds its org's concurrency", ok, err)
	}

	do := func(method, org string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/v1/sandboxes/sb-dead", nil)
		r.SetPathValue("id", "sb-dead")
		r.Header.Set(orgOverride, org)
		r = r.WithContext(withOperator(r.Context()))
		w := httptest.NewRecorder()
		if method == http.MethodGet {
			s.handleSandboxGet(w, r)
		} else {
			s.handleDestroy(w, r)
		}
		return w
	}
	if w := do(http.MethodGet, "u:someone-else"); w.Code != http.StatusNotFound {
		t.Fatalf("another org sees a lost sandbox: %d", w.Code)
	}
	if w := do(http.MethodGet, org); w.Code != http.StatusGone {
		t.Fatalf("GET lost sandbox = %d %s, want 410", w.Code, w.Body)
	}
	if w := do(http.MethodDelete, org); w.Code != http.StatusOK {
		t.Fatalf("DELETE lost sandbox = %d %s, want 200", w.Code, w.Body)
	}
	if _, ok, _ := st.SandboxByID(t.Context(), "sb-dead"); ok {
		t.Fatal("the lost row survived its DELETE")
	}
	if h, ok, _ := st.GetHistory(t.Context(), "sb-dead"); !ok || h.ExitState != store.StateLost {
		t.Fatalf("history = %+v ok=%v, want exit_state lost", h, ok)
	}
}

// A node that stopped heartbeating is refused at once rather than dialled:
// dialling a partitioned node held each request for minutes.
func TestForwardRefusesANodeThatWentQuiet(t *testing.T) {
	s := NewServer("k", "t")
	quiet := &cluster.Node{ID: "n-quiet", URL: "http://10.255.255.1:1", LastSeen: s.now().Add(-time.Minute)}
	start := time.Now()
	_, err := s.forward(t.Context(), quiet, http.MethodGet, "/v1/sandboxes/x", nil, "")
	if !errors.Is(err, errNodeUnreachable) {
		t.Fatalf("err = %v, want errNodeUnreachable", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("refusing a quiet node took %v; it must not dial", d)
	}
}

// A node back from a partition still runs its sandboxes, so the reap's lost
// mark gives way to the state the node reports.
func TestReviveLostRestoresTheReportedState(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "revive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, id := range []string{"sb-a", "sb-b", "sb-gone"} {
		_ = st.SaveSandbox(t.Context(), store.SandboxRecord{ID: id, OrgID: "o", NodeID: "n1", Template: "base",
			State: "running", CreatedAt: 1, UpdatedAt: 1, ExpiresAt: 9e9})
	}
	if _, err := st.MarkNodeSandboxesLost(t.Context(), "n1"); err != nil {
		t.Fatal(err)
	}
	n, err := st.ReviveLost(t.Context(), "n1", map[string]string{"sb-a": "running", "sb-b": "paused"})
	if err != nil || n != 2 {
		t.Fatalf("revived %d (err %v), want 2", n, err)
	}
	for id, want := range map[string]string{"sb-a": "running", "sb-b": "paused", "sb-gone": store.StateLost} {
		if rec, _, _ := st.SandboxByID(t.Context(), id); rec.State != want {
			t.Errorf("%s state %q, want %q", id, rec.State, want)
		}
	}
}

// A caller holding a node for longer than the TTL (a drain) must not have its
// requests refused while the node itself keeps heartbeating.
func TestForwardJudgesTheNodeByItsCurrentHeartbeat(t *testing.T) {
	s := NewServer("k", "t")
	now := time.Now()
	upsert(s.reg, "n-live", "http://10.255.255.1:1", "key", "", nil, "", cluster.Capacity{}, now)
	held := &cluster.Node{ID: "n-live", URL: "http://10.255.255.1:1", LastSeen: now.Add(-time.Hour)}
	if _, quiet := s.nodeWentQuiet(held); quiet {
		t.Fatal("a node heartbeating now was judged quiet from a caller's hour-old copy")
	}
}
