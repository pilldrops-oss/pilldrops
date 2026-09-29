package nodeboot

import (
	"os"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/artifact/cache"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// The end-to-end shape of the silent-staleness bug, through the real fetcher:
// one template id, two builds, and the node already holding the first.
//
// Against the old code this fails on the byte comparison while every call
// returns nil — ResolveFetch stopped at the disk hit, so the second build was
// never fetched and the create booted the first one reporting success.
func TestASecondBuildOfOneTemplateReplacesWhatTheNodeHolds(t *testing.T) {
	const id = "snap-abcd-golden"
	index := fakeIndex{id: "bld-s0000000001-one"}
	fx := newFixture(t, index, cache.Options{})
	templates.SetCurrentBuild(fx.fetcher.CurrentBuild)
	t.Cleanup(func() { templates.SetCurrentBuild(nil) })

	publishRootfs(t, fx.remote, "bld-s0000000001-one", []byte("version-one"))
	if _, err := templates.ResolveFetch(t.Context(), id); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if got := templates.HeldBuild(id); got != "bld-s0000000001-one" {
		t.Fatalf("stamp after the first fetch = %q, want the first build", got)
	}

	// `snapshot save golden` a second time: a new build under the same id.
	publishRootfs(t, fx.remote, "bld-s0000000002-two", []byte("version-two"))
	index[id] = "bld-s0000000002-two"

	if _, err := templates.ResolveFetch(t.Context(), id); err != nil {
		t.Fatalf("fetch after the second save: %v", err)
	}
	got, err := os.ReadFile(fx.imagePath(id))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "version-two" {
		t.Fatalf("the image a create would boot holds %q, want %q — the node served the "+
			"previous version and the create reported success", got, "version-two")
	}
}

// A template the index has never published must not be read as "stale".
func TestCurrentBuildReportsNoOpinionForAnUnpublishedTemplate(t *testing.T) {
	fx := newFixture(t, fakeIndex{}, cache.Options{})
	build, err := fx.fetcher.CurrentBuild(t.Context(), "never-published")
	if err != nil {
		t.Fatalf("CurrentBuild returned %v; an id the index never heard of is not an outage, "+
			"and treating it as one would refuse every create for a baked-in template", err)
	}
	if build != "" {
		t.Fatalf("CurrentBuild = %q, want empty", build)
	}
}
