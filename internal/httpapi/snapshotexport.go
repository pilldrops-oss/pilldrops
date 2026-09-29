package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// The node's half of a named snapshot save. The control plane cannot reach a
// node's disk, so it asks for a capture, streams the objects off, and asks for
// the capture to be dropped. Nothing here publishes anything: the node's
// database role is SELECT-only on template_builds, and a node that could
// publish would turn one compromise into fleet-wide code execution.
//
// The capture itself is an ordinary named checkpoint. There is deliberately no
// second capture path — a snapshot save and a checkpoint save are the same
// engine call, and the only difference is who keeps the result.

// snapshotExportName is the checkpoint name one export uses. Derived from the
// build id so two concurrent saves of one sandbox cannot collide, and carrying
// the reserved prefix that keeps it out of the owner's save points: it is not a
// checkpoint anyone took, so it neither lists, nor counts against the cap, nor
// answers to the checkpoint verbs.
func snapshotExportName(buildID string) string {
	return daemon.ExportDirPrefix + strings.ToLower(buildID)
}

// exportObjectFile maps an artifact object name to the file the capture wrote.
// The artifact layer's names are the store's; the engine's are its own, and
// this is the one place the two meet.
func exportObjectFile(name string) (string, bool) {
	switch name {
	case artifact.NameSnapfile:
		return "state", true
	case artifact.NameMemfile:
		return "mem", true
	case artifact.NameOverlay:
		return "overlay.ext4", true
	case artifact.NameSnapMeta:
		// The engine's own capture record, served verbatim. Nothing between the
		// node that writes it and the node that restores it parses it.
		return "meta.json", true
	}
	return "", false
}

// exportRootfsPath is the read-only image a capture was taken against, which
// unlike the other objects does not live in the capture directory: it is the
// template's image, shared by every sandbox of that template on this node and
// far too large to copy into every capture.
//
// The template name comes out of the capture's own meta.json rather than the
// sandbox record, because what has to travel is the image the CAPTURE was taken
// against. It is this node's own writing, but it is still validated: it is
// about to be joined onto a path that is then served.
func exportRootfsPath(captureDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(captureDir, "meta.json"))
	if err != nil {
		return "", fmt.Errorf("read the capture record: %w", err)
	}
	var ident capturedIdentity
	if err := json.Unmarshal(raw, &ident); err != nil {
		return "", fmt.Errorf("parse the capture record: %w", err)
	}
	if err := rt.ValidateTemplateName(ident.Template); err != nil {
		return "", err
	}
	dir := templates.Dir()
	if dir == "" {
		return "", errors.New("this node has no image directory, so it cannot serve a root image")
	}
	return filepath.Join(dir, ident.Template, "rootfs.squashfs"), nil
}

func (s *Server) registerSnapshotExportRoutes(route func(string, http.Handler)) {
	route("POST /v1/sandboxes/{id}/snapshot-export", s.auth(s.handleSnapshotExport))
	route("GET /v1/sandboxes/{id}/snapshot-export/{build}/{name}", s.auth(s.handleSnapshotExportObject))
	route("DELETE /v1/sandboxes/{id}/snapshot-export/{build}", s.auth(s.handleSnapshotExportDelete))
}

// capturedIdentity is the part of the engine's capture record this layer reads,
// and it reads it only to report it. The restore takes the whole file verbatim;
// nothing here is a second source of truth for it.
type capturedIdentity struct {
	TapName string `json:"tap_name"`
	// Template names the read-only image the capture was taken against, so the
	// rootfs object can be served from where this node keeps it.
	Template string `json:"template"`
	// "vmip", not "vm_ip": these names are the engine's, and this struct has to
	// match what it wrote rather than what reads well here. A mismatch is
	// silent — the field just comes back empty — so it is asserted live.
	VMIP string `json:"vmip"`
	// CPUFeatures is the capturing host's dispatch-relevant instruction set.
	// Reported so the control plane can record it on the snapshot and place a
	// restore where the guest's frozen code paths actually run; the engine's
	// own check is the backstop. Absent on a capture taken before it was
	// recorded.
	CPUFeatures []string `json:"cpu_features"`
}

