package nodeboot

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/artifact/cache"
	"github.com/pilldrops-oss/pilldrops/internal/artifact/header"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// publishObject writes one data object and its block map.
func publishObject(t *testing.T, remote artifact.StorageProvider, buildID, name string, body []byte) {
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

func publishMetadata(t *testing.T, remote artifact.StorageProvider, meta artifact.Metadata) {
	t.Helper()
	raw, err := meta.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	key, err := artifact.Key(meta.BuildID, artifact.NameMetadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Put(t.Context(), key, bytes.NewReader(raw), int64(len(raw))); err != nil {
		t.Fatal(err)
	}
}

// snapshotOwnRootfs is what a complete snapshot build carries as its root
// image: the bytes of the image the CAPTURE was taken against, which are not
// the bytes of any image the fetching node built for itself.
func snapshotOwnRootfs(snapID string) []byte {
	return []byte("hsqs-captured-against-" + snapID + strings.Repeat("~", 64))
}

// publishSnapshot puts a complete snapshot build in the store: every object
// plus metadata naming the template it was captured from.
func publishSnapshot(t *testing.T, remote artifact.StorageProvider, buildID, snapID, source string) {
	t.Helper()
	for _, name := range artifact.SnapshotObjects() {
		body := []byte(snapID + "/" + name + strings.Repeat("!", 32))
		if name == artifact.NameRootfs {
			body = snapshotOwnRootfs(snapID)
		}
		publishObject(t, remote, buildID, name, body)
	}
	publishMetadata(t, remote, artifact.Metadata{
		BuildID: buildID, TemplateID: snapID, Payload: artifact.PayloadSnapshot,
		RootfsSize: 1, RootfsSHA256: strings.Repeat("0", 64),
		Snapshot: &artifact.SnapshotMeta{
			Runtime: "firecracker", Template: source, VCPU: 2, MemMB: 2048,
			TapName: "tap-src", VMIP: "10.128.9.2",
		},
	})
}

// publishLegacySnapshot is a build published before the root image travelled
// with the capture: every object but the rootfs.
func publishLegacySnapshot(t *testing.T, remote artifact.StorageProvider, buildID, snapID, source string) {
	t.Helper()
	for _, name := range artifact.SnapshotObjects() {
		if name == artifact.NameRootfs {
			continue
		}
		publishObject(t, remote, buildID, name, []byte(snapID+"/"+name+strings.Repeat("!", 32)))
	}
	publishMetadata(t, remote, artifact.Metadata{
		BuildID: buildID, TemplateID: snapID, Payload: artifact.PayloadSnapshot,
		RootfsSize: 1, RootfsSHA256: strings.Repeat("0", 64),
		Snapshot: &artifact.SnapshotMeta{
			Runtime: "firecracker", Template: source, VCPU: 2, MemMB: 2048,
			TapName: "tap-src", VMIP: "10.128.9.2",
		},
	})
}

// A node that has never held any of it must end up with every object, the root
// image the capture was taken against included: the overlay is a delta against
// that exact disk and the memory image refers into it.
func TestAColdNodeFetchesEveryPartOfASnapshot(t *testing.T) {
	const snapID = "snap-deadbeef-golden"
	index := fakeIndex{snapID: "bld-snap", "base": "bld-base"}
	fx := newFixture(t, index, cache.Options{})
	publishSnapshot(t, fx.remote, "bld-snap", snapID, "base")
	publishRootfs(t, fx.remote, "bld-base", []byte("hsqs"+strings.Repeat("base-rootfs", 64)))

	// Nothing local: this is the cold case.
	if entries, _ := os.ReadDir(fx.imageDir); len(entries) != 0 {
		t.Fatalf("the image directory is not empty to start with: %v", entries)
	}

	if err := fx.fetcher.Fetch(t.Context(), snapID, fx.imageDir); err != nil {
		t.Fatalf("cold fetch of %s: %v", snapID, err)
	}

	// The engine's file names, not the store's: the fetched directory has to be
	// a capture directory a restore can read.
	want := []string{"state", "mem", "overlay.ext4", "meta.json", "rootfs.squashfs"}
	for _, name := range want {
		fi, err := os.Stat(filepath.Join(fx.imageDir, snapID, name))
		if err != nil {
			t.Fatalf("%s is missing after a cold fetch: %v", name, err)
		}
		if fi.Size() == 0 {
			t.Errorf("%s is empty", name)
		}
	}
	// The whole point: the image beside the snapshot is the one the CAPTURE was
	// taken against, not this node's own build of the source template. Both are
	// present and they are different bytes, so a fetch that picked the wrong one
	// fails here.
	rootfs, err := os.ReadFile(filepath.Join(fx.imageDir, snapID, "rootfs.squashfs"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rootfs, snapshotOwnRootfs(snapID)) {
		t.Fatalf("the image beside the snapshot is not the one it was captured against: %q",
			rootfs[:min(len(rootfs), 40)])
	}

	// The heartbeat's "templates I hold" is the disk scan, so the snapshot id
	// must appear in it — a node that holds the objects and does not advertise
	// them is never preferred, and one that advertises without them fails a boot.
	templates.SetDir(fx.imageDir)
	t.Cleanup(func() { templates.SetDir("") })
	ids := templates.PublishedIDs()
	sort.Strings(ids)
	if sort.SearchStrings(ids, snapID) == len(ids) || !contains(ids, snapID) {
		t.Fatalf("after fetching it, this node advertises %v — the snapshot is not among them", ids)
	}
}

func contains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// The failure this guards is the quiet one: the other objects present, no
// backing disk, and a create that gets as far as the boot before anything
// notices. Only reachable for a build published before the image travelled —
// a current one carries its own and can never be in this state.
func TestALegacySnapshotWhoseSourceTemplateIsMissingIsRefused(t *testing.T) {
	const snapID = "snap-deadbeef-orphan"
	index := fakeIndex{snapID: "bld-orphan"} // no build for "base"
	fx := newFixture(t, index, cache.Options{})
	publishLegacySnapshot(t, fx.remote, "bld-orphan", snapID, "base")

	err := fx.fetcher.Fetch(t.Context(), snapID, fx.imageDir)
	if err == nil {
		t.Fatal("a snapshot with no source rootfs fetched successfully; the node now advertises " +
			"a machine that cannot boot")
	}
	for _, want := range []string{"base", "backing disk"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
	// Positive control: publishing the source makes the same fetch succeed.
	index["base"] = "bld-base"
	publishRootfs(t, fx.remote, "bld-base", []byte("hsqs"+strings.Repeat("base-rootfs", 64)))
	if err := fx.fetcher.Fetch(t.Context(), snapID, fx.imageDir); err != nil {
		t.Fatalf("with the source template published the fetch still failed: %v", err)
	}
}

// A legacy build has no image of its own, so it still falls back to the copy
// this node already holds rather than failing. That fallback is only correct
// when the two nodes built the same bytes, which is why the image travels now;
// the restore refuses on the recorded digest when they did not.
func TestALegacySnapshotFetchReusesALocalSourceRootfs(t *testing.T) {
	const snapID = "snap-deadbeef-warm"
	index := fakeIndex{snapID: "bld-warm"} // "base" is NOT in the store
	fx := newFixture(t, index, cache.Options{})
	publishLegacySnapshot(t, fx.remote, "bld-warm", snapID, "base")

	local := filepath.Join(fx.imageDir, "base")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("hsqs" + strings.Repeat("local-base", 64))
	if err := os.WriteFile(filepath.Join(local, "rootfs.squashfs"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := fx.fetcher.Fetch(t.Context(), snapID, fx.imageDir); err != nil {
		t.Fatalf("fetch with a locally held source template: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(fx.imageDir, snapID, "rootfs.squashfs"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("the snapshot's rootfs is not the copy this node already held")
	}
}

// A snapshot build reached through the ordinary template path must not be
// linked in as a rootfs: the bytes are a memory image, and a VM told to boot
// them would fail in the VMM rather than here.
func TestASnapshotBuildIsNeverLinkedInAsARootfs(t *testing.T) {
	const snapID = "snap-deadbeef-mixed"
	index := fakeIndex{snapID: "bld-mixed", "base": "bld-base"}
	fx := newFixture(t, index, cache.Options{})
	publishSnapshot(t, fx.remote, "bld-mixed", snapID, "base")
	publishRootfs(t, fx.remote, "bld-base", []byte("hsqs"+strings.Repeat("base-rootfs", 64)))

	if err := fx.fetcher.Fetch(t.Context(), snapID, fx.imageDir); err != nil {
		t.Fatal(err)
	}
	rootfs, err := os.ReadFile(filepath.Join(fx.imageDir, snapID, "rootfs.squashfs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"state", "mem", "overlay.ext4"} {
		obj, err := os.ReadFile(filepath.Join(fx.imageDir, snapID, name))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(obj, rootfs) {
			t.Fatalf("rootfs.squashfs holds the %s bytes", name)
		}
	}
}
