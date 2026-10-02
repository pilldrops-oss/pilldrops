package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// A rehome whose boot fails must not leave its capture behind as a template:
// on the rig /v1/templates listed snap-rehome-<id> to the tenant.
func TestAFailedRehomeLeavesNoTemplate(t *testing.T) {
	templates.SetDir(t.TempDir())
	t.Cleanup(func() { templates.SetDir("") })
	blobs, err := artifact.Open(artifact.KindFS, artifact.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const id = "sb-1790000000000-0123abcd"
	capture := t.TempDir()
	for _, name := range []string{"mem", "state", "rootfs.squashfs"} {
		if err := os.WriteFile(filepath.Join(capture, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	archive := &BlobArchive{Store: blobs}
	if err := archive.PutPause(ctx, id, capture, "p1"); err != nil {
		t.Fatal(err)
	}
	fp := recordFake{newFakePersister(nil)}
	fp.saved[id] = PersistedSandbox{ID: id, OrgID: "o", Template: "base", State: "paused", PauseArchive: "p1", CreatedAt: 100}
	fake := runtimetest.NewFake()
	fake.BootErr = errors.New("Failed to set all KVM MSRs for this vCPU")
	d := New(Config{Runtime: fake, Store: fp, SnapshotArchive: archive, IdleTimeout: time.Minute, HardTTL: time.Hour})

	if err := d.Rehome(ctx, id, "o"); err == nil {
		t.Fatal("a rehome whose boot failed reported success")
	}
	if ids := templates.PublishedIDs(); len(ids) != 0 {
		t.Fatalf("templates left after the failed rehome: %v", ids)
	}
	if ok, err := archive.Get(ctx, id, filepath.Join(t.TempDir(), "again")); !ok || err != nil {
		t.Fatalf("the archive was lost with the failed rehome: %v, %v", ok, err)
	}
}
