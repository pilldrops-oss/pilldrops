package orchestrator

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHeartbeatThenScheduleAndProxy(t *testing.T) {
	var gotCreate, gotDestroy bool
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/sandboxes":
			gotCreate = true
			w.WriteHeader(201)
			w.Write([]byte(`{"id":"sb-9","status":"running","url":"http://node/preview/sb-9/8000/"}`))
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/v1/sandboxes/"):
			gotDestroy = true
			w.WriteHeader(200)
			w.Write([]byte(`{"status":"destroyed"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer node.Close()

	s := fleetServer("apikey", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	hb := `{"id":"n1","url":"` + node.URL + `","key":"nodekey","capacity":{"mem_total_mb":64000,"mem_available_mb":40000}}`
	req, _ := http.NewRequest("POST", srv.URL+"/internal/heartbeat", strings.NewReader(hb))
	req.Header.Set("Authorization", "Bearer "+bearerFor(hb))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("heartbeat = %d", resp.StatusCode)
	}

	req, _ = http.NewRequest("POST", srv.URL+"/v1/sandboxes", strings.NewReader(`{"template":"base","memory_mb":512}`))
	req.Header.Set("Authorization", "Bearer apikey")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 201 || !gotCreate {
		t.Fatalf("create = %d, gotCreate=%v", resp.StatusCode, gotCreate)
	}
	var out map[string]any
	b, _ := io.ReadAll(resp.Body)
	json.Unmarshal(b, &out)
	if out["node"] != "n1" {
		t.Fatalf("response missing node: %v", out)
	}
	if u, _ := out["url"].(string); !strings.Contains(u, "/preview/sb-9/8000/") || strings.Contains(u, "node/preview") {
		t.Fatalf("url not rewritten to orchestrator: %q", u)
	}

	req, _ = http.NewRequest("DELETE", srv.URL+"/v1/sandboxes/sb-9", nil)
	req.Header.Set("Authorization", "Bearer apikey")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 || !gotDestroy {
		t.Fatalf("destroy = %d, gotDestroy=%v", resp.StatusCode, gotDestroy)
	}
	if _, ok := s.reg.NodeFor("sb-9"); ok {
		t.Fatal("sb-9 should be unplaced after destroy")
	}
}

func TestCreateNoCapacity(t *testing.T) {
	s := NewServer("k", "t")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/sandboxes", strings.NewReader(`{"memory_mb":512}`))
	req.Header.Set("Authorization", "Bearer k")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 503 { // no nodes registered
		t.Fatalf("create with no nodes = %d, want 503", resp.StatusCode)
	}
}
