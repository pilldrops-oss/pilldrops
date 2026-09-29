package docker

import (
	"context"
	"os"
	"strings"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// Restore boots the sandbox from its committed snapshot image, falling back to
// the template base image if the snapshot is missing.
func (r *Runtime) Restore(ctx context.Context, snap rt.SnapshotRef, spec rt.BootSpec) (rt.Instance, error) {
	spec, devices := resolveBuiltin(spec)
	img := spec.Template.Image
	if img == "" {
		img = imageFor(spec.Template.Name)
	} else if warm := r.builtinWarmImage(ctx, spec.Template.Name); warm != "" {
		img = warm
	}
	if !snap.IsZero() {
		if data, err := os.ReadFile(snap.Path); err == nil {
			if tag := strings.TrimSpace(string(data)); tag != "" {
				// dockerOut reports code 0 when the command could not run at all,
				// so check runErr too or a broken docker reads as "image present".
				if _, _, code, runErr := dockerOut(ctx, "image", "inspect", tag); code == 0 && runErr == nil {
					img = tag
				}
			}
		}
	}
	return r.bootImage(ctx, spec, img, devices)
}

// Adopt reattaches to a still-running container from a previous daemon process,
// matched by the sandboxd.id label. A container with no published port still adopts.
func (r *Runtime) Adopt(ctx context.Context, sandboxID string) (rt.Instance, bool) {
	out, _, code, err := dockerOut(ctx, "ps", "-q",
		"--filter", "label=sandboxd.id="+sandboxID, "--filter", "status=running")
	if err != nil || code != 0 {
		return nil, false
	}
	cid := strings.TrimSpace(out)
	if cid == "" {
		return nil, false
	}
	if i := strings.IndexByte(cid, '\n'); i >= 0 {
		cid = cid[:i]
	}
	// Builtin services preview on their own primary port, recovered from the image.
	primary := appPort
	if imgOut, _, _, _ := dockerOut(ctx, "inspect", "-f", "{{.Config.Image}}", cid); imgOut != "" {
		img := strings.TrimSpace(imgOut)
		for _, bs := range builtinServices {
			if (bs.image == img || (bs.warmTag != "" && bs.warmTag == img)) && len(bs.ports) > 0 {
				primary = bs.ports[0]
			}
		}
	}
	host, _ := mappedPort(ctx, cid, primary)
	termHost, _ := mappedPort(ctx, cid, terminalPort)
	cmdOut, _, _, _ := dockerOut(ctx, "inspect", "-f", "{{join .Config.Cmd \" \"}}", cid)
	service := !strings.Contains(cmdOut, "sleep infinity")
	return &dockerInstance{
		id: sandboxID, cid: cid, host: host, termHost: termHost, service: service,
		runUser: r.adoptedRunUser(ctx, cid, service), start: time.Now(),
		eng:   r.eng,
		ports: map[int]string{primary: host},
	}, true
}

// adoptedRunUser reads the run user from the sandboxd.runuser label. The label
// is authoritative because re-deriving it from docker inspect would be a second
// dropUser that can drift, and drift here fails open. Pre-label containers fall
// back to sandboxRunUser, the safe direction.
func (r *Runtime) adoptedRunUser(ctx context.Context, cid string, service bool) string {
	out, _, code, err := dockerOut(ctx, "inspect", "-f",
		`{{index .Config.Labels "sandboxd.runuser"}}`, cid)
	if err == nil && code == 0 {
		v := strings.TrimSpace(out)
		// "" is a recorded root drop; docker renders a missing label as "<no value>".
		if v != "<no value>" {
			return v
		}
	}
	if service {
		return ""
	}
	return sandboxRunUser
}

// SweepUnknown removes sandboxd-labeled containers whose sandbox id is not in known.
func (r *Runtime) SweepUnknown(known []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	keep := make(map[string]bool, len(known))
	for _, id := range known {
		keep[id] = true
	}
	out, _, _, _ := dockerOut(ctx, "ps", "-aq", "--filter", "label=sandboxd=1")
	for _, cid := range strings.Fields(out) {
		id, _, _, _ := dockerOut(ctx, "inspect", "-f", `{{index .Config.Labels "sandboxd.id"}}`, cid)
		if keep[strings.TrimSpace(id)] {
			continue
		}
		_, _, _, _ = dockerOut(ctx, "rm", "-f", cid)
	}
}

// RecoverOrphans removes sandboxd containers and snapshot images left over from
// a previous run; state lives in the store, not in stray containers.
func (r *Runtime) RecoverOrphans() []rt.Instance {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, _, _, _ := dockerOut(ctx, "ps", "-aq", "--filter", "label=sandboxd=1")
	for _, cid := range strings.Fields(out) {
		_, _, _, _ = dockerOut(ctx, "rm", "-f", cid)
	}
	// Both prefixes: a named checkpoint outlives its container the same way a
	// pause snapshot does, and neither survives the daemon that owned it.
	seen := map[string]bool{}
	for _, ref := range []string{"sandboxd-snap-*", "pd-ckpt-*"} {
		imgs, _, _, _ := dockerOut(ctx, "images", "--filter", "reference="+ref, "-q")
		for _, id := range strings.Fields(imgs) {
			if !seen[id] {
				seen[id] = true
				_, _, _, _ = dockerOut(ctx, "rmi", "-f", id)
			}
		}
	}
	return nil
}

// DeleteSnapshot removes the committed snapshot image for sandboxID, if any.
// Best-effort: a never-paused sandbox has no image to remove.
func (r *Runtime) DeleteSnapshot(ctx context.Context, sandboxID string, _ rt.SnapshotRef) error {
	_, _, _, _ = dockerOut(ctx, "rmi", "-f", snapImageTag(sandboxID))
	removeCheckpointImages(ctx, sandboxID)
	return nil
}
