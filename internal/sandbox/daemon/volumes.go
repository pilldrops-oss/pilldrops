package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

var (
	// ErrVolumesUnsupported is a create naming volumes on a runtime that cannot attach them.
	ErrVolumesUnsupported = errors.New("this node's runtime cannot attach volumes; they need the firecracker runtime")
	// ErrVolumeNotFound is a volume that does not exist, is another org's, or lives on another node.
	ErrVolumeNotFound = errors.New("volume not found on this node")
	// ErrHasVolumes refuses what would let two machines write one volume, or
	// leave it behind: fork, backup, move and rehome.
	ErrHasVolumes = errors.New("this sandbox has a volume attached, so it cannot be forked, backed up or moved; " +
		"detach it by destroying the sandbox, then attach the volume to a new one")
)

// MaxVolumesPerSandbox bounds the extra drives one VM gets.
const MaxVolumesPerSandbox = 4

// mountPathRe is a plain absolute path: it is passed to mount in the guest, so
// nothing a shell would read specially.
var mountPathRe = regexp.MustCompile(`^/[a-zA-Z0-9._/-]+$`)

// reservedMounts are the guest's own directories; mounting over one breaks it.
var reservedMounts = []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/lib64", "/proc", "/root", "/run", "/sbin", "/sys", "/tmp", "/usr", "/var"}

func validVolumeMounts(vols []rt.VolumeMount) error {
	if len(vols) > MaxVolumesPerSandbox {
		return &sandbox.InvalidError{Reason: fmt.Sprintf("at most %d volumes per sandbox", MaxVolumesPerSandbox)}
	}
	seen := map[string]bool{}
	for _, v := range vols {
		p := strings.TrimRight(v.Path, "/")
		if !mountPathRe.MatchString(p) || strings.Contains(p, "..") || strings.Contains(p, "//") {
			return &sandbox.InvalidError{Reason: fmt.Sprintf("volume path %q must be a plain absolute path such as /data", v.Path)}
		}
		for _, r := range reservedMounts {
			if p == r || strings.HasPrefix(p, r+"/") {
				return &sandbox.InvalidError{Reason: fmt.Sprintf("volume path %q is inside %s, which the guest needs", v.Path, r)}
			}
		}
		if seen[v.ID] || seen["path:"+p] {
			return &sandbox.InvalidError{Reason: "each volume and each path may appear once"}
		}
		seen[v.ID], seen["path:"+p] = true, true
	}
	return nil
}

type volumeStore interface {
	CreateVolume(ctx context.Context, v store.Volume) error
	ListVolumes(ctx context.Context, orgID string) ([]store.Volume, error)
	DeleteVolume(ctx context.Context, id string) error
	GetVolume(ctx context.Context, id string) (store.Volume, bool, error)
	AttachVolume(ctx context.Context, id, sandboxID string) error
	DetachVolumes(ctx context.Context, sandboxID string) error
	VolumesAttachedTo(ctx context.Context, sandboxID string) ([]string, error)
}

// attachVolumes claims each volume for sandbox id. Every volume must be the
// org's and on this node; on any failure the ones already claimed are released.
func (d *Daemon) attachVolumes(ctx context.Context, id, org string, vols []rt.VolumeMount) error {
	if len(vols) == 0 {
		return nil
	}
	if !d.cfg.Runtime.Capabilities().Volumes {
		return ErrVolumesUnsupported
	}
	if err := validVolumeMounts(vols); err != nil {
		return err
	}
	vs, ok := d.cfg.Store.(volumeStore)
	if !ok {
		return fmt.Errorf("%w: this node has no store to hold volumes", ErrVolumesUnsupported)
	}
	for _, v := range vols {
		rec, found, err := vs.GetVolume(ctx, v.ID)
		if err == nil && (!found || rec.OrgID != org || (rec.NodeID != "" && d.cfg.NodeID != "" && rec.NodeID != d.cfg.NodeID)) {
			err = fmt.Errorf("%w: %s", ErrVolumeNotFound, v.ID)
		}
		if err == nil {
			err = vs.AttachVolume(ctx, v.ID, id)
		}
		if err != nil {
			_ = vs.DetachVolumes(ctx, id)
			return err
		}
	}
	return nil
}

