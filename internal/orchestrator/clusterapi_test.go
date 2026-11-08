package orchestrator

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// registerNode drives a heartbeat through the public endpoint.
func registerNode(t *testing.T, srv *httptest.Server, hb string) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/internal/heartbeat", strings.NewReader(hb))
	req.Header.Set("Authorization", "Bearer "+bearerFor(hb))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("heartbeat failed: %v (%v)", err, resp)
	}
	resp.Body.Close()
}

func getJSON(t *testing.T, srv *httptest.Server, method, path string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer apikey")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	b, _ := io.ReadAll(resp.Body)
	if len(b) > 0 {
		_ = json.Unmarshal(b, &out)
	}
	return resp.StatusCode, out
}

func TestClusterStatsHeadroom(t *testing.T) {
	s := fleetServer("apikey", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	registerNode(t, srv, `{"id":"n1","url":"http://n1","zone":"fsn1","capacity":{"cpu_cores":16,"mem_total_mb":64000,"mem_available_mb":24000,"mem_committed_mb":4000,"sandboxes_active":3}}`)

	code, stats := getJSON(t, srv, "GET", "/v1/cluster/stats")
	if code != 200 {
		t.Fatalf("cluster/stats = %d, want 200", code)
	}
	if got := stats["nodes_total"].(float64); got != 1 {
		t.Fatalf("nodes_total = %v, want 1", got)
	}
	if got := stats["nodes_available"].(float64); got != 1 {
		t.Fatalf("nodes_available = %v, want 1", got)
	}
	// RAM bounds headroom here; the over-commit ceiling is larger.
	want := (24000 - cluster.FitMarginMB) / 1024
	if got := int(stats["headroom_sandboxes"].(float64)); got != want {
		t.Fatalf("headroom_sandboxes = %d, want %d", got, want)
	}
	if got := stats["sandboxes_active"].(float64); got != 3 {
		t.Fatalf("sandboxes_active = %v, want 3", got)
	}
	zones, _ := stats["zones"].([]any)
	if len(zones) != 1 {
		t.Fatalf("zones = %v, want 1 zone", zones)
	}
	z := zones[0].(map[string]any)
	if z["zone"] != "fsn1" {
		t.Fatalf("zone name = %v, want fsn1", z["zone"])
	}
	if z["status"] != "ok" {
		t.Fatalf("zone status = %v, want ok (healthy headroom)", z["status"])
	}
	rec := stats["recommendation"].(map[string]any)
	if rec["level"] != "ok" {
		t.Fatalf("recommendation level = %v, want ok", rec["level"])
	}
}

func TestClusterStatsFullZoneCritical(t *testing.T) {
	s := fleetServer("apikey", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	registerNode(t, srv, `{"id":"n1","url":"http://n1","zone":"fsn1","capacity":{"cpu_cores":8,"mem_total_mb":8000,"mem_available_mb":600,"mem_committed_mb":7400,"sandboxes_active":6}}`)

	code, stats := getJSON(t, srv, "GET", "/v1/cluster/stats")
	if code != 200 {
		t.Fatalf("cluster/stats = %d", code)
	}
	if got := stats["headroom_sandboxes"].(float64); got != 0 {
		t.Fatalf("headroom_sandboxes = %v, want 0 (node full)", got)
	}
	if got := stats["nodes_full"].(float64); got != 1 {
		t.Fatalf("nodes_full = %v, want 1", got)
	}
	rec := stats["recommendation"].(map[string]any)
	if rec["level"] != "critical" {
		t.Fatalf("recommendation level = %v, want critical", rec["level"])
	}
	if got := rec["suggested_nodes"].(float64); got < 1 {
		t.Fatalf("suggested_nodes = %v, want >= 1", got)
	}
}

func TestNodeManagementEndpoints(t *testing.T) {
	s := fleetServer("apikey", "nodetoken")
	s.SetStore(store.OpenForTest(t)) // removal is store-backed
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	registerNode(t, srv, `{"id":"n1","url":"http://n1","zone":"fsn1","capacity":{"mem_total_mb":64000,"mem_available_mb":40000}}`)

	if code, _ := getJSON(t, srv, "POST", "/v1/nodes/n1/cordon"); code != 200 {
		t.Fatalf("cordon = %d, want 200", code)
	}
	code, nodes := getJSON(t, srv, "GET", "/v1/nodes")
	if code != 200 {
		t.Fatalf("nodes = %d", code)
	}
	list := nodes["nodes"].([]any)
	if got := list[0].(map[string]any)["status"]; got != "cordoned" {
		t.Fatalf("status after cordon = %v, want cordoned", got)
	}
	// Cordon survives a re-heartbeat.
	registerNode(t, srv, `{"id":"n1","url":"http://n1","zone":"fsn1","capacity":{"mem_total_mb":64000,"mem_available_mb":40000}}`)
	if n, _ := s.reg.Node("n1"); !n.Cordoned {
		t.Fatal("cordon lost across heartbeat")
	}
	if code, _ := getJSON(t, srv, "POST", "/v1/nodes/n1/uncordon"); code != 200 {
		t.Fatalf("uncordon = %d", code)
	}
	if n, _ := s.reg.Node("n1"); n.Cordoned {
		t.Fatal("uncordon did not clear the flag")
	}
	if code, _ := getJSON(t, srv, "POST", "/v1/nodes/n1/drain"); code != 200 {
		t.Fatalf("drain = %d", code)
	}
	if n, _ := s.reg.Node("n1"); !n.Draining {
		t.Fatal("drain did not set the flag")
	}
	if code, _ := getJSON(t, srv, "DELETE", "/v1/nodes/n1"); code != 200 {
		t.Fatalf("remove = %d, want 200", code)
	}
	// Removing an already-removed (now offline) node is idempotent, not 404:
	// an offline node must still be revocable.
	if code, _ := getJSON(t, srv, "DELETE", "/v1/nodes/n1"); code != 200 {
		t.Fatalf("remove already-removed = %d, want 200", code)
	}
	if code, _ := getJSON(t, srv, "POST", "/v1/nodes/ghost/cordon"); code != 404 {
		t.Fatalf("cordon missing = %d, want 404", code)
	}
}

// Asserts "no credential comes back", not 404: unhandled /v1 paths fall through
// to the node catch-all.
func TestLegacyJoinCommandNoLongerHandsOutCredentials(t *testing.T) {
	s := fleetServer("apikey", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	code, out := getJSON(t, srv, "GET", "/v1/cluster/join-command?id=kvm-2&zone=nbg1")
	if code == 200 {
		t.Fatalf("the secret-embedding onboarding helper still answers 200: %v", out)
	}
	body := fmt.Sprintf("%v", out)
	for _, secret := range []string{"nodetoken", "sf_node_", "ORCHESTRATOR_TOKEN"} {
		if strings.Contains(body, secret) {
			t.Fatalf("a credential (%q) came back from the retired join-command route: %v", secret, out)
		}
	}
}

func TestJoinTokenMintReplacesIt(t *testing.T) {
	s := fleetServer("apikey", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	code, out := getJSON(t, srv, "POST", "/v1/join-tokens")
	if code != 200 {
		t.Fatalf("POST /v1/join-tokens = %d, want 200", code)
	}
	tok, _ := out["token"].(string)
	if tok == "" {
		t.Fatal("no token minted")
	}
	cmd, _ := out["bootstrap_cmd"].(string)
	if !strings.Contains(cmd, "/install.sh") {
		t.Fatalf("bootstrap_cmd = %q, want it to fetch install.sh", cmd)
	}
	if strings.Contains(cmd, tok) || strings.Contains(cmd, "nodetoken") {
		t.Fatalf("bootstrap_cmd embeds a secret — that is the defect the join-token "+
			"flow exists to remove: %q", cmd)
	}
}

func TestImagesProxyNoHealthyNode(t *testing.T) {
	s := fleetServer("apikey", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	if code, out := getJSON(t, srv, "GET", "/v1/images"); code != 503 || out["error"] != "no healthy node" {
		t.Fatalf("images with no node = %d %v, want 503 no healthy node", code, out)
	}
}

func TestImagesProxyForwardsToHealthyNode(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/images" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"images":[{"id":"base","published":true}]}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer node.Close()

	s := fleetServer("apikey", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	registerNode(t, srv, `{"id":"n1","url":"`+node.URL+`","zone":"fsn1","capacity":{"mem_total_mb":64000,"mem_available_mb":40000}}`)

	code, out := getJSON(t, srv, "GET", "/v1/images")
	if code != 200 {
		t.Fatalf("images = %d, want 200", code)
	}
	imgs, ok := out["images"].([]any)
	if !ok || len(imgs) != 1 {
		t.Fatalf("images payload not proxied: %v", out)
	}
}

func TestUnreachableThenRemove(t *testing.T) {
	s := fleetServer("apikey", "nodetoken")
	s.SetStore(store.OpenForTest(t)) // removal is store-backed
	base := time.Now()
	s.now = func() time.Time { return base }
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	registerNode(t, srv, `{"id":"n1","url":"http://n1","zone":"fsn1","capacity":{"mem_total_mb":64000,"mem_available_mb":40000}}`)
	s.now = func() time.Time { return base.Add(cluster.NodeTTL + time.Minute) }

	_, nodes := getJSON(t, srv, "GET", "/v1/nodes")
	list := nodes["nodes"].([]any)
	if got := list[0].(map[string]any)["status"]; got != "unreachable" {
		t.Fatalf("status = %v, want unreachable", got)
	}
	if code, _ := getJSON(t, srv, "DELETE", "/v1/nodes/n1"); code != 200 {
		t.Fatalf("remove unreachable = %d, want 200", code)
	}
}
