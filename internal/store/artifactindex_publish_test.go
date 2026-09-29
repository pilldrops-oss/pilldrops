package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
)

// The publish path's store methods, tested for what they refuse.
// artifactindex_test.go drives the CHECK constraints with raw SQL.

func TestOnlyPublishedFactsCanMarkABuildReady(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()

	if err := st.BeginTemplateBuild(ctx, TemplateBuild{
		BuildID: "bld-facts", TemplateID: "base", CreatedAt: 100,
	}); err != nil {
		t.Fatalf("begin: %v", err)
	}
	b, err := st.TemplateBuildByID(ctx, "bld-facts")
	if err != nil || b == nil {
		t.Fatalf("read back: %v", err)
	}
	if b.Ready() || b.ReadyAt != 0 {
		t.Fatalf("BeginTemplateBuild produced status=%q ready_at=%d — it must be incapable of "+
			"creating a bootable build, whatever it is passed", b.Status, b.ReadyAt)
	}

	for _, tc := range []struct {
		name string
		in   TemplateBuild
	}{
		{"no size and no checksum", TemplateBuild{BuildID: "bld-facts", ReadyAt: 200}},
		{"a checksum but no size", TemplateBuild{BuildID: "bld-facts", ReadyAt: 200, RootfsSHA256: hex64}},
		{"a size but no checksum", TemplateBuild{BuildID: "bld-facts", ReadyAt: 200, RootfsSize: 4096}},
		{"a checksum of the wrong shape", TemplateBuild{
			BuildID: "bld-facts", ReadyAt: 200, RootfsSize: 4096, RootfsSHA256: "deadbeef"}},
		{"no ready timestamp", TemplateBuild{
			BuildID: "bld-facts", RootfsSize: 4096, RootfsSHA256: hex64}},
	} {
		if err := st.MarkTemplateBuildReady(ctx, tc.in); err == nil {
			t.Errorf("marked a build ready with %s — an interrupted publish could then leave a ready "+
				"row whose bytes were never written", tc.name)
		}
	}

	if err := st.MarkTemplateBuildReady(ctx, TemplateBuild{
		BuildID: "bld-facts", ReadyAt: 200, RootfsSize: 4096, RootfsSHA256: hex64,
		TotalSize: 4600, KernelVersion: "6.1.0", EnvdVersion: "v9",
	}); err != nil {
		t.Fatalf("a fully published build was refused, so the guard is too strict to use: %v", err)
	}
	b, err = st.TemplateBuildByID(ctx, "bld-facts")
	if err != nil || b == nil {
		t.Fatalf("read back: %v", err)
	}
	if !b.Ready() || b.ReadyAt != 200 || b.KernelVersion != "6.1.0" || b.EnvdVersion != "v9" {
		t.Errorf("after the ready mark: %+v", *b)
	}
}

