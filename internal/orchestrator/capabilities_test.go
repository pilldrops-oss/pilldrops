package orchestrator

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

const capsTTL = cluster.NodeTTL

func fcNode(id string, seen time.Time) *cluster.Node {
	return &cluster.Node{
		ID: id, URL: "http://" + id, Runtime: "firecracker", Isolation: true,
		Templates: []string{"base", "code-interpreter"}, LastSeen: seen,
	}
}

func dockerNode(id string, seen time.Time) *cluster.Node {
	return &cluster.Node{
		ID: id, URL: "http://" + id, Runtime: "docker", Isolation: true,
		CanFetchTemplates: true, LastSeen: seen,
	}
}

func hasLevel(got []rt.IsolationLevel, want rt.IsolationLevel) bool {
	for _, l := range got {
		if l == want {
			return true
		}
	}
	return false
}

func TestFleetReportsEveryLevelItHas(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	got := aggregateCapabilities(
		[]*cluster.Node{dockerNode("d1", now), fcNode("f1", now)}, now, capsTTL)

	want := []rt.IsolationLevel{rt.IsolationStrong, rt.IsolationShared}
	if len(got.IsolationLevels) != len(want) {
		t.Fatalf("isolation_levels = %v, want %v", got.IsolationLevels, want)
	}
	for i := range want {
		if got.IsolationLevels[i] != want[i] {
			t.Errorf("isolation_levels[%d] = %q, want %q (strongest first — the list is a "+
				"ranking, so its order is part of the answer)", i, got.IsolationLevels[i], want[i])
		}
	}
	if len(got.Runtimes) != 2 || got.Runtimes[0] != "docker" || got.Runtimes[1] != "firecracker" {
		t.Errorf("runtimes = %v, want [docker firecracker]", got.Runtimes)
	}
	if len(got.Templates) != 2 || got.Templates[0] != "base" {
		t.Errorf("templates = %v, want [base code-interpreter]", got.Templates)
	}
	if !got.CanFetchTemplates {
		t.Error("can_fetch_templates = false, but the docker node advertises a shared artifact store")
	}
	if got.AutoPause {
		t.Error("auto_pause = true, but neither node heartbeats auto_pause")
	}
}

// The fleet bit is the AND over counted nodes: one node that never pauses
// makes the promise false, and a fleet of pausing nodes makes it true.
func TestFleetAutoPauseIsTheAndOverNodes(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	f1, f2 := fcNode("f1", now), fcNode("f2", now)
	f1.AutoPause, f2.AutoPause = true, true
	if got := aggregateCapabilities([]*cluster.Node{f1, f2}, now, capsTTL); !got.AutoPause {
		t.Error("auto_pause = false with every node pausing")
	}
	f2.AutoPause = false
	if got := aggregateCapabilities([]*cluster.Node{f1, f2}, now, capsTTL); got.AutoPause {
		t.Error("auto_pause = true with a node that never pauses")
	}
	// A stale node does not count either way.
	f2.LastSeen = now.Add(-2 * capsTTL)
	if got := aggregateCapabilities([]*cluster.Node{f1, f2}, now, capsTTL); !got.AutoPause {
		t.Error("auto_pause = false because of a node that is not counted")
	}
	if got := aggregateCapabilities(nil, now, capsTTL); got.AutoPause {
		t.Error("auto_pause = true for an empty fleet")
	}
}

// The only case that goes red if aggregateCapabilities filters on Schedulable
// instead of Healthy.
func TestFleetCountsCordonedNodesAsCapable(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fc := fcNode("f1", now)
	fc.Cordoned = true

	got := aggregateCapabilities([]*cluster.Node{dockerNode("d1", now), fc}, now, capsTTL)

	if !hasLevel(got.IsolationLevels, rt.IsolationStrong) {
		t.Errorf("isolation_levels = %v, want strong to still be offered by a cordoned "+
			"firecracker node.\n\nThis is D3. Cordon means \"place nothing new here\"; it "+
			"does not mean the machine stopped being a KVM host. An aggregate that filters "+
			"on cluster.Node.Schedulable (= Healthy && !Cordoned && !Draining) reports the "+
			"fleet losing a capability every time an operator drains a node.",
			got.IsolationLevels)
	}
	if got.Nodes.Counted != 2 {
		t.Errorf("nodes.counted = %d, want 2 (both are healthy)", got.Nodes.Counted)
	}
	if got.Nodes.Schedulable != 1 {
		t.Errorf("nodes.schedulable = %d, want 1 (the cordoned node takes no placement)",
			got.Nodes.Schedulable)
	}
}

