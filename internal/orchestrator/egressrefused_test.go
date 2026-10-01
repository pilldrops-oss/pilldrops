package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// The control plane must refuse a domain rule itself, not lean on the node: a
// node running an older binary accepts the field and answers 201, and the caller
// is handed a sandbox that reads as confined and reaches anything. Address and
// CIDR rules pass through — the node's netns chain enforces those.
//
// No node is registered here, so a create that reached scheduling would 503 —
// a 501 also proves the refusal happens before placement.
func TestControlPlaneRefusesDomainEgressRulesUntilTheNameLayerExists(t *testing.T) {
	srv, _ := newProjectServer(t)

	for _, body := range []string{
		`{"template":"base","network":{"allow":["api.stripe.com"]}}`,
		`{"template":"base","network":{"deny":["*.evil.example"]}}`,
	} {
		code, out := do(t, srv, "POST", "/v1/sandboxes", "fleet-key", body)
		if code != http.StatusNotImplemented {
			t.Errorf("create %s = %d (%v), want 501 — an unenforced allowlist reads as protection", body, code, out)
		}
		if out["code"] != "egress_not_enforced" {
			t.Errorf("create %s answered code %v, want the same code the node uses", body, out["code"])
		}
	}

	// An address-only policy is enforceable, so the control plane forwards it;
	// with no node registered that is a 503, never the 501 above.
	for _, body := range []string{
		`{"template":"base","network":{"allow":["203.0.113.7"]}}`,
		`{"template":"base","network":{"deny":["0.0.0.0/0"]}}`,
		`{"template":"base","network":null}`,
	} {
		if code, out := do(t, srv, "POST", "/v1/sandboxes", "fleet-key", body); code == http.StatusNotImplemented {
			t.Errorf("create %s was refused as unenforceable (%v) — address rules are enforced "+
				"in the node's netns chain, so this refuses a policy that works", body, out)
		}
	}
}

// API B18: a domain allowlist pinned to docker was answered "no docker node
// has capacity", which sends an operator to add RAM. Docker never runs the
// name layer, so the answer is the runtime, not the room.
func TestADomainAllowlistOnDockerNamesTheRuntimeNotCapacity(t *testing.T) {
	s := NewServer("fleet-key", "nodetoken")
	capy := cluster.Capacity{Cores: 8, MemTotalMB: 64000, MemAvailMB: 64000}
	for _, n := range []struct {
		id, runtime string
		domains     bool
	}{{"d1", "docker", false}, {"f1", "firecracker", true}} {
		if err := s.reg.Upsert(cluster.NodeUpsert{ID: n.id, URL: "http://" + n.id + ".invalid", Key: "k-" + n.id, Cap: capy}, s.now()); err != nil {
			t.Fatal(err)
		}
		s.reg.SetNodeMeta(n.id, cluster.NodeMeta{Runtime: n.runtime, EgressDomains: n.domains})
	}
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)

	code, out := do(t, srv, "POST", "/v1/sandboxes", "fleet-key",
		`{"template":"base","runtime":"docker","network":{"allow":["example.com"]}}`)
	msg, _ := out["error"].(string)
	if code != http.StatusNotImplemented || out["code"] != "egress_not_enforced" {
		t.Fatalf("create = %d %v, want 501 egress_not_enforced", code, out)
	}
	if !strings.Contains(msg, "docker runtime") || !strings.Contains(msg, "firecracker") {
		t.Errorf("error %q should name the docker runtime and point at firecracker", msg)
	}
}

// A full firecracker node in a mixed fleet is pressure, not a missing
// capability: with no runtime pinned, a domain allowlist waits for room (503)
// rather than being refused for good (501) because a docker node has some.
func TestADomainAllowlistOnAFullFirecrackerNodeIsCapacity(t *testing.T) {
	s := NewServer("fleet-key", "nodetoken")
	for _, n := range []struct {
		id, runtime string
		domains     bool
		availMB     int
	}{{"d1", "docker", false, 64000}, {"f1", "firecracker", true, 0}} {
		capy := cluster.Capacity{Cores: 8, MemTotalMB: 64000, MemAvailMB: n.availMB}
		if err := s.reg.Upsert(cluster.NodeUpsert{ID: n.id, URL: "http://" + n.id + ".invalid", Key: "k-" + n.id, Cap: capy}, s.now()); err != nil {
			t.Fatal(err)
		}
		s.reg.SetNodeMeta(n.id, cluster.NodeMeta{Runtime: n.runtime, EgressDomains: n.domains})
	}
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)

	code, out := do(t, srv, "POST", "/v1/sandboxes", "fleet-key",
		`{"template":"base","network":{"allow":["example.com"]}}`)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("create = %d %v, want 503: the egress-capable node is full, not missing", code, out)
	}
}
