package daemon

import (
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
)

// restoreWith boots a daemon owned by nodeID against preloaded records.
func restoreWith(t *testing.T, nodeID string, recs []PersistedSandbox) *fakePersister {
	t.Helper()
	fp := newFakePersister(recs)
	d := New(Config{
		Runtime: runtimetest.NewFake(), Store: fp, NodeID: nodeID,
		SnapshotDir: t.TempDir(),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	d.Start()
	t.Cleanup(d.Stop)
	return fp
}

// unownedRec has no node stamped on it, like rows written before ownership existed.
func unownedRec(id string) PersistedSandbox {
	return runningRec(id, time.Now().Add(time.Hour))
}

func ownedRec(id, nodeID string) PersistedSandbox {
	r := unownedRec(id)
	r.NodeID = nodeID
	return r
}

// On a shared database an unowned record is usually another node's live sandbox.
func TestRestoreDoesNotDeleteAnUnownedRecord(t *testing.T) {
	fp := restoreWith(t, "node-1", []PersistedSandbox{unownedRec("legacy")})
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if fp.deleted["legacy"] {
		t.Error("node-1 deleted an unowned record it merely could not adopt — " +
			"on a shared database that is another node's live sandbox")
	}
}

func TestRestoreStillDeletesItsOwnUnrestorableRecord(t *testing.T) {
	fp := restoreWith(t, "node-1", []PersistedSandbox{ownedRec("mine", "node-1")})
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if !fp.deleted["mine"] {
		t.Error("node-1 kept its own unrestorable record, which leaks a ghost row per restart")
	}
}

// An unclustered daemon (empty NodeID) owns every record.
func TestRestoreUnclusteredStillDropsUnrestorableRecords(t *testing.T) {
	fp := restoreWith(t, "", []PersistedSandbox{unownedRec("orphan")})
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if !fp.deleted["orphan"] {
		t.Error("an unclustered daemon must still drop unrestorable records — " +
			"there is no other node for them to belong to")
	}
}
