package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// Named snapshots: save a running sandbox under an org-owned name, list what is
// saved, and delete it. A save is asynchronous like an image build — the memfile
// is the guest's whole RAM — so the route answers 202 with a build id and the
// name reports `building` until every object is in the store.

// snapshotPublishTimeout bounds the export-and-publish leg. The same ceiling as
// an image publish: both move multiple gigabytes off a node.
const snapshotPublishTimeout = imagePublishTimeout

func (s *Server) registerSnapshotRoutes(mux *http.ServeMux) {
	// More specific than the /v1/sandboxes/{id}/{rest...} catch-all, so a save
	// is handled here rather than proxied to the node verbatim.
	mux.HandleFunc("POST /v1/sandboxes/{id}/snapshot", s.auth(s.bySlug(s.handleSnapshotSave)))
	mux.HandleFunc("GET /v1/snapshots", s.auth(s.handleSnapshotList))
	mux.HandleFunc("GET /v1/snapshots/{name}", s.auth(s.handleSnapshotGet))
	mux.HandleFunc("DELETE /v1/snapshots/{name}", s.auth(s.handleSnapshotDelete))
}

// snapshotJSON is one version on the wire.
type snapshotJSON struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
	Status  string `json:"status"`
	BuildID string `json:"build_id"`
	Runtime string `json:"runtime"`
	// Kind is "memory" or "disk". Always present: a Docker snapshot is a
	// committed filesystem and nothing else, and a caller must never have to
	// infer from the runtime whether the RAM came with it.
	Kind          string `json:"snapshot_kind"`
	Template      string `json:"template,omitempty"`
	VCPU          int    `json:"vcpu,omitempty"`
	MemMB         int    `json:"mem_mb,omitempty"`
	SourceSandbox string `json:"source_sandbox,omitempty"`
	Size          int64  `json:"size"`
	CreatedAt     int64  `json:"created_at"`
	ReadyAt       int64  `json:"ready_at,omitempty"`
}

func snapshotView(v store.SnapshotView) snapshotJSON {
	return snapshotJSON{
		Name: v.Name, Version: v.Version, Status: v.Status, BuildID: v.BuildID,
		Runtime: v.Runtime, Kind: v.Kind(), Template: v.Template, VCPU: v.VCPU, MemMB: v.MemMB,
		SourceSandbox: v.SourceSandbox, Size: v.Size, CreatedAt: v.CreatedAt, ReadyAt: v.ReadyAt,
	}
}

func snapshotViews(vs []store.SnapshotView) []snapshotJSON {
	out := make([]snapshotJSON, 0, len(vs))
	for _, v := range vs {
		out = append(out, snapshotView(v))
	}
	return out
}

