package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// Every node-served /v1 path the SPA calls must be forwarded, not 404 here.
func TestNodeResourcesAreReachableThroughTheControlPlane(t *testing.T) {
	var gotPath, gotMethod string
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer node.Close()

	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()
	s := NewServer("fleet-key", "nodetoken")
	s.SetStore(st)
	upsert(s.reg, "n1", node.URL, "kk", "", nil, "",
		cluster.Capacity{Cores: 8, MemTotalMB: 64000, MemAvailMB: 64000}, s.now())

	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	// /v1/projects is absent on purpose: the control plane answers it from
	// the shared store instead of forwarding.
	for _, path := range []string{
		"/v1/keys", "/v1/templates", "/v1/cron",
		"/v1/integrations", "/v1/host",
	} {
		gotPath, gotMethod = "", ""
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer fleet-key")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()

		if resp.StatusCode == http.StatusNotFound {
			t.Errorf("GET %s = 404 through the control plane — the SPA calls this "+
				"and a node serves it", path)
			continue
		}
		if gotPath != path {
			t.Errorf("GET %s reached the node as %q", path, gotPath)
		}
		if gotMethod != "GET" {
			t.Errorf("GET %s reached the node as %s", path, gotMethod)
		}
	}
}

func TestCatchAllDoesNotShadowControlPlaneRoutes(t *testing.T) {
	reached := false
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	defer node.Close()

	s := NewServer("fleet-key", "nodetoken")
	upsert(s.reg, "n1", node.URL, "kk", "", nil, "",
		cluster.Capacity{Cores: 8, MemTotalMB: 64000, MemAvailMB: 64000}, s.now())
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/nodes", nil)
	req.Header.Set("Authorization", "Bearer fleet-key")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if reached {
		t.Error("GET /v1/nodes was forwarded to a node — the catch-all shadowed " +
			"a control-plane route")
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v1/nodes = %d, want 200", resp.StatusCode)
	}
}

func TestCatchAllIsBehindAuth(t *testing.T) {
	reached := false
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	defer node.Close()

	s := NewServer("fleet-key", "nodetoken")
	upsert(s.reg, "n1", node.URL, "kk", "", nil, "",
		cluster.Capacity{Cores: 8, MemTotalMB: 64000, MemAvailMB: 64000}, s.now())
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/v1/keys") // no credential
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if reached {
		t.Error("an unauthenticated request reached a node through the catch-all")
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET /v1/keys = %d, want 401", resp.StatusCode)
	}
}

func TestControlPlaneOwnedRoutesDoNotReachANode(t *testing.T) {
	reached := false
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	defer node.Close()

	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()
	s := NewServer("fleet-key", "nodetoken")
	s.SetStore(st)
	upsert(s.reg, "n1", node.URL, "kk", "", nil, "",
		cluster.Capacity{Cores: 8, MemTotalMB: 64000, MemAvailMB: 64000}, s.now())
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	for _, path := range []string{"/v1/projects"} {
		reached = false
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer fleet-key")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if reached {
			t.Errorf("GET %s reached a node — it is answered from the shared store on "+
				"this tier, and forwarding it would ask a node about rows it does not have", path)
		}
		if resp.StatusCode == http.StatusNotFound {
			t.Errorf("GET %s = 404 on the control plane that owns it", path)
		}
	}
}