// detachVolumes releases every volume a destroyed sandbox held; the data stays.
func (d *Daemon) detachVolumes(ctx context.Context, id string) {
	if vs, ok := d.cfg.Store.(volumeStore); ok {
		if err := vs.DetachVolumes(ctx, id); err != nil {
			d.log().Error("could not release a destroyed sandbox's volumes", "sandbox_id", id, "error", err)
		}
	}
}

// refuseIfVolumes is the guard for fork, backup, move and rehome.
func (d *Daemon) refuseIfVolumes(ctx context.Context, id string) error {
	vs, ok := d.cfg.Store.(volumeStore)
	if !ok {
		return nil
	}
	ids, err := vs.VolumesAttachedTo(ctx, id)
	if err != nil {
		return err
	}
	if len(ids) > 0 {
		return ErrHasVolumes
	}
	return nil
}

// RefuseIfVolumes is refuseIfVolumes for callers outside the daemon, such as a
// capture export, whose result would name drives no other node has.
func (d *Daemon) RefuseIfVolumes(ctx context.Context, id string) error {
	return d.refuseIfVolumes(ctx, id)
}

// Volume sizes: mkfs.ext4's floor, and the largest disk any plan grants a sandbox.
const (
	MinVolumeMB = 256
	MaxVolumeMB = 100 * 1024
)

// volumes returns the store and the runtime's Volumer, or why there are none.
func (d *Daemon) volumes() (volumeStore, rt.Volumer, error) {
	vr, ok := d.cfg.Runtime.(rt.Volumer)
	if !ok || !d.cfg.Runtime.Capabilities().Volumes {
		return nil, nil, ErrVolumesUnsupported
	}
	vs, ok := d.cfg.Store.(volumeStore)
	if !ok {
		return nil, nil, fmt.Errorf("%w: this node has no store to hold volumes", ErrVolumesUnsupported)
	}
	return vs, vr, nil
}

// CreateVolume makes an empty volume on this node for org.
func (d *Daemon) CreateVolume(ctx context.Context, org, name string, sizeMB int) (store.Volume, error) {
	if sizeMB < MinVolumeMB || sizeMB > MaxVolumeMB {
		return store.Volume{}, &sandbox.InvalidError{Reason: fmt.Sprintf("size_mb must be %d-%d", MinVolumeMB, MaxVolumeMB)}
	}
	if len(name) > 64 {
		return store.Volume{}, &sandbox.InvalidError{Reason: "name must be at most 64 characters"}
	}
	vs, vr, err := d.volumes()
	if err != nil {
		return store.Volume{}, err
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return store.Volume{}, err
	}
	v := store.Volume{ID: "vol-" + hex.EncodeToString(b), OrgID: org, Name: name, SizeMB: sizeMB, NodeID: d.cfg.NodeID, CreatedAt: time.Now().Unix()}
	if err := vr.CreateVolume(v.ID, sizeMB); err != nil {
		return store.Volume{}, err
	}
	if err := vs.CreateVolume(ctx, v); err != nil {
		_ = vr.DeleteVolume(v.ID)
		return store.Volume{}, err
	}
	return v, nil
}

// ListVolumes lists org's volumes.
func (d *Daemon) ListVolumes(ctx context.Context, org string) ([]store.Volume, error) {
	vs, _, err := d.volumes()
	if err != nil {
		return nil, err
	}
	return vs.ListVolumes(ctx, org)
}

// GetVolume reads one of org's volumes; another org's is not found.
func (d *Daemon) GetVolume(ctx context.Context, org, id string) (store.Volume, error) {
	vs, _, err := d.volumes()
	if err != nil {
		return store.Volume{}, err
	}
	v, ok, err := vs.GetVolume(ctx, id)
	if err != nil {
		return store.Volume{}, err
	}
	if !ok || v.OrgID != org {
		return store.Volume{}, fmt.Errorf("%w: %s", ErrVolumeNotFound, id)
	}
	return v, nil
}

// DeleteVolume removes one of org's volumes and its data. Refused while attached.
func (d *Daemon) DeleteVolume(ctx context.Context, org, id string) error {
	v, err := d.GetVolume(ctx, org, id)
	if err != nil {
		return err
	}
	vs, vr, _ := d.volumes()
	if err := vs.DeleteVolume(ctx, v.ID); err != nil {
		return err
	}
	return vr.DeleteVolume(v.ID)
}
