package docker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// ckptTagFile is where a checkpoint directory records its committed image tag.
const ckptTagFile = "tag"

// ckptImageTag is the local image a named checkpoint's filesystem is committed
// to. The `pd-ckpt-<id>-` prefix is what destroy and RecoverOrphans sweep on.
func ckptImageTag(sandboxID, name string) string {
	return "pd-ckpt-" + dockerSafe(sandboxID) + "-" + dockerSafe(name)
}

// ckptImageGlob matches every checkpoint image of one sandbox.
func ckptImageGlob(sandboxID string) string {
	return "pd-ckpt-" + dockerSafe(sandboxID) + "-*"
}

// Checkpoint implements rt.Checkpointer. Docker has no memory capture, so this
// is `docker commit`: the filesystem, with the container never stopped. The
// response says so with Kind "disk", and a restore relaunches the start command.
func (r *Runtime) Checkpoint(ctx context.Context, inst rt.Instance, dst rt.SnapshotRef) (rt.CheckpointInfo, error) {
	i, ok := inst.(*dockerInstance)
	if !ok {
		return rt.CheckpointInfo{}, fmt.Errorf("checkpoint: not a docker instance")
	}
	tag := ckptImageTag(i.id, dst.Name)
	if _, se, code, err := dockerOut(ctx, "commit", i.cid, tag); err != nil || code != 0 {
		return rt.CheckpointInfo{}, fmt.Errorf("commit checkpoint: %w %s", err, strings.TrimSpace(se))
	}
	if err := os.WriteFile(filepath.Join(dst.Path, ckptTagFile), []byte(tag), 0o600); err != nil {
		return rt.CheckpointInfo{}, err
	}
	// Size is decoration: a checkpoint whose size docker will not report is
	// still a checkpoint, so a parse failure leaves it 0 rather than failing.
	var size int64
	if out, _, code, err := dockerOut(ctx, "image", "inspect", "-f", "{{.Size}}", tag); err == nil && code == 0 {
		size, _ = strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	}
	return rt.CheckpointInfo{Kind: "disk", SizeBytes: size}, nil
}

// RestoreCheckpoint replaces the container with a fresh one from the checkpoint
// image, keeping the sandbox id. RAM and running processes are lost; the daemon
// relaunches the start command because Kind is "disk".
func (r *Runtime) RestoreCheckpoint(ctx context.Context, inst rt.Instance, src rt.SnapshotRef, spec rt.BootSpec) (rt.Instance, error) {
	data, err := os.ReadFile(filepath.Join(src.Path, ckptTagFile))
	if err != nil {
		return nil, fmt.Errorf("read checkpoint tag: %w", err)
	}
	tag := strings.TrimSpace(string(data))
	if _, _, code, err := dockerOut(ctx, "image", "inspect", tag); err != nil || code != 0 {
		return nil, fmt.Errorf("checkpoint image %s is gone", tag)
	}
	if i, ok := inst.(*dockerInstance); ok {
		_, _, _, _ = dockerOut(ctx, "rm", "-f", i.cid)
	}
	spec, devices := resolveBuiltin(spec)
	return r.bootImage(ctx, spec, tag, devices)
}

var _ rt.Checkpointer = (*Runtime)(nil)

// ckptImageFile is the saved image inside an externalized capture directory.
const ckptImageFile = "image.tar"

// Externalize implements rt.PortableCapture: `docker save` the committed image
// into the capture directory, so the directory alone is the whole machine.
// Without it a docker capture that leaves this node restores to "checkpoint
// image is gone" — the tag file names an image no other host has.
func (r *Runtime) Externalize(ctx context.Context, dir string) error {
	tag, err := os.ReadFile(filepath.Join(dir, ckptTagFile))
	if err != nil {
		return fmt.Errorf("read checkpoint tag: %w", err)
	}
	dst := filepath.Join(dir, ckptImageFile)
	if _, se, code, err := dockerOut(ctx, "save", "-o", dst, strings.TrimSpace(string(tag))); err != nil || code != 0 {
		return fmt.Errorf("save checkpoint image: %w %s", err, strings.TrimSpace(se))
	}
	return nil
}

// Internalize loads the saved image back, so RestoreCheckpoint's tag resolves.
// A capture taken on this node still has the image and the load is a no-op
// re-import; one fetched from object storage needs it.
func (r *Runtime) Internalize(ctx context.Context, dir string) error {
	src := filepath.Join(dir, ckptImageFile)
	if _, err := os.Stat(src); err != nil {
		// Nothing to load: the capture was never externalized, which is only
		// legal when the image is still on this node. RestoreCheckpoint says
		// so clearly if it is not.
		return nil
	}
	if _, se, code, err := dockerOut(ctx, "load", "-i", src); err != nil || code != 0 {
		return fmt.Errorf("load checkpoint image: %w %s", err, strings.TrimSpace(se))
	}
	return nil
}

var _ rt.PortableCapture = (*Runtime)(nil)

// BootFromSnapshot implements rt.SnapshotBooter. A docker named snapshot is a
// committed filesystem published as an ordinary image, so booting one is
// booting that image: src.Name is its tag, ensureImage pulls it out of the
// artifact store if this node has not loaded it, and the daemon relaunches the
// start command because there is no memory image to come back with.
func (r *Runtime) BootFromSnapshot(ctx context.Context, src rt.SnapshotRef, spec rt.BootSpec) (rt.Instance, string, error) {
	if src.Name == "" {
		return nil, "", fmt.Errorf("boot from snapshot: no image tag")
	}
	spec, devices := resolveBuiltin(spec)
	inst, err := r.bootImage(ctx, spec, src.Name, devices)
	if err != nil {
		return nil, "", err
	}
	return inst, "disk", nil
}

var _ rt.SnapshotBooter = (*Runtime)(nil)

// Freeze implements rt.Freezer with the cgroup freezer: the processes stop, the
// memory and every open socket stay. The daemon's state machine, not this, is
// what decides when a freeze is legal.
func (i *dockerInstance) Freeze(ctx context.Context) error {
	if _, se, code, err := dockerOut(ctx, "pause", i.cid); err != nil || code != 0 {
		return fmt.Errorf("pause container: %w %s", err, strings.TrimSpace(se))
	}
	return nil
}

// Thaw implements rt.Freezer.
func (i *dockerInstance) Thaw(ctx context.Context) error {
	if _, se, code, err := dockerOut(ctx, "unpause", i.cid); err != nil || code != 0 {
		return fmt.Errorf("unpause container: %w %s", err, strings.TrimSpace(se))
	}
	return nil
}

var _ rt.Freezer = (*dockerInstance)(nil)

// removeCheckpointImages drops every checkpoint image of one sandbox. Called
// from DeleteSnapshot, so a destroyed sandbox leaves none behind.
func removeCheckpointImages(ctx context.Context, sandboxID string) {
	imgs, _, _, _ := dockerOut(ctx, "images", "--filter", "reference="+ckptImageGlob(sandboxID), "-q")
	seen := map[string]bool{}
	for _, id := range strings.Fields(imgs) {
		if seen[id] {
			continue
		}
		seen[id] = true
		_, _, _, _ = dockerOut(ctx, "rmi", "-f", id)
	}
}
