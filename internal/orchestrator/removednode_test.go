package orchestrator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// heartbeatStatus drives a heartbeat and returns its status code, unlike
// registerNode, which asserts success.
func heartbeatStatus(t *testing.T, srv *httptest.Server, hb string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/internal/heartbeat", strings.NewReader(hb))
	req.Header.Set("Authorization", "Bearer "+bearerFor(hb))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestRemovedNodeHeartbeatRefused(t *testing.T) {
	s := fleetServer("apikey", "nodetoken")
	s.SetStore(store.OpenForTest(t))
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	op := httptest.NewServer(s.Routes())
	defer op.Close()

	hb := `{"id":"n1","url":"http://n1","zone":"fsn1","capacity":{"mem_total_mb":64000,"mem_available_mb":40000}}`
	registerNode(t, srv, hb)

	if code, _ := getJSON(t, op, "DELETE", "/v1/nodes/n1"); code != 200 {
		t.Fatalf("remove = %d, want 200", code)
	}
	if code := heartbeatStatus(t, srv, hb); code != 403 {
		t.Fatalf("heartbeat from a removed node = %d, want 403", code)
	}
}

// An offline node — never in the registry at removal time, or gone from it
// after a restart — must still be revocable: removal must not depend on the
// node currently being there to remove.
func TestRemovingAnOfflineNodeStillRevokesIt(t *testing.T) {
	s := fleetServer("apikey", "nodetoken")
	s.SetStore(store.OpenForTest(t))
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	op := httptest.NewServer(s.Routes())
	defer op.Close()

	if code, _ := getJSON(t, op, "DELETE", "/v1/nodes/ghost-node"); code != 200 {
		t.Fatalf("remove offline node = %d, want 200", code)
	}
	hb := `{"id":"ghost-node","url":"http://ghost","zone":"fsn1","capacity":{"mem_total_mb":1,"mem_available_mb":1}}`
	if code := heartbeatStatus(t, srv, hb); code != 403 {
		t.Fatalf("heartbeat from a removed-while-offline node = %d, want 403", code)
	}
}

// A removed id's credentials are refused forever, so handing it straight
// back out at the next join would just re-admit a node that can never
// heartbeat. The re-joined machine must get a fresh id instead.
func TestRemovedNodeIDNotReissuedAtJoin(t *testing.T) {
	s := fleetServer("apikey", "nodetoken")
	s.SetStore(store.OpenForTest(t))
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	op := httptest.NewServer(s.Routes())
	defer op.Close()

	registerNode(t, srv, `{"id":"n1","url":"http://n1","zone":"fsn1","capacity":{"mem_total_mb":1,"mem_available_mb":1}}`)
	if code, _ := getJSON(t, op, "DELETE", "/v1/nodes/n1"); code != 200 {
		t.Fatalf("remove = %d, want 200", code)
	}

	tok, _ := mintToken(t, op, "apikey")["token"].(string)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/join",
		strings.NewReader(`{"token":"`+tok+`","suggested_id":"n1","zone":"fsn1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("join = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["node_id"] == "n1" {
		t.Fatalf("a re-joined machine was handed the removed id back: %v", out)
	}
}

// Node credentials are derived, so revocation has to be durable: an
// in-memory-only removal is forgotten the moment the process restarts, which
// is the original bug. A fresh Server on the same store must still refuse.
func TestRemovedNodeSurvivesRestart(t *testing.T) {
	st := store.OpenForTest(t)

	s1 := fleetServer("apikey", "nodetoken")
	s1.SetStore(st)
	srv1 := httptest.NewServer(s1.Routes())
	op1 := httptest.NewServer(s1.Routes())

	registerNode(t, srv1, `{"id":"n1","url":"http://n1","zone":"fsn1","capacity":{"mem_total_mb":1,"mem_available_mb":1}}`)
	if code, _ := getJSON(t, op1, "DELETE", "/v1/nodes/n1"); code != 200 {
		t.Fatalf("remove = %d, want 200", code)
	}
	srv1.Close()
	op1.Close()

	s2 := fleetServer("apikey", "nodetoken") // a new process, as a restart would start
	s2.SetStore(st)
	srv2 := httptest.NewServer(s2.Routes())
	defer srv2.Close()

	hb := `{"id":"n1","url":"http://n1","zone":"fsn1","capacity":{"mem_total_mb":1,"mem_available_mb":1}}`
	if code := heartbeatStatus(t, srv2, hb); code != 403 {
		t.Fatalf("heartbeat after restart = %d, want 403", code)
	}
}
