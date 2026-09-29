package store

import (
	"errors"
	"strings"
	"testing"
)

// readyBuild publishes a minimal ready build under templateID.
func readyBuild(t *testing.T, st *Store, buildID, templateID string, at int64) {
	t.Helper()
	ctx := t.Context()
	if err := st.BeginTemplateBuild(ctx, TemplateBuild{
		BuildID: buildID, TemplateID: templateID, OrgID: "acme", CreatedAt: at,
	}); err != nil {
		t.Fatalf("begin build %s: %v", buildID, err)
	}
	if err := st.MarkTemplateBuildReady(ctx, TemplateBuild{
		BuildID: buildID, ReadyAt: at, RootfsSize: 4096, RootfsSHA256: hex64, TotalSize: 4096,
	}); err != nil {
		t.Fatalf("mark %s ready: %v", buildID, err)
	}
}

func TestLatestReadySnapshotIgnoresAVersionStillBuilding(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()
	tmpl := SnapshotTemplateID("acme", "golden")

	// Version 1: published and ready. The registry row goes first — it is what
	// assigns the build id, so that the id sorts in version order.
	v1 := Snapshot{OrgID: "acme", Name: "golden",
		Runtime: "firecracker", Template: "base", VCPU: 2, MemMB: 2048,
		SourceSandbox: "sb-a", CreatedAt: 1000}
	if err := st.SaveSnapshot(ctx, &v1); err != nil {
		t.Fatalf("save v1: %v", err)
	}
	if v1.Version != 1 {
		t.Fatalf("first save is version %d, want 1", v1.Version)
	}
	readyBuild(t, st, v1.BuildID, tmpl, 1000)

	// Version 2: the row exists and the build is still building.
	v2 := Snapshot{OrgID: "acme", Name: "golden",
		Runtime: "firecracker", Template: "base", VCPU: 2, MemMB: 2048,
		SourceSandbox: "sb-a", CreatedAt: 2000}
	if err := st.SaveSnapshot(ctx, &v2); err != nil {
		t.Fatalf("save v2: %v", err)
	}
	if v2.Version != 2 {
		t.Fatalf("second save is version %d, want 2", v2.Version)
	}
	if err := st.BeginTemplateBuild(ctx, TemplateBuild{
		BuildID: v2.BuildID, TemplateID: tmpl, OrgID: "acme", CreatedAt: 2000,
	}); err != nil {
		t.Fatalf("begin v2: %v", err)
	}

	got, err := st.LatestReadySnapshot(ctx, "acme", "golden")
	if err != nil {
		t.Fatalf("resolve while v2 builds: %v", err)
	}
	if got.Version != 1 || got.BuildID != v1.BuildID {
		t.Fatalf("resolved version %d (build %s) while v2 was still building; a create would boot "+
			"bytes that are not in the store yet", got.Version, got.BuildID)
	}
	// The resolution must agree with what the node will resolve independently.
	if b, err := st.ReadyBuildForTemplate(ctx, tmpl); err != nil || b != got.BuildID {
		t.Fatalf("the node resolves %q (err %v) and the control plane %q — they must agree or the "+
			"fleet boots a different version than it promised", b, err, got.BuildID)
	}

	// Mark v2 ready: the name now resolves to it.
	if err := st.MarkTemplateBuildReady(ctx, TemplateBuild{
		BuildID: v2.BuildID, ReadyAt: 2000, RootfsSize: 4096, RootfsSHA256: hex64, TotalSize: 4096,
	}); err != nil {
		t.Fatalf("mark v2 ready: %v", err)
	}
	got, err = st.LatestReadySnapshot(ctx, "acme", "golden")
	if err != nil {
		t.Fatalf("resolve after v2 is ready: %v", err)
	}
	if got.Version != 2 {
		t.Fatalf("resolved version %d after v2 became ready, want 2", got.Version)
	}
	if b, _ := st.ReadyBuildForTemplate(ctx, tmpl); b != got.BuildID {
		t.Fatalf("node resolves %q, control plane %q", b, got.BuildID)
	}

	// Purging v1 leaves the name resolving to v2 and the row readable as deleted.
	sup, err := st.SupersededSnapshots(ctx, "acme", "golden", v2.ID)
	if err != nil {
		t.Fatalf("list superseded: %v", err)
	}
	if len(sup) != 1 || sup[0].ID != v1.ID {
		t.Fatalf("superseded = %+v, want just v1", sup)
	}
	if _, err := st.DeleteBuild(ctx, sup[0].BuildID); err != nil {
		t.Fatalf("delete v1's build: %v", err)
	}
	if err := st.MarkSnapshotDeleted(ctx, sup[0].ID, 3000); err != nil {
		t.Fatalf("mark v1 deleted: %v", err)
	}
	versions, err := st.SnapshotVersions(ctx, "acme", "golden")
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 2 || versions[0].Status != "ready" || versions[1].Status != "deleted" {
		t.Fatalf("versions = %+v, want v2 ready and v1 deleted", versions)
	}
	if got, err := st.LatestReadySnapshot(ctx, "acme", "golden"); err != nil || got.Version != 2 {
		t.Fatalf("after purging v1 the name resolves to %+v (err %v), want version 2", got, err)
	}
}

