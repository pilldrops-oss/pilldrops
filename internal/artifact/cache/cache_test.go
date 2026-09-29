package cache_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/artifact/cache"
	"github.com/pilldrops-oss/pilldrops/internal/artifact/header"
)

// publish writes a data object and its header to a remote store.
func publish(t *testing.T, remote artifact.StorageProvider, buildID, name string, body []byte) {
	t.Helper()
	ctx := t.Context()

	key, err := artifact.Key(buildID, name)
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
	hkey, err := artifact.HeaderKey(buildID, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Put(ctx, hkey, bytes.NewReader(raw), int64(len(raw))); err != nil {
		t.Fatal(err)
	}
}

func newCache(t *testing.T, opts cache.Options) (*cache.Cache, artifact.StorageProvider) {
	t.Helper()
	remote, err := artifact.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.New(remote, t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return c, remote
}

func TestFetchesOnceAndServesFromDisk(t *testing.T) {
	t.Parallel()
	c, remote := newCache(t, cache.Options{})
	publish(t, remote, "bld-1", artifact.NameRootfs, []byte("template payload"))

	p, err := c.Path(t.Context(), "bld-1", artifact.NameRootfs)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil || string(got) != "template payload" {
		t.Fatalf("materialised file = %q, %v", got, err)
	}

	if _, err := c.Path(t.Context(), "bld-1", artifact.NameRootfs); err != nil {
		t.Fatal(err)
	}
	s := c.Stats()
	if s.Misses != 1 || s.Hits != 1 {
		t.Fatalf("hits=%d misses=%d; want 1 and 1 — the second call must not re-fetch", s.Hits, s.Misses)
	}
}

func TestCorruptedArtifactIsRejectedAndDiscarded(t *testing.T) {
	t.Parallel()
	c, remote := newCache(t, cache.Options{})
	publish(t, remote, "bld-1", artifact.NameRootfs, []byte("the real payload"))

	// Overwrite the data without updating the header.
	key, _ := artifact.Key("bld-1", artifact.NameRootfs)
	if err := remote.Put(t.Context(), key, strings.NewReader("TAMPERED"), 8); err != nil {
		t.Fatal(err)
	}

	_, err := c.Path(t.Context(), "bld-1", artifact.NameRootfs)
	if err == nil {
		t.Fatal("a checksum mismatch must fail the fetch")
	}
	if !strings.Contains(err.Error(), "verification") {
		t.Errorf("error should name verification, got: %v", err)
	}
	if entries, _ := filepath.Glob(filepath.Join(c.Dir(), "bld-1", "*")); len(entries) != 0 {
		t.Fatalf("a failed verification left files behind: %v", entries)
	}
}

func TestUnverifiableArtifactIsRefused(t *testing.T) {
	t.Parallel()
	c, remote := newCache(t, cache.Options{})
	key, _ := artifact.Key("bld-1", artifact.NameRootfs)
	if err := remote.Put(t.Context(), key, strings.NewReader("no header"), 9); err != nil {
		t.Fatal(err)
	}

	_, err := c.Path(t.Context(), "bld-1", artifact.NameRootfs)
	if err == nil {
		t.Fatal("an artifact with no header must be refused")
	}
	if !strings.Contains(err.Error(), "no header") {
		t.Errorf("error should say the header is missing, got: %v", err)
	}
}

// Evicting a pinned artifact would pull the rootfs out from under a live VM.
func TestPinnedArtifactSurvivesEvictionPressure(t *testing.T) {
	t.Parallel()
	body := bytes.Repeat([]byte("x"), 1000)
	c, remote := newCache(t, cache.Options{MaxBytes: 2500})

	publish(t, remote, "pinned", artifact.NameRootfs, body)
	pinnedPath, err := c.Path(t.Context(), "pinned", artifact.NameRootfs)
	if err != nil {
		t.Fatal(err)
	}
	release := c.Acquire("pinned")
	defer release()

	for _, id := range []string{"b1", "b2", "b3", "b4", "b5"} {
		publish(t, remote, id, artifact.NameRootfs, body)
		if _, err := c.Path(t.Context(), id, artifact.NameRootfs); err != nil {
			t.Fatalf("fetch %s: %v", id, err)
		}
	}

	if _, err := os.Stat(pinnedPath); err != nil {
		t.Fatalf("the pinned artifact was evicted under pressure: %v", err)
	}
	if c.Stats().Evictions == 0 {
		t.Fatal("nothing was evicted, so this did not exercise the pressure path")
	}

	release()
	publish(t, remote, "b6", artifact.NameRootfs, body)
	if _, err := c.Path(t.Context(), "b6", artifact.NameRootfs); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pinnedPath); err == nil {
		t.Log("note: the released artifact happened to survive; it is the most recently used")
	}
}

func TestEvictionKeepsResidentBytesUnderTheLimit(t *testing.T) {
	t.Parallel()
	body := bytes.Repeat([]byte("y"), 1000)
	c, remote := newCache(t, cache.Options{MaxBytes: 2500})

	for _, id := range []string{"b1", "b2", "b3", "b4", "b5"} {
		publish(t, remote, id, artifact.NameRootfs, body)
		if _, err := c.Path(t.Context(), id, artifact.NameRootfs); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.Stats().ResidentBytes; got > 2500 {
		t.Fatalf("resident bytes = %d, above the 2500 limit", got)
	}
}

func TestExpiredEntriesAreDroppedWithoutPressure(t *testing.T) {
	t.Parallel()
	now := time.Now()
	clock := func() time.Time { return now }

	remote, err := artifact.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.New(remote, t.TempDir(), cache.Options{TTL: time.Hour, Now: func() time.Time { return clock() }})
	if err != nil {
		t.Fatal(err)
	}

	publish(t, remote, "old", artifact.NameRootfs, []byte("old"))
	oldPath, err := c.Path(t.Context(), "old", artifact.NameRootfs)
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(2 * time.Hour)

	publish(t, remote, "new", artifact.NameRootfs, []byte("new"))
	if _, err := c.Path(t.Context(), "new", artifact.NameRootfs); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); err == nil {
		t.Fatal("an entry past its TTL should have been dropped")
	}
}

func TestReclaimRemovesFetchTemporaries(t *testing.T) {
	t.Parallel()
	c, remote := newCache(t, cache.Options{})
	publish(t, remote, "bld-1", artifact.NameRootfs, []byte("payload"))
	if _, err := c.Path(t.Context(), "bld-1", artifact.NameRootfs); err != nil {
		t.Fatal(err)
	}

	stray := filepath.Join(c.Dir(), "bld-1", ".fetch-rootfs-123456")
	if err := os.WriteFile(stray, []byte("half a template"), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := c.Reclaim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed %d, want 1", removed)
	}
	if _, err := os.Stat(stray); err == nil {
		t.Fatal("the fetch temporary survived reclaim")
	}
	if s := c.Stats(); s.Entries != 1 || s.ResidentBytes != int64(len("payload")) {
		t.Fatalf("after reclaim: entries=%d bytes=%d", s.Entries, s.ResidentBytes)
	}
}

func TestPathRefetchesWhenTheLocalFileVanishes(t *testing.T) {
	t.Parallel()
	c, remote := newCache(t, cache.Options{})
	publish(t, remote, "bld-1", artifact.NameRootfs, []byte("payload"))

	p, err := c.Path(t.Context(), "bld-1", artifact.NameRootfs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}

	p2, err := c.Path(t.Context(), "bld-1", artifact.NameRootfs)
	if err != nil {
		t.Fatalf("a vanished file should re-fetch: %v", err)
	}
	if got, _ := os.ReadFile(p2); string(got) != "payload" {
		t.Fatalf("re-fetched content = %q", got)
	}
}

// With the only other artifact pinned, the fresh fetch is the sole eviction
// candidate; Path must not return a path it just deleted.
func TestPathDoesNotEvictTheArtifactItIsReturning(t *testing.T) {
	t.Parallel()
	body := bytes.Repeat([]byte("x"), 4096)
	// Room for one artifact.
	c, remote := newCache(t, cache.Options{MaxBytes: int64(len(body))})
	publish(t, remote, "bld-running", artifact.NameRootfs, body)
	publish(t, remote, "bld-new", artifact.NameRootfs, body)

	if _, err := c.Path(t.Context(), "bld-running", artifact.NameRootfs); err != nil {
		t.Fatal(err)
	}
	defer c.Acquire("bld-running")()

	p, err := c.Path(t.Context(), "bld-new", artifact.NameRootfs)
	if err != nil {
		t.Fatalf("fetch under pressure: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("Path returned %s, which does not exist — it evicted the artifact it had "+
			"just fetched and verified, so every cold create on a node whose cache is "+
			"smaller than its pinned set plus one template fails: %v", p, err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("the returned path holds different bytes from the ones published")
	}
	if s := c.Stats(); s.Evictions != 0 {
		t.Logf("evictions during the sweep: %d", s.Evictions)
	}
}

func TestNewRejectsBadInput(t *testing.T) {
	t.Parallel()
	remote, _ := artifact.NewFS(t.TempDir())
	if _, err := cache.New(nil, t.TempDir(), cache.Options{}); err == nil {
		t.Error("want an error with no remote store")
	}
	if _, err := cache.New(remote, "", cache.Options{}); err == nil {
		t.Error("want an error with no directory")
	}
}

func TestPathPinnedHoldsTheArtifactAcrossTheReturn(t *testing.T) {
	// Room for one artifact, so any second fetch must evict something.
	c, remote := newCache(t, cache.Options{MaxBytes: 1000})
	publish(t, remote, "bld-a", "rootfs", bytes.Repeat([]byte("a"), 600))
	publish(t, remote, "bld-b", "rootfs", bytes.Repeat([]byte("b"), 600))

	p, release, err := c.PathPinned(t.Context(), "bld-a", "rootfs")
	if err != nil {
		t.Fatal(err)
	}

	// Another fetch drives the sweep while the path is still held.
	if _, err := c.Path(t.Context(), "bld-b", "rootfs"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(p); err != nil {
		t.Errorf("the artifact PathPinned returned was evicted while pinned: %v\n"+
			"a caller about to hardlink or boot from this path gets an ENOENT on a file "+
			"the cache had just verified", err)
	}
	release()

	// Once released it is an ordinary eviction candidate again.
	publish(t, remote, "bld-c", "rootfs", bytes.Repeat([]byte("c"), 600))
	if _, err := c.Path(t.Context(), "bld-c", "rootfs"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err == nil {
		t.Error("the artifact survived eviction after its pin was released — the release " +
			"does not decrement, so nothing pinned is ever reclaimable")
	}
}

func TestReleasingTwiceDoesNotUnderCount(t *testing.T) {
	c, remote := newCache(t, cache.Options{MaxBytes: 1000})
	publish(t, remote, "bld-a", "rootfs", bytes.Repeat([]byte("a"), 600))

	p, release, err := c.PathPinned(t.Context(), "bld-a", "rootfs")
	if err != nil {
		t.Fatal(err)
	}
	// The sandbox's own pin is taken while the fetch pin is still held.
	hold := c.Acquire("bld-a")
	defer hold()
	release()
	release()

	publish(t, remote, "bld-b", "rootfs", bytes.Repeat([]byte("b"), 600))
	if _, err := c.Path(t.Context(), "bld-b", "rootfs"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("a double release evicted an artifact a sandbox still holds: %v", err)
	}
}
