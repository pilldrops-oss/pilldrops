package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func newVolumeDaemon(t *testing.T) (*Daemon, *runtimetest.Fake, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	fake := runtimetest.NewFake()
	fake.Volumes = true
	d := New(Config{Runtime: fake, Store: StoreAdapter{S: st}, SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Minute, HardTTL: time.Hour})
	return d, fake, st
}

func TestAVolumeOutlivesTheSandboxItIsAttachedTo(t *testing.T) {
	d, fake, st := newVolumeDaemon(t)
	ctx := context.Background()
	vol, err := d.CreateVolume(ctx, "acme", "data", 1024)
	if err != nil {
		t.Fatalf("create volume: %v", err)
	}
	if !fake.VolumeFile(vol.ID) {
		t.Fatal("no volume image was made")
	}
	mount := []rt.VolumeMount{{ID: vol.ID, Path: "/data"}}

	a, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "acme", Volumes: mount})
	if err != nil {
		t.Fatalf("create with volume: %v", err)
	}
	if spec, _ := fake.LastBootSpec(); len(spec.Volumes) != 1 || spec.Volumes[0] != mount[0] {
		t.Errorf("boot got volumes %+v, want %+v", spec.Volumes, mount)
	}
	if got, _, _ := st.GetVolume(ctx, vol.ID); got.AttachedTo != a.ID {
		t.Errorf("attached to %q, want %q", got.AttachedTo, a.ID)
	}

	if _, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "acme", Volumes: mount}); !errors.Is(err, store.ErrVolumeAttached) {
		t.Errorf("a second sandbox on the same volume: err = %v, want ErrVolumeAttached", err)
	}
	if _, err := d.Fork(ctx, a.ID, ForkSpec{}); !errors.Is(err, ErrHasVolumes) {
		t.Errorf("fork of a sandbox with a volume: err = %v, want ErrHasVolumes", err)
	}
	if err := d.DeleteVolume(ctx, "acme", vol.ID); !errors.Is(err, store.ErrVolumeAttached) {
		t.Errorf("delete while attached: err = %v, want ErrVolumeAttached", err)
	}

	if err := d.Destroy(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	b, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "acme", Volumes: mount})
	if err != nil {
		t.Fatalf("reattach after destroy: %v", err)
	}
	_ = d.Destroy(ctx, b.ID)
	if err := d.DeleteVolume(ctx, "acme", vol.ID); err != nil {
		t.Fatalf("delete detached: %v", err)
	}
	if fake.VolumeFile(vol.ID) {
		t.Error("deleted volume's image is still there")
	}
}

func TestVolumeRefusals(t *testing.T) {
	d, fake, _ := newVolumeDaemon(t)
	ctx := context.Background()
	vol, err := d.CreateVolume(ctx, "acme", "data", 1024)
	if err != nil {
		t.Fatal(err)
	}
	create := func(org, path string) error {
		_, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: org, Volumes: []rt.VolumeMount{{ID: vol.ID, Path: path}}})
		return err
	}
	if err := create("other-org", "/data"); !errors.Is(err, ErrVolumeNotFound) {
		t.Errorf("another org's volume: err = %v, want ErrVolumeNotFound", err)
	}
	var inv *sandbox.InvalidError
	for _, p := range []string{"/etc", "/usr/local", "data", "/data;reboot", "/a/../etc", "/"} {
		if err := create("acme", p); !errors.As(err, &inv) {
			t.Errorf("mount path %q: err = %v, want an invalid-request error", p, err)
		}
	}
	if _, err := d.CreateVolume(ctx, "acme", "tiny", 10); !errors.As(err, &inv) {
		t.Errorf("10 MB volume: err = %v, want an invalid-request error", err)
	}
	if _, err := d.GetVolume(ctx, "other-org", vol.ID); !errors.Is(err, ErrVolumeNotFound) {
		t.Errorf("another org reading it: err = %v, want ErrVolumeNotFound", err)
	}

	fake.Volumes = false
	if err := create("acme", "/data"); !errors.Is(err, ErrVolumesUnsupported) {
		t.Errorf("runtime without volumes: err = %v, want ErrVolumesUnsupported", err)
	}
}

// A destroy stops the VM like a power cut, so the guest is told to sync first:
// on a real node, data written less than ~30 s before the destroy was lost.
func TestDestroyFlushesASandboxWithAVolume(t *testing.T) {
	d, fake, _ := newVolumeDaemon(t)
	ctx := context.Background()
	vol, err := d.CreateVolume(ctx, "acme", "data", 1024)
	if err != nil {
		t.Fatalf("create volume: %v", err)
	}
	withVolume, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "acme",
		Volumes: []rt.VolumeMount{{ID: vol.ID, Path: "/data"}}})
	if err != nil {
		t.Fatalf("create with volume: %v", err)
	}
	plain, err := d.Create(ctx, CreateSpec{Template: "python-3.11", OrgID: "acme"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, id := range []string{withVolume.ID, plain.ID} {
		if err := d.Destroy(ctx, id); err != nil {
			t.Fatalf("destroy %s: %v", id, err)
		}
	}
	if log := fake.ExecLog(withVolume.ID); len(log) == 0 || log[len(log)-1] != "sync" {
		t.Errorf("sandbox with a volume: exec log %v, want a final sync before stop", log)
	}
	for _, cmd := range fake.ExecLog(plain.ID) {
		if cmd == "sync" {
			t.Error("a sandbox without volumes was synced; only volume sandboxes pay for it")
		}
	}
}