func TestFleetDrainingNodeIsAlsoStillCapable(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	fc := fcNode("f1", now)
	fc.Draining = true

	got := aggregateCapabilities([]*cluster.Node{dockerNode("d1", now), fc}, now, capsTTL)
	if !hasLevel(got.IsolationLevels, rt.IsolationStrong) {
		t.Errorf("isolation_levels = %v, want strong — a draining node still has its own kernel",
			got.IsolationLevels)
	}
	if got.Nodes.Schedulable != 1 {
		t.Errorf("nodes.schedulable = %d, want 1", got.Nodes.Schedulable)
	}
}

func TestFleetWithoutTheNodeDoesNotOfferItsLevel(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	got := aggregateCapabilities([]*cluster.Node{dockerNode("d1", now)}, now, capsTTL)

	if hasLevel(got.IsolationLevels, rt.IsolationStrong) {
		t.Errorf("isolation_levels = %v, want no strong — no node in this fleet has its own kernel",
			got.IsolationLevels)
	}
	if !hasLevel(got.IsolationLevels, rt.IsolationShared) {
		t.Errorf("isolation_levels = %v, want shared", got.IsolationLevels)
	}
	if got.Nodes.Total != 1 || got.Nodes.Counted != 1 {
		t.Errorf("nodes = %+v, want total 1 counted 1", got.Nodes)
	}
}

// Every other case uses fresh heartbeats, so only this one exercises the Healthy filter.
func TestFleetUnreachableNodeDoesNotOfferItsLevel(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	stale := fcNode("f1", now.Add(-10*capsTTL))

	got := aggregateCapabilities([]*cluster.Node{dockerNode("d1", now), stale}, now, capsTTL)
	if hasLevel(got.IsolationLevels, rt.IsolationStrong) {
		t.Errorf("isolation_levels = %v, want no strong — the only firecracker node stopped "+
			"heartbeating %v ago, so this tier cannot reach it", got.IsolationLevels, 10*capsTTL)
	}
	if got.Nodes.Total != 2 || got.Nodes.Counted != 1 {
		t.Errorf("nodes = %+v, want total 2 counted 1 — an unreachable node is still in the "+
			"registry, and saying so is how an operator sees why the level went away", got.Nodes)
	}
}

// Empty arrays, not nulls: a client reading null as absent would fall back to a default.
func TestFleetEmptyReportsNoCapability(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	got := aggregateCapabilities(nil, now, capsTTL)
	if len(got.IsolationLevels) != 0 || len(got.Runtimes) != 0 {
		t.Fatalf("empty fleet reported %+v", got)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"isolation_levels", "runtimes", "templates"} {
		if string(raw[f]) != "[]" {
			t.Errorf("%s marshalled to %s, want [] — null reads as an absent field", f, raw[f])
		}
	}
}

// LevelFor's unknown case answers shared, which is wrong for a missing field.
func TestFleetIgnoresANodeWithNoRuntime(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	blank := &cluster.Node{ID: "n", URL: "http://n", LastSeen: now}

	got := aggregateCapabilities([]*cluster.Node{blank}, now, capsTTL)
	if len(got.IsolationLevels) != 0 {
		t.Errorf("isolation_levels = %v, want none — this node never said what it runs",
			got.IsolationLevels)
	}
	if got.Nodes.Counted != 1 {
		t.Errorf("nodes.counted = %d, want 1 — it is still a healthy member of the fleet",
			got.Nodes.Counted)
	}
}

// The node stub returns a valid node-scoped reply, so falling through to the
// /v1/ catch-all proxy shows up as a wrong answer rather than an error.
func TestFleetCapabilitiesIsNotProxiedToANode(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"scope":"node","runtime":"docker","isolation_level":"shared"}`)
	}))
	defer node.Close()

	s := NewServer("k", "t")
	now := time.Now()
	upsert(s.reg, "node-a", node.URL, "kk", "", nil, "", cluster.Capacity{}, now)
	s.reg.SetNodeMeta("node-a", cluster.NodeMeta{Runtime: "firecracker", Isolation: true, Templates: []string{"base"}})

	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/capabilities", nil)
	req.Header.Set("Authorization", "Bearer k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /v1/capabilities = %d, want 200", resp.StatusCode)
	}

	var got fleetCapabilities
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Scope != "fleet" {
		t.Errorf("scope = %q, want \"fleet\". The control plane answered with a NODE document, "+
			"which means the request fell through to the /v1/ catch-all proxy instead of "+
			"reaching handleCapabilities", got.Scope)
	}
	if !hasLevel(got.IsolationLevels, rt.IsolationStrong) {
		t.Errorf("isolation_levels = %v, want strong — the registered node reports firecracker, "+
			"whatever the stub's own body says", got.IsolationLevels)
	}
}

func TestFleetCapabilitiesRequiresAuth(t *testing.T) {
	s := NewServer("k", "t")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous GET /v1/capabilities = %d, want 401", resp.StatusCode)
	}
}