func TestOnlyABuildingBuildCanBeMarkedReady(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()
	ready := func(id string) TemplateBuild {
		return TemplateBuild{BuildID: id, ReadyAt: 200, RootfsSize: 4096, RootfsSHA256: hex64}
	}

	err := st.MarkTemplateBuildReady(ctx, ready("bld-never-begun"))
	if !errors.Is(err, ErrBuildNotBuilding) {
		t.Errorf("marking a nonexistent build ready = %v, want ErrBuildNotBuilding — a caller could "+
			"otherwise publish a build row it never created", err)
	}

	// The second mark is a second publisher on the same build id.
	if err := st.BeginTemplateBuild(ctx, TemplateBuild{BuildID: "bld-twice", TemplateID: "base", CreatedAt: 100}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTemplateBuildReady(ctx, ready("bld-twice")); err != nil {
		t.Fatalf("first mark: %v", err)
	}
	if err := st.MarkTemplateBuildReady(ctx, ready("bld-twice")); !errors.Is(err, ErrBuildNotBuilding) {
		t.Errorf("marking an already-ready build ready again = %v, want ErrBuildNotBuilding", err)
	}

	if err := st.BeginTemplateBuild(ctx, TemplateBuild{BuildID: "bld-dead", TemplateID: "base", CreatedAt: 100}); err != nil {
		t.Fatal(err)
	}
	if err := st.FailTemplateBuild(ctx, "bld-dead"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if err := st.MarkTemplateBuildReady(ctx, ready("bld-dead")); !errors.Is(err, ErrBuildNotBuilding) {
		t.Errorf("marking a FAILED build ready = %v, want ErrBuildNotBuilding — the objects a failed "+
			"publish left behind are exactly the ones nothing verified", err)
	}
}

func TestFailTemplateBuildNeverUnreadiesALiveBuild(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()

	if err := st.BeginTemplateBuild(ctx, TemplateBuild{BuildID: "bld-live", TemplateID: "base", CreatedAt: 100}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkTemplateBuildReady(ctx, TemplateBuild{
		BuildID: "bld-live", ReadyAt: 200, RootfsSize: 4096, RootfsSHA256: hex64}); err != nil {
		t.Fatal(err)
	}
	if err := st.FailTemplateBuild(ctx, "bld-live"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	b, err := st.TemplateBuildByID(ctx, "bld-live")
	if err != nil || b == nil {
		t.Fatal(err)
	}
	if !b.Ready() {
		t.Errorf("FailTemplateBuild un-readied a published build (status=%q) — a late tidy-up from a "+
			"retried publish would then pull a live template out of the fleet", b.Status)
	}
}

func TestRecordArtifactObjectDescribesTheBytesThatAreThereNow(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()
	key, err := artifact.Key("bld-upsert", artifact.NameRootfs)
	if err != nil {
		t.Fatal(err)
	}
	newSum := strings.Repeat("a", 64)

	for _, a := range []ArtifactObject{
		{BuildID: "bld-upsert", Kind: artifact.NameRootfs, Key: key, Size: 4096,
			SHA256: hex64, StoreKind: string(artifact.KindFS), UploadedAt: 300},
		{BuildID: "bld-upsert", Kind: artifact.NameRootfs, Key: key, Size: 8192,
			SHA256: newSum, StoreKind: string(artifact.KindControlPlane), UploadedAt: 400},
	} {
		if err := st.RecordArtifactObject(ctx, a); err != nil {
			t.Fatalf("record %d: %v", a.Size, err)
		}
	}

	rows, err := st.ArtifactObjects(ctx, "bld-upsert")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d rows for one (build, kind), want 1: %+v", len(rows), rows)
	}
	if rows[0].Size != 8192 || rows[0].SHA256 != newSum || rows[0].StoreKind != string(artifact.KindControlPlane) {
		t.Errorf("the row still describes the FIRST upload (%+v) — a reader would verify a fetch "+
			"against a checksum the stored object no longer has", rows[0])
	}

	// StoreKind is required; there is no safe default.
	if err := st.RecordArtifactObject(ctx, ArtifactObject{
		BuildID: "bld-upsert", Kind: artifact.NameMetadata,
		Key: "bld-upsert/" + artifact.NameMetadata, Size: 12, SHA256: hex64, UploadedAt: 300,
	}); err == nil {
		t.Error("recorded an artifact that does not say which store holds the bytes")
	}
}

func TestTemplateBuildsIsScopedAndOrdered(t *testing.T) {
	st := openTemp(t)
	ctx := t.Context()

	mk := func(id, org string, created int64, ready bool) {
		t.Helper()
		if err := st.BeginTemplateBuild(ctx, TemplateBuild{
			BuildID: id, TemplateID: "shared", OrgID: org, CreatedAt: created}); err != nil {
			t.Fatal(err)
		}
		if ready {
			if err := st.MarkTemplateBuildReady(ctx, TemplateBuild{
				BuildID: id, ReadyAt: created + 10, RootfsSize: 1, RootfsSHA256: hex64}); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("bld-acme-old", "acme", 100, true)
	mk("bld-acme-new", "acme", 300, true)
	mk("bld-acme-building", "acme", 400, false)
	mk("bld-globex", "globex", 200, true)

	all, err := st.TemplateBuilds(ctx, "shared", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Errorf("unscoped list returned %d builds, want 4", len(all))
	}
	// Newest ready first, matching what a node would resolve to.
	var firstReady string
	for _, b := range all {
		if b.Ready() {
			firstReady = b.BuildID
			break
		}
	}
	if firstReady != "bld-acme-new" {
		t.Errorf("the newest ready build is %q, want bld-acme-new — an operator and a node reading "+
			"the same rows would resolve differently", firstReady)
	}

	acme, err := st.TemplateBuilds(ctx, "shared", "acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(acme) != 3 {
		t.Fatalf("org-scoped list returned %d builds, want 3: %+v", len(acme), acme)
	}
	for _, b := range acme {
		if b.OrgID != "acme" {
			t.Errorf("org-scoped list leaked %q's build %q", b.OrgID, b.BuildID)
		}
	}

	// An unpublished template is an empty list, not an error.
	none, err := st.TemplateBuilds(ctx, "never-published", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("an unpublished template returned %d builds", len(none))
	}
}
