package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// A reaped node's paused sandbox stays paused, reads as paused without a
// download, and its first action rehomes it onto a live node and runs there.
// One no node holds an archive for is lost after all; a running one was lost
// at the reap, since its last pause is not its latest state.
func TestPausedSandboxOfADeadNodeRehomes(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "rehome.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := NewServer("k", "t")
	s.SetStore(st)

	const org = "u:rehome"
	for id, state := range map[string]string{"sb-paused": "paused", "sb-noarch": "paused", "sb-running": "running"} {
		if err := st.SaveSandbox(t.Context(), store.SandboxRecord{
			ID: id, OrgID: org, NodeID: "node-dead", Template: "base", State: state,
			VCPU: 1, MemMiB: 512, CreatedAt: 1, UpdatedAt: 1, ExpiresAt: 9e9,
		}); err != nil {
			t.Fatal(err)
		}
	}
	s.markLost([]string{"node-dead"})
	if rec, _, _ := st.SandboxByID(t.Context(), "sb-running"); rec.State != store.StateLost {
		t.Fatalf("a running sandbox of the dead node is %q, want lost", rec.State)
	}
	if rec, _, _ := st.SandboxByID(t.Context(), "sb-paused"); rec.State != "paused" {
		t.Fatalf("a paused sandbox of the dead node is %q, want still paused", rec.State)
	}

	var mu sync.Mutex
	var calls []string
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/v1/sandboxes/sb-noarch/rehome" {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"sandbox cannot be rehomed here: no archived snapshot of it"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"running"}`))
	}))
	defer live.Close()
	upsert(s.reg, "node-live", live.URL, "node-key", "", nil, "",
		cluster.Capacity{Cores: 8, MemTotalMB: 8192, MemAvailMB: 8192}, s.now())

	req := func(method, id, rest string) *httptest.ResponseRecorder {
		path := "/v1/sandboxes/" + id
		if rest != "" {
			path += "/" + rest
		}
		r := httptest.NewRequest(method, path, nil)
		r.SetPathValue("id", id)
		r.SetPathValue("rest", rest)
		r.Header.Set(orgOverride, org)
		r = r.WithContext(withOperator(r.Context()))
		w := httptest.NewRecorder()
		if rest == "" {
			s.handleSandboxGet(w, r)
		} else {
			s.handleSandboxProxy(w, r)
		}
		return w
	}

	if w := req(http.MethodGet, "sb-paused", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"paused"`) {
		t.Fatalf("GET of an orphaned paused sandbox = %d %s", w.Code, w.Body.String())
	}
	if len(calls) != 0 {
		t.Fatalf("a read reached a node (%v): it must not start a download", calls)
	}
	if w := req(http.MethodPost, "sb-paused", "resume"); w.Code != 200 {
		t.Fatalf("resume = %d %s", w.Code, w.Body.String())
	}
	if strings.Join(calls, ",") != "POST /v1/sandboxes/sb-paused/rehome,POST /v1/sandboxes/sb-paused/resume" {
		t.Fatalf("node saw %v, want rehome then resume", calls)
	}
	if n, ok := s.reg.NodeFor("sb-paused"); !ok || n.ID != "node-live" {
		t.Fatalf("after the rehome it routes to %v, want node-live", n)
	}

	if w := req(http.MethodPost, "sb-noarch", "resume"); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("resume without an archive = %d %s", w.Code, w.Body.String())
	}
	if rec, _, _ := st.SandboxByID(t.Context(), "sb-noarch"); rec.State != store.StateLost {
		t.Fatalf("a sandbox no node could rehome is %q, want lost", rec.State)
	}
}