func (s *Server) handleSnapshotList(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	list, err := st.SnapshotsForOrg(r.Context(), fastOrg(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": snapshotViews(list)})
}

func (s *Server) handleSnapshotGet(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	versions, err := st.SnapshotVersions(r.Context(), fastOrg(r), r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(versions) == 0 {
		writeErr(w, http.StatusNotFound, "no snapshot named "+r.PathValue("name"))
		return
	}
	// versions[0] is the newest; the summary reports the newest *ready* one,
	// because that is what a create would boot.
	latest := versions[0]
	for _, v := range versions {
		if v.Status == "ready" {
			latest = v
			break
		}
	}
	body := snapshotView(latest)
	writeJSON(w, http.StatusOK, map[string]any{
		"name": body.Name, "version": body.Version, "status": body.Status,
		"snapshot_kind": body.Kind, "runtime": body.Runtime, "build_id": body.BuildID,
		"template": body.Template, "vcpu": body.VCPU, "mem_mb": body.MemMB,
		"size": body.Size, "created_at": body.CreatedAt, "ready_at": body.ReadyAt,
		"versions": snapshotViews(versions),
	})
}

func (s *Server) handleSnapshotDelete(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	name := r.PathValue("name")
	gone, err := st.DeleteSnapshot(r.Context(), fastOrg(r), name, s.now().Unix())
	if err != nil {
		// A foreign org sees exactly this: the fence is that the name is not theirs.
		if errors.Is(err, store.ErrSnapshotNotFound) {
			writeErr(w, http.StatusNotFound, "no snapshot named "+name)
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	blobs := s.blobStore()
	for _, v := range gone {
		if err := purgeBuild(r.Context(), st, blobs, v.BuildID); err != nil {
			s.log().ErrorContext(r.Context(), "a deleted snapshot version's build could not be purged",
				"component", "artifact", "snapshot", name, "build", v.BuildID, "err", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "deleted_versions": len(gone)})
}

// blobStore returns the shared artifact store, or nil. Unlike artifactStore it
// writes no response: the snapshot paths report the absence themselves.
func (s *Server) blobStore() artifact.StorageProvider {
	if p := s.artifacts.Load(); p != nil && *p != nil {
		return *p
	}
	return nil
}

func (s *Server) handleSnapshotSave(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	blobs := s.blobStore()
	if blobs == nil {
		writeErr(w, http.StatusServiceUnavailable,
			"no artifact store is configured on this control plane (set ORCHESTRATOR_ARTIFACT_DIR), "+
				"so a snapshot could not be shared with the rest of the fleet")
		return
	}
	id := r.PathValue("id")
	// The capture reaches the node as the control plane, which the node does
	// not scope to an org, so ownership is checked here or nowhere.
	_, node, ok := s.movable(r, st, id)
	if !ok {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	name := strings.TrimSpace(body.Name)
	if err := store.ValidSnapshotName(name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	org := fastOrg(r)
	templateID := store.SnapshotTemplateID(org, name)
	now := s.now().Unix()

	// The registry row goes first: it is what claims the version, and the build
	// id is derived from that version so the two orderings cannot disagree.
	snap := store.Snapshot{OrgID: org, Name: name,
		Runtime: node.Runtime, SourceSandbox: id, CreatedAt: now}
	if err := st.SaveSnapshot(r.Context(), &snap); err != nil {
		if errors.Is(err, store.ErrSnapshotName) {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	buildID := snap.BuildID
	// The 'building' row before the 202, so a client that polls the name the
	// moment it gets the build id sees "building" rather than "no such snapshot".
	if err := st.BeginTemplateBuild(r.Context(), store.TemplateBuild{
		BuildID: buildID, TemplateID: templateID, OrgID: org, CreatedAt: now,
	}); err != nil {
		_ = st.MarkSnapshotDeleted(r.Context(), snap.ID, now) //nolint:errcheck // best effort: the row is unreachable either way
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Detached from the caller: a client hanging up must not abandon a capture
	// that has already paused the guest.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), snapshotPublishTimeout)
	go func() {
		defer cancel()
		if err := s.captureSnapshot(ctx, node, id, snap, templateID); err != nil {
			slog.Error("saving a named snapshot failed", "component", "artifact",
				"snapshot", name, "build", buildID, "sandbox", id, "node", node.ID, "err", err)
			_ = st.FailTemplateBuild(ctx, buildID) //nolint:errcheck // best effort: the build is already unreachable, this only makes it legible
		}
	}()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"name": name, "version": snap.Version, "status": "building",
		"build_id": buildID, "runtime": node.Runtime, "snapshot_kind": snap.Kind(),
	})
}

// captureSnapshot runs the whole save after the 202: ask the node to capture,
// stream every object into the store, mark the build ready, then drop the
// node's export directory and the versions this one replaces.
func (s *Server) captureSnapshot(ctx context.Context, node *cluster.Node, sandboxID string,
	snap store.Snapshot, templateID string) error {

	st := s.st.Load()
	blobs := s.blobStore()
	if st == nil || blobs == nil {
		return errors.New("the control plane lost its database or artifact store mid-save")
	}
	capt, err := s.nodeSnapshotExport(ctx, node, sandboxID, snap.BuildID)
	if err != nil {
		return err
	}
	// The export directory is the node's, and it is the size of the guest's RAM.
	defer func() {
		if err := s.nodeSnapshotExportDelete(context.WithoutCancel(ctx), node, sandboxID, snap.BuildID); err != nil {
			slog.Warn("a snapshot export directory was left on the node", "component", "artifact",
				"node", node.ID, "sandbox", sandboxID, "build", snap.BuildID, "err", err)
		}
	}()

	// The row carries what the capture reported before the publish marks it
	// ready, so a create can be sized and pinned without reading the object
	// store. After it, a create landing between the ready mark and this write
	// found a ready snapshot with no template or size.
	if err := st.UpdateSnapshotCapture(ctx, snap.ID, capt.Template, capt.VCPU, capt.MemMB, capt.CPUFeatures); err != nil {
		return err
	}

	if capt.Payload == artifact.NameImage {
		// Docker: the capture is a committed image, which is exactly what the
		// image publish already writes. Disk only — there is no memory file.
		// Pulled with the image export this tier already has rather than through
		// the snapshot-export API, because `docker save` is what produces the
		// bytes and the node already serves that.
		if capt.Tag == "" {
			return fmt.Errorf("node %s committed a docker snapshot but named no image", node.ID)
		}
		body, err := s.exportImage(ctx, node, capt.Tag, artifact.NameImage)
		if err != nil {
			return err
		}
		defer func() { _ = body.Close() }() //nolint:errcheck // response fully consumed by publishBuild
		if _, err := publishBuild(ctx, st, blobs, publishSpec{
			TemplateID: templateID, BuildID: snap.BuildID, OrgID: snap.OrgID,
			Payload: artifact.NameImage, Body: body, Size: -1, Now: s.now().Unix(), RowBegun: true,
		}); err != nil {
			return err
		}
	} else {
		if err := publishSnapshotBuild(ctx, st, blobs, snapshotPublishSpec{
			TemplateID: templateID, BuildID: snap.BuildID, OrgID: snap.OrgID,
			Meta: artifact.SnapshotMeta{
				Runtime: node.Runtime, Template: capt.Template, VCPU: capt.VCPU, MemMB: capt.MemMB,
				SourceSandbox: sandboxID, TapName: capt.TapName, VMIP: capt.VMIP,
			},
			Open: func(ctx context.Context, name string) (io.ReadCloser, error) {
				return s.nodeSnapshotObject(ctx, node, sandboxID, snap.BuildID, name)
			},
			Now: s.now().Unix(),
		}); err != nil {
			return err
		}
	}

	// Only now: older versions stayed bootable for the whole publish, so a
	// create during the save never fell through to nothing.
	s.purgeSupersededSnapshots(ctx, st, blobs, snap)
	return nil
}

// purgeSupersededSnapshots removes every older live version of the name.
func (s *Server) purgeSupersededSnapshots(ctx context.Context, st *store.Store,
	blobs artifact.StorageProvider, snap store.Snapshot) {

	old, err := st.SupersededSnapshots(ctx, snap.OrgID, snap.Name, snap.ID)
	if err != nil {
		slog.Warn("could not list the snapshot versions this save replaces; their objects stay",
			"component", "artifact", "snapshot", snap.Name, "err", err)
		return
	}
	for _, v := range old {
		if err := purgeBuild(ctx, st, blobs, v.BuildID); err != nil {
			slog.Warn("a superseded snapshot version could not be purged", "component", "artifact",
				"snapshot", snap.Name, "version", v.Version, "err", err)
			continue
		}
		if err := st.MarkSnapshotDeleted(ctx, v.ID, s.now().Unix()); err != nil {
			slog.Warn("a superseded snapshot version's row could not be marked deleted",
				"component", "artifact", "snapshot", snap.Name, "version", v.Version, "err", err)
		}
	}
}

// snapshotCapture is the node's answer to the capture request.
// Only the fields this tier acts on; the node reports more (paused_ms,
// size_bytes, kind) and unknown fields decode away.
type snapshotCapture struct {
	// Payload is "snapshot" (memory + state + disk) or "image.tar" (Docker,
	// disk only). The node says which; the control plane never assumes.
	Payload string `json:"payload"`
	// Tag is the committed image, on a runtime whose capture is one.
	Tag      string `json:"tag"`
	Template string `json:"template"`
	VCPU     int    `json:"vcpu"`
	MemMB    int    `json:"mem_mb"`
	TapName  string `json:"tap_name"`
	VMIP     string `json:"vm_ip"`
	// CPUFeatures is the capturing host's dispatch-relevant instruction set.
	// Recorded so placement can keep a memory image off a node that would kill
	// the guest with SIGILL; absent from a node that predates the record.
	CPUFeatures []string `json:"cpu_features"`
}

// nodeSnapshotRequest issues one authenticated request to a node's snapshot
// export API. Its own client, not forward: a capture writes the guest's whole
// RAM to disk and a transfer moves it, so both outlast the ordinary node
// timeout, and neither may be replayed after a transport error.
func (s *Server) nodeSnapshotRequest(ctx context.Context, node *cluster.Node,
	method, path string, body []byte, timeout time.Duration) (*http.Response, error) {

	base, hop, err := s.nodeHopLong(node, timeout)
	if err != nil {
		return nil, err
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if node.Key != "" {
		req.Header.Set("Authorization", "Bearer "+node.Key)
	}
	resp, err := hop.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s on %s: %w", method, path, node.ID, err)
	}
	return resp, nil
}

// snapshotExportPath is the node route one build's export lives under.
func snapshotExportPath(sandboxID, buildID string) string {
	return "/v1/sandboxes/" + url.PathEscape(sandboxID) + "/snapshot-export/" + url.PathEscape(buildID)
}

// nodeSnapshotExport asks the node to capture the sandbox into an export
// directory named for the build. The sandbox keeps running.
func (s *Server) nodeSnapshotExport(ctx context.Context, node *cluster.Node, sandboxID, buildID string) (snapshotCapture, error) {
	var out snapshotCapture
	body, err := json.Marshal(map[string]string{"build_id": buildID})
	if err != nil {
		return out, err
	}
	resp, err := s.nodeSnapshotRequest(ctx, node,
		"POST", "/v1/sandboxes/"+url.PathEscape(sandboxID)+"/snapshot-export", body, snapshotPublishTimeout)
	if err != nil {
		return out, err
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // body read below
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return out, fmt.Errorf("capture %s on %s: %w", sandboxID, node.ID, err)
	}
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("capture %s on %s: HTTP %d %s",
			sandboxID, node.ID, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("capture %s on %s: unreadable response: %w", sandboxID, node.ID, err)
	}
	if out.Payload == "" {
		return out, fmt.Errorf("node %s did not say what kind of capture it took; a snapshot of "+
			"unknown payload cannot be published", node.ID)
	}
	return out, nil
}

// nodeSnapshotObject streams one captured object off the node, the way
// exportImage streams a built rootfs.
func (s *Server) nodeSnapshotObject(ctx context.Context, node *cluster.Node, sandboxID, buildID, name string) (io.ReadCloser, error) {
	resp, err := s.nodeSnapshotRequest(ctx, node,
		"GET", snapshotExportPath(sandboxID, buildID)+"/"+url.PathEscape(name), nil, snapshotPublishTimeout)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096)) //nolint:errcheck // best effort: the status is the error
		_ = resp.Body.Close()                               //nolint:errcheck // discarding a failed response
		return nil, fmt.Errorf("read %s of %s from %s: HTTP %d %s",
			name, buildID, node.ID, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return resp.Body, nil
}

// nodeSnapshotExportDelete drops the node's export directory.
func (s *Server) nodeSnapshotExportDelete(ctx context.Context, node *cluster.Node, sandboxID, buildID string) error {
	resp, err := s.nodeSnapshotRequest(ctx, node, "DELETE", snapshotExportPath(sandboxID, buildID), nil, 30*time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // nothing in the body matters
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
