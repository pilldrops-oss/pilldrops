package orchestrator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// nestedNode registers one fake node of the given runtime and reports which
// node a forwarded create actually reached.
func nestedNode(t *testing.T, s *Server, id, runtime string) *string {
	t.Helper()
	var reached string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = id
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"sb-1","status":"running"}`))
	}))
	t.Cleanup(srv.Close)
	if err := s.reg.Upsert(cluster.NodeUpsert{
		ID: id, URL: srv.URL, Key: "k",
		Cap: cluster.Capacity{Cores: 8, MemTotalMB: fleetNodeMemMB, MemAvailMB: fleetNodeMemMB},
	}, s.now()); err != nil {
		t.Fatalf("upsert %s: %v", id, err)
	}
	// The runtime reaches the registry through the heartbeat's meta, not Upsert.
	s.reg.SetNodeMeta(id, cluster.NodeMeta{Runtime: runtime, Isolation: true})
	if n, ok := s.reg.Node(id); !ok || n.Runtime != runtime {
		t.Fatalf("node %s did not take runtime %q — the fixture would prove nothing", id, runtime)
	}
	return &reached
}

// The docker template lands only on a firecracker node, and on a fleet with no
// firecracker node the refusal is a 400 naming the runtime, not a retryable 503.
func TestDockerTemplatePlacement(t *testing.T) {
	t.Run("prefers the firecracker node over the docker node", func(t *testing.T) {
		s := NewServer("api-key", "node-token")
		dk := nestedNode(t, s, "node-docker", "docker")
		fc := nestedNode(t, s, "node-fc", "firecracker")

		if w := doCreate(t, s, `{"template":"docker"}`); w.Code != http.StatusCreated {
			t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
		}
		if *fc != "node-fc" {
			t.Errorf("the firecracker node was not used (reached %q)", *fc)
		}
		if *dk != "" {
			t.Errorf("the create reached the docker node %q, which cannot host a nested daemon", *dk)
		}
	})

	t.Run("no firecracker node online is a 400 naming firecracker", func(t *testing.T) {
		s := NewServer("api-key", "node-token")
		nestedNode(t, s, "node-docker", "docker")

		w := doCreate(t, s, `{"template":"docker"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("create = %d, want 400 (no retry will ever make a docker node able to run this): %s",
				w.Code, w.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		msg, _ := out["error"].(string)
		if !strings.Contains(msg, "firecracker") {
			t.Errorf("refusal %q does not name firecracker", msg)
		}

		// Positive control in the same run: an unpinned template still places
		// on that same docker node, so the 400 above is about the pin.
		if w := doCreate(t, s, `{"template":"base"}`); w.Code != http.StatusCreated {
			t.Fatalf("create base = %d, want 201 — the docker node is usable: %s", w.Code, w.Body.String())
		}
	})
}