// ready_at has one-second granularity, so two versions of one name saved inside
// the same second fall through to ReadyBuildForTemplate's build_id tie-break.
// With a random build id that is a coin toss, and the node then boots a
// different version than the control plane promised — seen on the reference
// node, where a green local suite had hidden it.
//
// Every version here is marked ready at the SAME timestamp, so the tie-break is
// the only thing deciding, and the two resolvers must still agree.
func TestVersionsReadyInTheSameSecondStillResolveToTheNewest(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()
	tmpl := SnapshotTemplateID("acme", "golden")
	const sameSecond = int64(1000)

	var last Snapshot
	for v := 1; v <= 6; v++ {
		snap := Snapshot{OrgID: "acme", Name: "golden", Runtime: "firecracker", CreatedAt: sameSecond}
		if err := st.SaveSnapshot(ctx, &snap); err != nil {
			t.Fatalf("save v%d: %v", v, err)
		}
		if snap.Version != v {
			t.Fatalf("version %d, want %d", snap.Version, v)
		}
		if err := st.BeginTemplateBuild(ctx, TemplateBuild{
			BuildID: snap.BuildID, TemplateID: tmpl, OrgID: "acme", CreatedAt: sameSecond,
		}); err != nil {
			t.Fatalf("begin v%d: %v", v, err)
		}
		if err := st.MarkTemplateBuildReady(ctx, TemplateBuild{
			BuildID: snap.BuildID, ReadyAt: sameSecond, RootfsSize: 4096,
			RootfsSHA256: hex64, TotalSize: 4096,
		}); err != nil {
			t.Fatalf("ready v%d: %v", v, err)
		}
		last = snap

		got, err := st.LatestReadySnapshot(ctx, "acme", "golden")
		if err != nil {
			t.Fatalf("resolve after v%d: %v", v, err)
		}
		if got.Version != v {
			t.Fatalf("with %d versions all ready at the same second, the control plane resolved "+
				"version %d", v, got.Version)
		}
		node, err := st.ReadyBuildForTemplate(ctx, tmpl)
		if err != nil {
			t.Fatalf("node resolve after v%d: %v", v, err)
		}
		if node != snap.BuildID {
			t.Fatalf("the node resolves %q and the control plane %q — the fleet would boot a "+
				"different version than it promised", node, snap.BuildID)
		}
	}
	if last.Version != 6 {
		t.Fatalf("ended at version %d", last.Version)
	}
}

func TestSnapshotsAreFencedToTheirOrg(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()
	snap := Snapshot{OrgID: "acme", Name: "golden", Runtime: "firecracker", CreatedAt: 1000}
	if err := st.SaveSnapshot(ctx, &snap); err != nil {
		t.Fatalf("save: %v", err)
	}
	readyBuild(t, st, snap.BuildID, SnapshotTemplateID("acme", "golden"), 1000)

	list, err := st.SnapshotsForOrg(ctx, "other")
	if err != nil {
		t.Fatalf("list for the other org: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("another org lists %d snapshots, want 0", len(list))
	}
	if _, err := st.LatestReadySnapshot(ctx, "other", "golden"); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("another org resolved the name (err %v), want ErrSnapshotNotFound", err)
	}
	if _, err := st.DeleteSnapshot(ctx, "other", "golden", 2000); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("another org deleted the name (err %v), want ErrSnapshotNotFound", err)
	}
	// The owner still has it: the refusals above were the fence, not an empty table.
	if _, err := st.LatestReadySnapshot(ctx, "acme", "golden"); err != nil {
		t.Fatalf("the owning org cannot resolve its own snapshot: %v", err)
	}
}

// Two orgs whose ids differ only where a '-' could be read as the separator
// must not share a template id, or one org boots the other's bytes.
func TestSnapshotTemplateIDsCannotCollideAcrossOrgs(t *testing.T) {
	a := SnapshotTemplateID("acme", "web-prod")
	b := SnapshotTemplateID("acme-web", "prod")
	if a == b {
		t.Fatalf("org acme/web-prod and org acme-web/prod both publish under %q", a)
	}
	for _, id := range []string{a, b} {
		if !strings.HasPrefix(id, "snap-") {
			t.Fatalf("%q does not carry the snap- prefix placement reads", id)
		}
		if err := ValidSnapshotName("golden"); err != nil {
			t.Fatalf("a legal name was refused: %v", err)
		}
	}
	for _, bad := range []string{"", "-lead", "Upper", "has_underscore", strings.Repeat("a", 41), "../escape"} {
		if err := ValidSnapshotName(bad); err == nil {
			t.Fatalf("name %q was accepted; it cannot be part of a template id", bad)
		}
	}
}
