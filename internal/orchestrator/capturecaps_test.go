package orchestrator

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// The capture-plane verbs existed only on the node's own /v1/capabilities,
// which tenants never reach, so the fleet answer claimed none of them and
// nothing could tell a client whether checkpoints, named snapshots or standby
// were available at all.
//
// OR, not AND, and deliberately unlike auto_pause: placement routes a create
// that needs one of these onto a node that has it, so one capable node makes
// the verb available. The narrow case is the point — on firecracker
// named_snapshot needs FC_NETNS as well as FC_MEM_SNAPSHOTS, so a node can
// report checkpoint and standby and not named_snapshot.
func TestFleetReportsTheCapturePlaneVerbs(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	// Control: a fleet whose nodes report none of them must claim none.
	bare := aggregateCapabilities([]*cluster.Node{dockerNode("d1", now), fcNode("f1", now)}, now, capsTTL)
	if bare.Checkpoint || bare.NamedSnapshot || bare.Standby || bare.TagNetworks {
		t.Fatalf("a fleet reporting nothing claimed %+v", bare)
	}

	// A node with snapshots but no namespaces: checkpoint and standby, no
	// named_snapshot and no tag_networks. Both need the netns half.
	half := fcNode("f-half", now)
	half.Checkpoint, half.Standby = true, true
	got := aggregateCapabilities([]*cluster.Node{dockerNode("d1", now), half}, now, capsTTL)
	if !got.Checkpoint || !got.Standby {
		t.Errorf("checkpoint=%v standby=%v, want both true from the capable node", got.Checkpoint, got.Standby)
	}
	if got.NamedSnapshot || got.TagNetworks {
		t.Errorf("named_snapshot=%v tag_networks=%v, but no node reports either — a client would "+
			"offer a verb whose every create 501s", got.NamedSnapshot, got.TagNetworks)
	}

	full := fcNode("f-full", now)
	full.Checkpoint, full.Standby, full.NamedSnapshot, full.TagNetworks = true, true, true, true
	got = aggregateCapabilities([]*cluster.Node{dockerNode("d1", now), half, full}, now, capsTTL)
	if !got.NamedSnapshot || !got.TagNetworks {
		t.Errorf("named_snapshot=%v tag_networks=%v with one fully capable node, want both true",
			got.NamedSnapshot, got.TagNetworks)
	}

	// An unhealthy node contributes nothing: it cannot take a placement.
	stale := fcNode("f-stale", now.Add(-time.Hour))
	stale.NamedSnapshot = true
	if got := aggregateCapabilities([]*cluster.Node{dockerNode("d1", now), stale}, now, capsTTL); got.NamedSnapshot {
		t.Error("a node last seen an hour ago still made named_snapshot true")
	}
}

// The keys are the contract. Every other test here sets cluster.Node fields
// directly and would pass with a typo in the heartbeat's JSON name, which would
// make the whole fleet report false forever. This one posts the literal payload
// a node sends. Keep it in step with internal/nodeboot/heartbeat.go.
func TestTheHeartbeatKeysReachTheFleetAnswer(t *testing.T) {
	s := fleetServer("apikey", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	registerNode(t, srv, `{"id":"n1","url":"http://n1","zone":"fsn1","runtime":"firecracker",
		"isolation":true,"auto_pause":true,"tag_networks":true,
		"checkpoint":true,"named_snapshot":true,"standby":true,
		"capacity":{"cpu_cores":8,"mem_total_mb":64000,"mem_available_mb":32000}}`)

	code, out := getJSON(t, srv, "GET", "/v1/capabilities")
	if code != 200 {
		t.Fatalf("GET /v1/capabilities = %d", code)
	}
	for _, k := range []string{"tag_networks", "checkpoint", "named_snapshot", "standby"} {
		if v, ok := out[k].(bool); !ok || !v {
			t.Errorf("fleet %s = %v, but the node's heartbeat said true — the key does not "+
				"match internal/nodeboot/heartbeat.go and every node reads as incapable", k, out[k])
		}
	}
	// The scheduler reads the same record; a create from a snapshot must now be
	// filtered onto this node rather than placed blind.
	if !s.reg.AnyWithCapability(cluster.CapNamedSnapshot, s.now(), cluster.NodeTTL) {
		t.Error("the registry does not see named_snapshot from a node that heartbeats it")
	}
}
