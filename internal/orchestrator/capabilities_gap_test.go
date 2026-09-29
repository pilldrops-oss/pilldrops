package orchestrator

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// The fleet answer is the only /v1/capabilities a tenant reaches, and it left
// out exactly the three verbs the endpoint exists to let them check: whether a
// second port can be previewed, whether raw TCP can be forwarded, and whether a
// sandbox can run its own dockerd.
//
// Against the old code this fails on the first missing key: the fields were not
// on the struct, so a caller could not tell "absent" from "false".
func TestTheFleetAnswerCarriesTheVerbsANodeAdvertises(t *testing.T) {
	now := time.Now()
	nodes := []*cluster.Node{{
		ID: "n1", Runtime: "firecracker", LastSeen: now,
		MultiPort: true, L4Forward: true, NestedDocker: true,
	}}
	raw, err := json.Marshal(aggregateCapabilities(nodes, now, cluster.NodeTTL))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"multiport", "l4_forward", "nested_docker"} {
		v, present := got[k]
		if !present {
			t.Errorf("the fleet answer has no %q at all, so a caller cannot tell absent from false", k)
			continue
		}
		if v != true {
			t.Errorf("%s = %v although a counted node advertises it", k, v)
		}
	}
}

// ANY, like the other caller-facing verbs: a create that needs one is placed
// where it exists, so "some node can" is the answer a caller can act on.
func TestAVerbIsTrueWhenAnyCountedNodeHasIt(t *testing.T) {
	now := time.Now()
	nodes := []*cluster.Node{
		{ID: "docker-node", Runtime: "docker", LastSeen: now},
		{ID: "fc-node", Runtime: "firecracker", LastSeen: now, MultiPort: true, NestedDocker: true},
	}
	caps := aggregateCapabilities(nodes, now, cluster.NodeTTL)
	if !caps.MultiPort || !caps.NestedDocker {
		t.Errorf("multiport=%v nested_docker=%v; one node has both", caps.MultiPort, caps.NestedDocker)
	}
	if caps.L4Forward {
		t.Error("l4_forward is true although no node advertises it")
	}
}

// An unhealthy node's verbs are not the fleet's.
func TestADeadNodesVerbsDoNotCount(t *testing.T) {
	now := time.Now()
	nodes := []*cluster.Node{{
		ID: "gone", Runtime: "firecracker",
		LastSeen:  now.Add(-10 * cluster.NodeTTL),
		MultiPort: true, L4Forward: true, NestedDocker: true,
	}}
	caps := aggregateCapabilities(nodes, now, cluster.NodeTTL)
	if caps.MultiPort || caps.L4Forward || caps.NestedDocker {
		t.Errorf("a node that has not beaten in %v still contributes its verbs: %+v",
			10*cluster.NodeTTL, caps)
	}
}