// handleSnapshotExport captures the sandbox for the named build. The sandbox
// keeps running: this is `docker commit` on Docker and a live memory capture on
// Firecracker, neither of which stops it for more than the write.
func (s *Server) handleSnapshotExport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	var body struct {
		BuildID string `json:"build_id"`
	}
	if !decodeBody(r, &body) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := artifact.ValidateBuildID(body.BuildID); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	info, ok := s.d.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	c, err := s.d.Checkpoint(r.Context(), id, snapshotExportName(body.BuildID))
	if err != nil {
		checkpointErr(w, err)
		return
	}

	// payload says which shape the control plane should publish. "snapshot" is
	// the three-object memory capture; "image.tar" is a committed filesystem
	// and carries no memory image at all, and the control plane must be told
	// rather than left to guess from the runtime's name.
	payload := artifact.PayloadSnapshot
	if c.Kind != "memory" {
		payload = artifact.NameImage
	}
	resp := map[string]any{
		"paused_ms": 0, "size_bytes": c.SizeBytes, "payload": payload,
		"kind": c.Kind, "template": info.Template,
		"vcpu": info.VCPU, "mem_mb": info.MemMiB,
	}
	if c.PausedMs > 0 {
		resp["paused_ms"] = c.PausedMs
	}
	dir := s.d.CheckpointDir(id, snapshotExportName(body.BuildID))
	if payload == artifact.NameImage {
		// Docker: the bytes are an image, and the control plane pulls it with
		// the image export it already has rather than through this API.
		if tag, err := os.ReadFile(filepath.Join(dir, "tag")); err == nil {
			resp["tag"] = strings.TrimSpace(string(tag))
		}
	} else if raw, err := os.ReadFile(filepath.Join(dir, "meta.json")); err == nil {
		var ident capturedIdentity
		if json.Unmarshal(raw, &ident) == nil {
			resp["tap_name"], resp["vm_ip"] = ident.TapName, ident.VMIP
			resp["cpu_features"] = ident.CPUFeatures
		}
	}
	s.auditDetail(r, "sandbox.snapshot.export", id, map[string]any{
		"build_id": body.BuildID, "kind": c.Kind, "size_bytes": c.SizeBytes,
	})
	writeJSON(w, http.StatusOK, resp)
}

// handleSnapshotExportObject streams one captured object. Content-Length is set
// from the file, so a truncated transfer is a transport error rather than a
// short object the publisher would happily store.
func (s *Server) handleSnapshotExportObject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	if err := artifact.ValidateBuildID(r.PathValue("build")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	dir := s.d.CheckpointDir(id, snapshotExportName(r.PathValue("build")))
	var path string
	if r.PathValue("name") == artifact.NameRootfs {
		p, err := exportRootfsPath(dir)
		if err != nil {
			writeErr(w, http.StatusNotFound, "root image for this capture: "+err.Error())
			return
		}
		path = p
	} else {
		file, ok := exportObjectFile(r.PathValue("name"))
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Sprintf("%q is not part of a snapshot export", r.PathValue("name")))
			return
		}
		path = filepath.Join(dir, file)
	}
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such export object")
		return
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // read-only
	fi, err := f.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_ = httpx.CopyBody(w, r, fi.Size(), f) //nolint:errcheck // status is committed; a short write is the client's to notice
}

// handleSnapshotExportDelete drops the capture. It is the size of the guest's
// RAM, so this is not optional housekeeping.
func (s *Server) handleSnapshotExportDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	err := s.d.RemoveCheckpoint(id, snapshotExportName(r.PathValue("build")))
	if err != nil && !isNotFound(err) {
		writeErr(w, statusForErr(err), err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func isNotFound(err error) bool { return errors.Is(err, rt.ErrNotFound) }
