package store

import (
	"context"
	"errors"
	"testing"
)

func TestVolumeLifecycle(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	v := Volume{ID: "vol-1", OrgID: "acme", Name: "data", SizeMB: 1024, NodeID: "n1"}
	if err := st.CreateVolume(ctx, v); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, ok, err := st.GetVolume(ctx, "vol-1"); err != nil || !ok || got.Name != "data" || got.AttachedTo != "" {
		t.Fatalf("get = %+v ok=%v err=%v", got, ok, err)
	}
	if list, _ := st.ListVolumes(ctx, "other"); len(list) != 0 {
		t.Errorf("another org sees %d volumes", len(list))
	}

	if err := st.AttachVolume(ctx, "vol-1", "sb-a"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := st.AttachVolume(ctx, "vol-1", "sb-b"); !errors.Is(err, ErrVolumeAttached) {
		t.Errorf("second attach: err = %v, want ErrVolumeAttached", err)
	}
	if err := st.DeleteVolume(ctx, "vol-1"); !errors.Is(err, ErrVolumeAttached) {
		t.Errorf("delete while attached: err = %v, want ErrVolumeAttached", err)
	}
	if ids, _ := st.VolumesAttachedTo(ctx, "sb-a"); len(ids) != 1 || ids[0] != "vol-1" {
		t.Errorf("attached to sb-a = %v", ids)
	}
	if err := st.DetachVolumes(ctx, "sb-a"); err != nil {
		t.Fatal(err)
	}
	if err := st.AttachVolume(ctx, "vol-1", "sb-b"); err != nil {
		t.Errorf("attach after detach: %v", err)
	}
	_ = st.DetachVolumes(ctx, "sb-b")
	if err := st.DeleteVolume(ctx, "vol-1"); err != nil {
		t.Errorf("delete detached: %v", err)
	}
	if _, ok, _ := st.GetVolume(ctx, "vol-1"); ok {
		t.Error("deleted volume still there")
	}
}
