package nodeboot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/artifact/cache"
	"github.com/pilldrops-oss/pilldrops/internal/artifact/header"
	"github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

type fakeIndex map[string]string

func (f fakeIndex) ReadyBuildForTemplate(_ context.Context, templateID string) (string, error) {
	if b, ok := f[templateID]; ok {
		return b, nil
	}
	return "", fmt.Errorf("%w: %s", store.ErrNoReadyBuild, templateID)
}

// publishRootfs writes a rootfs and its header; the cache refuses an artifact without one.
func publishRootfs(t *testing.T, remote artifact.StorageProvider, buildID string, body []byte) {
	t.Helper()
	ctx := t.Context()
	key, err := artifact.Key(buildID, artifact.NameRootfs)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Put(ctx, key, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	h, err := header.New(buildID, 4096, uint64(len(body)), sha256.Sum256(body), 0)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := h.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	hkey, err := artifact.HeaderKey(buildID, artifact.NameRootfs)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Put(ctx, hkey, bytes.NewReader(raw), int64(len(raw))); err != nil {
		t.Fatal(err)
	}
}

// fixture installs a fetcher as the process-wide template source for one test.
type fixture struct {
	remote   artifact.StorageProvider
	storeDir string
	cacheDir string
	imageDir string
	fetcher  *TemplateFetcher
}

func newFixture(t *testing.T, index BuildIndex, opts cache.Options) *fixture {
	t.Helper()
	root := t.TempDir()
	fx := &fixture{
		storeDir: filepath.Join(root, "artifacts"),
		cacheDir: filepath.Join(root, "artifact-cache"),
		imageDir: filepath.Join(root, "templates"),
	}
	for _, d := range []string{fx.storeDir, fx.cacheDir, fx.imageDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	remote, err := artifact.NewFS(fx.storeDir)
	if err != nil {
		t.Fatal(err)
	}
	fx.remote = remote
	f, err := NewTemplateFetcher(remote, fx.cacheDir, index, opts)
	if err != nil {
		t.Fatal(err)
	}
	fx.fetcher = f

	// Both injection points are process-wide; restore both or they leak.
	templates.SetDir(fx.imageDir)
	templates.SetSource(f.Fetch)
	t.Cleanup(func() {
		templates.SetSource(nil)
		templates.SetDir("")
	})
	return fx
}

func (fx *fixture) cachePath(buildID string) string {
	return filepath.Join(fx.cacheDir, buildID, artifact.NameRootfs)
}

func (fx *fixture) imagePath(templateID string) string {
	return filepath.Join(fx.imageDir, templateID, "rootfs.squashfs")
}

// Hardlink, not copy: a copy doubles disk use and the cache pin protects only one of them.
func TestColdTemplateIsFetchedAndHardlinkedIntoTheImageDirectory(t *testing.T) {
	payload := bytes.Repeat([]byte("squashfs"), 4096)
	fx := newFixture(t, fakeIndex{"base": "bld-base-1"}, cache.Options{})
	publishRootfs(t, fx.remote, "bld-base-1", payload)

	if _, err := os.Stat(fx.imagePath("base")); err == nil {
		t.Fatal("precondition: the image directory already holds base, so a fetch proves nothing")
	}

	tmpl, stat, err := templates.ResolveFetchTimed(t.Context(), "base")
	if err != nil {
		t.Fatalf("a cold create could not resolve its template: %v", err)
	}
	if tmpl.ID != "base" {
		t.Fatalf("resolved %q, want base", tmpl.ID)
	}
	if !stat.Fetched {
		t.Error("the create was not reported as having fetched — the fetch is on the create's " +
			"critical path and an operator cannot tell a slow download from a slow boot " +
			"without it")
	}
	if stat.Duration <= 0 {
		t.Errorf("fetch duration reported as %v; a fetch that took no time did not happen", stat.Duration)
	}

	got, err := os.ReadFile(fx.imagePath("base"))
	if err != nil {
		t.Fatalf("the fetch left nothing bootable in the image directory: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("the image directory holds different bytes from the ones that were published")
	}

	// One inode, two names.
	imgInfo, err := os.Stat(fx.imagePath("base"))
	if err != nil {
		t.Fatal(err)
	}
	cacheInfo, err := os.Stat(fx.cachePath("bld-base-1"))
	if err != nil {
		t.Fatalf("the cache does not hold the artifact it just fetched: %v", err)
	}
	if !os.SameFile(imgInfo, cacheInfo) {
		t.Errorf("the image directory holds a COPY of the artifact, not a hardlink to it — "+
			"%d bytes are on this disk twice, under two eviction regimes, and the cache's "+
			"pin protects only one of them", imgInfo.Size())
	}

	// A second create of the same template is a cache hit, not a second download.
	before := fx.fetcher.Cache().Stats()
	if _, err := templates.ResolveFetch(t.Context(), "base"); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if after := fx.fetcher.Cache().Stats(); after.Misses != before.Misses {
		t.Errorf("a warm create went back to the store (misses %d -> %d)", before.Misses, after.Misses)
	}
}

// An unpublished template falls through to the compiled-in catalog.
func TestUnpublishedTemplateFallsThroughRatherThanFailing(t *testing.T) {
	fx := newFixture(t, fakeIndex{}, cache.Options{})
	_ = fx

	tmpl, stat, err := templates.ResolveFetchTimed(t.Context(), "desktop")
	if err != nil {
		t.Fatalf("a compiled-in template stopped resolving once the store tier was installed: %v", err)
	}
	if tmpl.ID != "desktop" {
		t.Fatalf("resolved %q, want desktop", tmpl.ID)
	}
	if stat.Fetched {
		t.Error("a template the store does not have was reported as fetched")
	}
}

// An unreachable store must be neither the caller's 400 (unknown template) nor a timeout.
func TestUnreachableStoreIsAFetchFailureNotAnUnknownTemplate(t *testing.T) {
	// Real transport to a closed port, so the failure is the one production sees.
	remote, err := artifact.Open(artifact.KindControlPlane, artifact.Options{
		BaseURL: "http://127.0.0.1:1", Credential: "node-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	imageDir := filepath.Join(root, "templates")
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := NewTemplateFetcher(remote, filepath.Join(root, "cache"),
		fakeIndex{"base": "bld-base-1"}, cache.Options{})
	if err != nil {
		t.Fatal(err)
	}
	templates.SetDir(imageDir)
	templates.SetSource(f.Fetch)
	t.Cleanup(func() { templates.SetSource(nil); templates.SetDir("") })

	_, err = templates.ResolveFetch(t.Context(), "base")
	if err == nil {
		t.Fatal("a create against an unreachable store succeeded — it fell through to the " +
			"compiled-in catalog and will now 503 at boot with no rootfs on disk")
	}
	if !errors.Is(err, templates.ErrFetchFailed) {
		t.Errorf("got %v, want ErrFetchFailed", err)
	}
	if errors.Is(err, templates.ErrUnknownTemplate) {
		t.Error("an unreachable store was reported as an unknown template — the caller gets a " +
			"400 naming their own template id and the operator never learns the store is down")
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Errorf("an unreachable store was reported as a timeout: %v", err)
	}
	// The message must name the store so an operator can read it.
	if !strings.Contains(err.Error(), "artifact store") {
		t.Errorf("the fetch failure does not name the artifact store: %v", err)
	}
}

// An unreadable index is an outage, not an unknown template.
func TestIndexFailureIsAFetchFailure(t *testing.T) {
	fx := newFixture(t, brokenIndex{}, cache.Options{})
	_ = fx

	_, err := templates.ResolveFetch(t.Context(), "base")
	if !errors.Is(err, templates.ErrFetchFailed) {
		t.Errorf("an unreadable artifact index gave %v, want ErrFetchFailed", err)
	}
}

type brokenIndex struct{}

func (brokenIndex) ReadyBuildForTemplate(context.Context, string) (string, error) {
	return "", errors.New("connection refused")
}

// Eviction cannot see which files a running VM has open; the pin is what protects its rootfs.
// Asserts the node can still produce that rootfs after the store is gone.
func TestPinnedArtifactSurvivesEvictionPressure(t *testing.T) {
	const size = 64 << 10
	payload := bytes.Repeat([]byte("p"), size)

	// Room for two artifacts. The third fetch puts the cache over.
	fx := newFixture(t, fakeIndex{
		"pinned":   "bld-pinned",
		"filler-1": "bld-f1",
		"filler-2": "bld-f2",
		"filler-3": "bld-f3",
		"filler-4": "bld-f4",
	}, cache.Options{MaxBytes: 2 * size})

	for _, b := range []string{"bld-pinned", "bld-f1", "bld-f2", "bld-f3", "bld-f4"} {
		publishRootfs(t, fx.remote, b, payload)
	}

	// Fetch then pin, as daemon.Create does: Pin only holds what is already resident.
	if err := fx.fetcher.Fetch(t.Context(), "pinned", fx.imageDir); err != nil {
		t.Fatalf("fetch the sandbox's template: %v", err)
	}
	release := fx.fetcher.Pin("pinned")
	t.Cleanup(release)

	for i := 1; i <= 4; i++ {
		id := fmt.Sprintf("filler-%d", i)
		if err := fx.fetcher.Fetch(t.Context(), id, fx.imageDir); err != nil {
			t.Fatalf("fetch %s: %v", id, err)
		}
	}
	if ev := fx.fetcher.Cache().Stats().Evictions; ev == 0 {
		t.Fatal("nothing was evicted, so the cache never went over its high-water mark — " +
			"this test would pass with no pinning at all")
	}

	// Remove the store so only locally cached bytes can serve.
	if err := os.RemoveAll(fx.storeDir); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(fx.cachePath("bld-pinned")); err != nil {
		t.Errorf("the running sandbox's artifact was evicted from the cache under pressure: %v", err)
	}
	p, err := fx.fetcher.Cache().Path(t.Context(), "bld-pinned", artifact.NameRootfs)
	if err != nil {
		t.Fatalf("this node can no longer produce the running sandbox's rootfs: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("the pinned artifact is present but wrong (err %v)", err)
	}

}

// Otherwise a node's disk fills with the template of every sandbox it has ever run.
func TestReleasedPinMakesTheArtifactEvictableAgain(t *testing.T) {
	const size = 64 << 10
	payload := bytes.Repeat([]byte("p"), size)
	fx := newFixture(t, fakeIndex{"gone": "bld-gone", "filler-1": "bld-f1", "filler-2": "bld-f2", "filler-3": "bld-f3"},
		cache.Options{MaxBytes: 2 * size})
	for _, b := range []string{"bld-gone", "bld-f1", "bld-f2", "bld-f3"} {
		publishRootfs(t, fx.remote, b, payload)
	}
	if err := fx.fetcher.Fetch(t.Context(), "gone", fx.imageDir); err != nil {
		t.Fatal(err)
	}
	release := fx.fetcher.Pin("gone")
	release() // the sandbox was destroyed

	for _, id := range []string{"filler-1", "filler-2", "filler-3"} {
		if err := fx.fetcher.Fetch(t.Context(), id, fx.imageDir); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(fx.cachePath("bld-gone")); err == nil {
		t.Error("an unpinned artifact survived the high-water mark — the pin is never " +
			"released and the cache's bound means nothing after the first sandbox")
	}
}

func TestPinIsHeldUntilEverySandboxOfTheTemplateIsGone(t *testing.T) {
	const size = 64 << 10
	payload := bytes.Repeat([]byte("p"), size)
	fx := newFixture(t, fakeIndex{"shared": "bld-shared", "filler-1": "bld-f1", "filler-2": "bld-f2"},
		cache.Options{MaxBytes: 2 * size})
	for _, b := range []string{"bld-shared", "bld-f1", "bld-f2"} {
		publishRootfs(t, fx.remote, b, payload)
	}
	if err := fx.fetcher.Fetch(t.Context(), "shared", fx.imageDir); err != nil {
		t.Fatal(err)
	}
	first := fx.fetcher.Pin("shared")
	second := fx.fetcher.Pin("shared")
	first() // one sandbox destroyed; the other is still running

	for _, id := range []string{"filler-1", "filler-2"} {
		if err := fx.fetcher.Fetch(t.Context(), id, fx.imageDir); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(fx.cachePath("bld-shared")); err != nil {
		t.Errorf("destroying one of two sandboxes released the template's artifact: %v", err)
	}
	second()
}

// A locally built template has nothing cached to protect; Pin still returns a callable no-op.
func TestPinOfALocallyBuiltTemplateIsHarmless(t *testing.T) {
	fx := newFixture(t, fakeIndex{}, cache.Options{})
	release := fx.fetcher.Pin("never-fetched")
	if release == nil {
		t.Fatal("Pin returned nil; every caller would need a nil check it cannot be trusted to write")
	}
	release()
	release() // releasing twice must be safe: destroy can be reached twice
}

// The probe must test the real filesystem; defaults sharing a volume says nothing about a deployment that split them.
func TestLinkProbeAcceptsOneFilesystemAndNamesTheSettings(t *testing.T) {
	root := t.TempDir()
	a, i := filepath.Join(root, "artifact-cache"), filepath.Join(root, "templates")
	if err := LinkProbeError(a, i); err != nil {
		t.Fatalf("two directories on one filesystem were refused: %v", err)
	}
	// Both were created by the probe, and neither is left holding the probe file.
	for _, d := range []string{a, i} {
		entries, err := os.ReadDir(d)
		if err != nil {
			t.Fatalf("the probe did not create %s: %v", d, err)
		}
		if len(entries) != 0 {
			t.Errorf("the probe left %d file(s) behind in %s", len(entries), d)
		}
	}
	// A node with no image directory (docker/process) has nothing to link.
	if err := LinkProbeError(a, ""); err != nil {
		t.Errorf("a node with no image directory was refused: %v", err)
	}
}
