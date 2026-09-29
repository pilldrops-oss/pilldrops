package orchestrator

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/artifact/header"
)

// Only a shared root image that nothing references and that is past its grace
// is deleted; an unreadable header stops the sweep before any delete.
func TestSweepSharedRootfs(t *testing.T) {
	ctx := context.Background()
	blobs, err := artifact.Open(artifact.KindFS, artifact.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	put := func(key string, b []byte) {
		if err := blobs.Put(ctx, key, bytes.NewReader(b), int64(len(b))); err != nil {
			t.Fatal(err)
		}
	}
	ref := func(key, self, shared string) {
		h := &header.Header{BuildID: self, BaseBuildID: shared, BlockSize: 4096, TotalSize: 4096,
			BuildIDs: []string{self, shared},
			Builds:   map[string]header.BuildData{self: {}, shared: {Size: 4096}},
			Mapping:  []header.Entry{{StartBlock: 0, BlockCount: 1, BuildIndex: 1}}}
		raw, err := h.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		put(key, raw)
	}
	for _, id := range []string{"rfs-a", "rfs-b", "rfs-c"} {
		put(id+"/rootfs", []byte("image "+id))
	}
	ref("bld-1/rootfs.header", "bld-1", "rfs-a")                          // a published capture
	ref("pause/sb-1/files/rootfs.squashfs.header", "pause-sb-1", "rfs-b") // a pause archive
	exists := func(id string) bool { _, err := blobs.Stat(ctx, id+"/rootfs"); return err == nil }

	if n, err := sweepSharedRootfs(ctx, blobs, time.Now(), time.Hour); err != nil || n != 0 {
		t.Fatalf("within the grace: deleted %d (%v), want 0", n, err)
	}
	put("rfs-d/rootfs", []byte("new")) // unreferenced; past the grace at the later clock below
	later := time.Now().Add(2 * time.Hour)
	n, err := sweepSharedRootfs(ctx, blobs, later, time.Hour)
	if err != nil || n != 2 || exists("rfs-c") || exists("rfs-d") {
		t.Fatalf("sweep deleted %d (%v); c exists %v, d exists %v; want c and d gone", n, err, exists("rfs-c"), exists("rfs-d"))
	}
	if !exists("rfs-a") || !exists("rfs-b") {
		t.Fatal("a referenced root image was deleted")
	}

	put("bld-2/rootfs.header", []byte("not a header"))
	put("rfs-e/rootfs", []byte("orphan"))
	if n, err := sweepSharedRootfs(ctx, blobs, later.Add(time.Hour), time.Hour); err == nil || n != 0 || !exists("rfs-e") {
		t.Fatalf("an unreadable header: deleted %d, err %v; want a refusal that deletes nothing", n, err)
	}
}
