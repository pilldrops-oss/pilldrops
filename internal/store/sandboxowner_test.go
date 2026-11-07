package store

import "testing"

// saveOn persists a minimal running sandbox owned by nodeID ("" = unowned).
func saveOn(t *testing.T, st *Store, id, org, nodeID string) {
	t.Helper()
	const now = int64(5000)
	if err := st.SaveSandbox(t.Context(), SandboxRecord{
		ID: id, OrgID: org, NodeID: nodeID, Template: "python-3.11", State: "running",
		CreatedAt: now, ExpiresAt: now + 3600, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("save %s: %v", id, err)
	}
}

func idsOf(recs []SandboxRecord) map[string]string {
	out := map[string]string{}
	for _, r := range recs {
		out[r.ID] = r.NodeID
	}
	return out
}

func TestLoadSandboxesForNodeExcludesOtherNodes(t *testing.T) {
	st := openTemp(t)
	saveOn(t, st, "mine", "u:abc", "node-1")
	saveOn(t, st, "theirs", "u:abc", "node-2")

	got, err := st.LoadSandboxesForNode(t.Context(), "node-1")
	if err != nil {
		t.Fatalf("LoadSandboxesForNode: %v", err)
	}
	ids := idsOf(got)
	if _, ok := ids["mine"]; !ok {
		t.Errorf("node-1 cannot see its own sandbox; got %v", ids)
	}
	if _, ok := ids["theirs"]; ok {
		t.Errorf("node-1 can see node-2's sandbox — this is the cross-node delete/double-claim bug; got %v", ids)
	}
}

func TestLoadSandboxesForNodeIncludesUnownedRows(t *testing.T) {
	st := openTemp(t)
	saveOn(t, st, "legacy", "u:abc", "")
	saveOn(t, st, "theirs", "u:abc", "node-2")

	got, err := st.LoadSandboxesForNode(t.Context(), "node-1")
	if err != nil {
		t.Fatalf("LoadSandboxesForNode: %v", err)
	}
	ids := idsOf(got)
	if _, ok := ids["legacy"]; !ok {
		t.Errorf("an unowned row is invisible to node-1 — an upgrade would look like data loss; got %v", ids)
	}
	if _, ok := ids["theirs"]; ok {
		t.Errorf("node-2's row leaked into node-1's scope; got %v", ids)
	}
}

func TestLoadSandboxesForNodeUnclusteredSeesEverything(t *testing.T) {
	st := openTemp(t)
	saveOn(t, st, "a", "u:abc", "")
	saveOn(t, st, "b", "u:abc", "node-2")

	got, err := st.LoadSandboxesForNode(t.Context(), "")
	if err != nil {
		t.Fatalf("LoadSandboxesForNode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("an unclustered daemon should see every row, got %d: %v", len(got), idsOf(got))
	}
}

func TestSaveSandboxNeverUnownsARecord(t *testing.T) {
	st := openTemp(t)
	saveOn(t, st, "s1", "u:abc", "node-1")
	saveOn(t, st, "s1", "u:abc", "") // an update that forgot the id

	got, err := st.LoadSandboxesForNode(t.Context(), "node-2")
	if err != nil {
		t.Fatalf("LoadSandboxesForNode: %v", err)
	}
	if _, ok := idsOf(got)["s1"]; ok {
		t.Error("a save with an empty node id cleared the owner, making node-1's sandbox visible to node-2")
	}
}

func TestSaveSandboxClaimsAnUnownedRecord(t *testing.T) {
	st := openTemp(t)
	saveOn(t, st, "s1", "u:abc", "")
	saveOn(t, st, "s1", "u:abc", "node-1")

	got, err := st.LoadSandboxesForNode(t.Context(), "node-2")
	if err != nil {
		t.Fatalf("LoadSandboxesForNode: %v", err)
	}
	if _, ok := idsOf(got)["s1"]; ok {
		t.Error("node-1 claimed s1 but node-2 can still see it")
	}
}

func TestSandboxesByOrgSpansNodesAndExcludesOtherOrgs(t *testing.T) {
	st := openTemp(t)
	saveOn(t, st, "s1", "u:abc", "node-1")
	saveOn(t, st, "s2", "u:abc", "node-2")
	saveOn(t, st, "s3", "u:xyz", "node-1")

	got, err := st.SandboxesByOrg(t.Context(), "u:abc")
	if err != nil {
		t.Fatalf("SandboxesByOrg: %v", err)
	}
	ids := idsOf(got)
	if len(ids) != 2 || ids["s1"] != "node-1" || ids["s2"] != "node-2" {
		t.Fatalf("want u:abc's two sandboxes across both nodes, got %v", ids)
	}
}

// The control plane's row is a fallback for a node write that failed, never a
// second writer: a row the node already wrote keeps its own config.
func TestRecordPlacedSandboxFillsInButNeverOverwrites(t *testing.T) {
	st := openTemp(t)
	saveOn(t, st, "node-wrote", "u:abc", "node-1")
	placed := SandboxRecord{OrgID: "u:abc", NodeID: "node-2", Template: "base", State: "running",
		CreatedAt: 1, UpdatedAt: 1}
	for _, id := range []string{"node-wrote", "node-failed"} {
		placed.ID = id
		if err := st.RecordPlacedSandbox(t.Context(), placed); err != nil {
			t.Fatalf("record %s: %v", id, err)
		}
	}
	if rec, _, _ := st.SandboxByID(t.Context(), "node-wrote"); rec.NodeID != "node-1" || rec.Template != "python-3.11" {
		t.Errorf("the node's row was overwritten: %+v", rec)
	}
	if rec, ok, _ := st.SandboxByID(t.Context(), "node-failed"); !ok || rec.NodeID != "node-2" || rec.OrgID != "u:abc" {
		t.Errorf("no row filled in for a sandbox the node did not record: %+v ok=%v", rec, ok)
	}
}
