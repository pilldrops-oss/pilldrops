package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestControlPlaneServesTheDashboard(t *testing.T) {
	s := NewServer("k", "t")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	for _, path := range []string{"/dashboard", "/dashboard/login"} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			t.Errorf("GET %s = 404 — the control plane is the user-facing tier", path)
		}
	}
}

func TestDashboardCanBeDisabled(t *testing.T) {
	t.Setenv("ORCHESTRATOR_UI", "off")
	s := NewServer("k", "t")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/dashboard")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /dashboard with ORCHESTRATOR_UI=off = %d, want 404", resp.StatusCode)
	}
}

// webui also has an /install.sh (the CLI installer); node-setup curls this one by URL.
func TestInstallScriptIsTheNodeJoinScript(t *testing.T) {
	s := NewServer("k", "t")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/install.sh")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /install.sh = %d, want 200", resp.StatusCode)
	}
}

func TestFleetIsStillReadableOverTheAPI(t *testing.T) {
	s := NewServer("k", "t")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/nodes", nil)
	req.Header.Set("Authorization", "Bearer k")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /v1/nodes: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v1/nodes = %d, want 200", resp.StatusCode)
	}
}
