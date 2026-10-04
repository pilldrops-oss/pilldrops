package docker

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// Restore boots the sandbox from its committed snapshot image, falling back to
// the template base image if the snapshot is missing.
func (r *Runtime) Restore(ctx context.Context, snap rt.SnapshotRef, spec rt.BootSpec) (rt.Instance, error) {
	img := spec.Template.Image
	if img == "" {
		img = imageFor(spec.Template.Name)
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
	return r.bootImage(ctx, spec, img)
}

// Adopt reattaches to the container a previous daemon process left for
// sandboxID, matched by the sandboxd.id label. A stopped one adopts too, marked
// a casualty for the daemon to start again: a host reboot stops every container,
// and one not adopted was swept, its writable layer with it. A container with no
// published port still adopts.
func (r *Runtime) Adopt(ctx context.Context, sandboxID string) (rt.Instance, error) {
	out, se, code, err := dockerOut(ctx, "ps", "-a",
		"--filter", "label=sandboxd.id="+sandboxID, "--format", "{{.ID}} {{.State}}")
	if err != nil || code != 0 {
		return nil, dockerErr("docker ps", err, code, se)
	}
	cid, live := pickAdoptable(out)
	if cid == "" {
		return nil, rt.ErrNotFound
	}
	primary := adoptedPrimaryPort(ctx, cid)
	host, _ := mappedPort(ctx, cid, primary)
	termHost, _ := mappedPort(ctx, cid, terminalPort)
	cmdOut, _, _, _ := dockerOut(ctx, "inspect", "-f", "{{join .Config.Cmd \" \"}}", cid)
	service := !strings.Contains(cmdOut, "sleep infinity")
	inst := &dockerInstance{
		id: sandboxID, cid: cid, host: host, termHost: termHost, service: service,
		runUser: r.adoptedRunUser(ctx, cid, service), start: time.Now(),
		eng:     r.eng,
		primary: primary,
		ports:   map[int]string{primary: host},
	}
	inst.casualty.Store(!live)
	return inst, nil
}

var _ rt.Adopter = (*Runtime)(nil)

// pickAdoptable picks from `ps -a` lines ("<id> <state>") a live container
// first, else any. A docker-paused one is live: it is a standby sandbox, thawed
// by the daemon, and `docker start` refuses it.
func pickAdoptable(psOut string) (cid string, live bool) {
	for _, line := range strings.Split(psOut, "\n") {
		id, state, _ := strings.Cut(strings.TrimSpace(line), " ")
		if id == "" {
			continue
		}
		if state == "running" || state == "paused" {
			return id, true
		}
		if cid == "" {
			cid = id
		}
	}
	return cid, false
}

// primaryPortLabel records a container's primary port for Adopt.
const primaryPortLabel = "sandboxd.port"

// adoptedPrimaryPort reads the primary port bootImage labelled the container
// with; a container booted before the label falls back to appPort.
func adoptedPrimaryPort(ctx context.Context, cid string) int {
	out, _, code, err := dockerOut(ctx, "inspect", "-f", `{{index .Config.Labels "`+primaryPortLabel+`"}}`, cid)
	if err == nil && code == 0 {
		if p, perr := strconv.Atoi(strings.TrimSpace(out)); perr == nil && p > 0 {
			return p
		}
	}
	return appPort
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
		id, _, code, err := dockerOut(ctx, "inspect", "-f", `{{index .Config.Labels "sandboxd.id"}}`, cid)
		// An unreadable label is not an unknown sandbox: rm -f is unrecoverable.
		if err != nil || code != 0 || keep[strings.TrimSpace(id)] {
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
