package templates

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeImage publishes one version of id into the image directory, the way a
// fetch would: the rootfs first, the build stamp last.
func writeImage(t *testing.T, dir, id, build, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, id), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id, "rootfs.squashfs"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	SetDir(dir)
	if err := StampBuild(id, build); err != nil {
		t.Fatal(err)
	}
	forceRescan()
}

func bootedBytes(t *testing.T, dir, id string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, id, "rootfs.squashfs"))
	if err != nil {
		t.Fatalf("read the image the restore would boot: %v", err)
	}
	return string(b)
}

// Saving a second version of one name must boot the second version's bytes.
//
// Against the old code this fails on the content assertion, not on an error:
// ResolveFetchTimed returned on the disk hit, the Source was never called, and
// every command up the stack still reported success — which is exactly what
// made the data loss silent. Asserting only that the resolve succeeds proves
// nothing, so this reads the bytes the boot would read.
func TestASecondVersionOfOneNameIsWhatBoots(t *testing.T) {
	dir := t.TempDir()
	const id = "snap-abcd-golden"

	writeImage(t, dir, id, "bld-s0000000001-v1", "version-1")

	// The store now publishes v2 under the same id, which is what a second
	// `snapshot save golden` does.
	const v2Build = "bld-s0000000002-v2"
	fetched := 0
	SetSource(func(_ context.Context, fetchID, into string) error {
		fetched++
		if fetchID != id {
			t.Errorf("fetched %q, want %q", fetchID, id)
		}
		if err := os.WriteFile(filepath.Join(into, fetchID, "rootfs.squashfs"), []byte("version-2"), 0o644); err != nil {
			return err
		}
		return StampBuild(fetchID, v2Build) // nodeboot.TemplateFetcher does this for real
	})
	SetCurrentBuild(func(context.Context, string) (string, error) { return v2Build, nil })
	t.Cleanup(func() { SetSource(nil); SetCurrentBuild(nil); SetDir("") })

	if _, err := ResolveFetch(t.Context(), id); err != nil {
		t.Fatalf("resolve after the second save: %v", err)
	}
	if got := bootedBytes(t, dir, id); got != "version-2" {
		t.Fatalf("the boot would read %q, want %q — the node served v1 for a create that "+
			"the control plane placed on v2, and reported success", got, "version-2")
	}
	if fetched != 1 {
		t.Fatalf("the store tier was consulted %d times, want 1", fetched)
	}
	if got := HeldBuild(id); got != v2Build {
		t.Fatalf("stamp = %q after the re-fetch, want %q", got, v2Build)
	}

	// And the very next create must not pay for the fetch again.
	if _, err := ResolveFetch(t.Context(), id); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if fetched != 1 {
		t.Fatalf("a current image was re-fetched (%d fetches) — every create would pay a download", fetched)
	}
}

// The image directory must hold one image per name however many versions are
// saved, or the fix for staleness becomes a full disk.
func TestReFetchReplacesTheImageRatherThanAccumulating(t *testing.T) {
	dir := t.TempDir()
	const id = "snap-abcd-churn"
	writeImage(t, dir, id, "bld-s0000000001-a", "v1")

	build := "bld-s0000000001-a"
	SetSource(func(_ context.Context, fetchID, into string) error {
		if err := os.WriteFile(filepath.Join(into, fetchID, "rootfs.squashfs"), []byte(build), 0o644); err != nil {
			return err
		}
		return StampBuild(fetchID, build)
	})
	SetCurrentBuild(func(context.Context, string) (string, error) { return build, nil })
	t.Cleanup(func() { SetSource(nil); SetCurrentBuild(nil); SetDir("") })

	for i := 2; i <= 6; i++ {
		build = "bld-s000000000" + string(rune('0'+i)) + "-v"
		if _, err := ResolveFetch(t.Context(), id); err != nil {
			t.Fatalf("resolve version %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("six versions left %d directories in the image dir (%v), want 1 — a cache keyed "+
			"by build id trades the staleness bug for a full disk", len(entries), names)
	}
}

// Everything the node cannot positively contradict keeps booting: an index it
// cannot reach must not turn every create into a refusal.
func TestAnUnreadableIndexDoesNotInvalidateWhatIsHeld(t *testing.T) {
	dir := t.TempDir()
	const id = "snap-abcd-offline"
	writeImage(t, dir, id, "bld-s0000000001-a", "held")

	SetSource(func(context.Context, string, string) error {
		t.Error("the store tier was consulted although the index gave no answer")
		return ErrNotInStore
	})
	SetCurrentBuild(func(context.Context, string) (string, error) {
		return "", context.DeadlineExceeded
	})
	t.Cleanup(func() { SetSource(nil); SetCurrentBuild(nil); SetDir("") })

	if _, err := ResolveFetch(t.Context(), id); err != nil {
		t.Fatalf("resolve with an unreachable index: %v", err)
	}
	if got := bootedBytes(t, dir, id); got != "held" {
		t.Fatalf("booted %q, want the held image", got)
	}
}

// An image the operator baked in place carries no stamp, and the index must not
// be allowed to overwrite it.
func TestAnUnstampedImageIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	const id = "operator-baked"
	if err := os.MkdirAll(filepath.Join(dir, id), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id, "rootfs.squashfs"), []byte("baked"), 0o644); err != nil {
		t.Fatal(err)
	}
	SetDir(dir)
	forceRescan()
	SetSource(func(context.Context, string, string) error {
		t.Error("an unstamped image was re-fetched over")
		return ErrNotInStore
	})
	SetCurrentBuild(func(context.Context, string) (string, error) { return "bld-something-else", nil })
	t.Cleanup(func() { SetSource(nil); SetCurrentBuild(nil); SetDir("") })

	if _, err := ResolveFetch(t.Context(), id); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := bootedBytes(t, dir, id); got != "baked" {
		t.Fatalf("booted %q, want the operator's own image", got)
	}
}
