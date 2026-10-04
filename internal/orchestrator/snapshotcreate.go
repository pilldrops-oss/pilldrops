package orchestrator

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// Creating from a snapshot. The whole thing is a rewrite of the create body
// before admission: `{"snapshot":"golden"}` becomes a create of the snapshot's
// own template id at the snapshot's own size, on the snapshot's own runtime.
// Nothing downstream — admission, placement, the node — learns a new concept.

// snapshotCreate is a create rewritten to boot from a snapshot.
type snapshotCreate struct {
	// Body carries the snapshot's template id, fixed size and runtime, so
	// admission, placement and the node all see the same request.
	Body []byte
	View store.SnapshotView
}

// resolveSnapshotCreate resolves the named snapshot and rewrites the body.
// It runs before admission, so the plan is charged for the size the snapshot
// will actually take rather than for the caller's silence.
//
// Returns ok=false after answering; nothing further may be written.
func (s *Server) resolveSnapshotCreate(w http.ResponseWriter, r *http.Request, body []byte,
	req sandbox.Spec) (snapshotCreate, bool) {

	var out snapshotCreate
	if req.Template != "" {
		writeJSONErr(w, http.StatusBadRequest, "snapshot_and_template",
			"a create names either a template or a snapshot, not both: a snapshot already carries "+
				"the template it was captured from")
		return out, false
	}
	st := s.st.Load()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable,
			"the control plane's database connection is not up, so no snapshot can be resolved")
		return out, false
	}
	view, err := st.LatestReadySnapshot(r.Context(), fastOrg(r), req.Snapshot)
	// A move's capture is the moved sandbox's root image, not the org's to boot;
	// only this tier's own move creates from it.
	_, moving := r.Context().Value(moveCtxKey{}).(moveCtx)
	if err == nil && store.ReservedSnapshotName(req.Snapshot) && !moving {
		err = store.ErrSnapshotNotFound
	}
	if err != nil {
		if errors.Is(err, store.ErrSnapshotNotFound) {
			// One answer for "never existed", "still building" and "another
			// org's": telling them apart would report on snapshots the caller
			// is not allowed to know exist.
			writeJSONErr(w, http.StatusNotFound, "snapshot_not_ready",
				"no ready snapshot named "+req.Snapshot+
					" — it may still be building, or may not exist; poll GET /v1/snapshots/"+req.Snapshot+
					` until its status is "ready"`)
			return out, false
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return out, false
	}

	// Firecracker restores into the shape it snapshotted; there is no resize.
	// Refused rather than quietly corrected: a caller that asked for 4 GiB and
	// was handed 2 has been given a machine that will run out of memory.
	if (req.VCPU > 0 && req.VCPU != view.VCPU) || (req.MemoryMB > 0 && req.MemoryMB != view.MemMB) {
		asked := ""
		if req.VCPU > 0 && req.VCPU != view.VCPU {
			asked = fmt.Sprintf("%d vCPU", req.VCPU)
		}
		if req.MemoryMB > 0 && req.MemoryMB != view.MemMB {
			if asked != "" {
				asked += " and "
			}
			asked += fmt.Sprintf("%d MB of memory", req.MemoryMB)
		}
		writeJSONErr(w, http.StatusBadRequest, "snapshot_size_fixed",
			fmt.Sprintf("snapshot %s was captured at %d vCPU and %d MB of memory and restores only "+
				"into that shape; this request asked for %s",
				view.Name, view.VCPU, view.MemMB, asked))
		return out, false
	}
	// A memory image is meaningless on another runtime, so the pin is a refusal
	// rather than an override.
	if rt := wantRuntime(req.Runtime, req.Isolation); rt != "" && view.Runtime != "" && rt != view.Runtime {
		writeJSONErr(w, http.StatusBadRequest, "snapshot_runtime_fixed",
			fmt.Sprintf("snapshot %s was captured on the %s runtime and cannot be restored under %s",
				view.Name, view.Runtime, rt))
		return out, false
	}

	out.View = view
	// `snapshot` stays on the body: it is what tells the node's daemon to wake
	// the machine rather than boot the template and run the start command.
	// No runtime field: the node knows its own, and placement is what has to be
	// pinned — handleCreate does that from View.Runtime.
	out.Body = injectJSONFields(body, map[string]any{
		"template":  store.SnapshotTemplateID(view.OrgID, view.Name),
		"snapshot":  view.Name,
		"vcpu":      view.VCPU,
		"memory_mb": view.MemMB,
	})
	return out, true
}

// writeJSONErr answers with a message and a stable machine-readable code.
func writeJSONErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}
