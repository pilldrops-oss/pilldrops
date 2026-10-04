package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// Moving a sandbox to another node under the same id: capture it as a named
// snapshot, create from that snapshot elsewhere with the id pinned, then evict
// the original. The id, its row (name, slug, env, owner, timers), its routes
// and its history all stay the sandbox's; only the node changes.
//
// The snapshot is kept, named move-<id>-<nonce>: the moved sandbox runs from
// it as its template, and a later resume or restart on the new node reads it
// again. Each move's name is new, so its template never lands on a node's copy
// of an earlier move's capture that a fork still runs on.
//
// ponytail: the source keeps running until the copy is up, so writes made to
// it after the capture are lost; a pause before the capture closes that at the
// cost of downtime. Firecracker carries memory across; Docker carries the disk
// and relaunches the app.

// moveFetchWait bounds how long a move waits for the target node to download
// the capture; moveFetchPoll is how often it asks. A memory image is the
// guest's whole RAM, so minutes are normal on a first move to a node.
const (
	moveFetchWait = 10 * time.Minute
	moveFetchPoll = 3 * time.Second
)

func (s *Server) registerMoveRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/sandboxes/{id}/move", s.auth(s.bySlug(s.handleMove)))
}

// moveResult is what a move reports.
type moveResult struct {
	ID    string `json:"id"`
	From  string `json:"from"`
	To    string `json:"to,omitempty"`
	Error string `json:"error,omitempty"`
	// Code is set with Error when the caller can act on it: move_in_progress.
	Code string `json:"code,omitempty"`
	// SourceLeft is set when the copy is live but the original could not be
	// evicted: both run until the source node's copy is removed by hand.
	SourceLeft bool `json:"source_left,omitempty"`
}

func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	if s.blobStore() == nil {
		writeErr(w, http.StatusServiceUnavailable,
			"no artifact store is configured on this control plane (set ORCHESTRATOR_ARTIFACT_DIR), "+
				"so a sandbox's state has nowhere to travel through")
		return
	}
	id := r.PathValue("id")
	rec, from, ok := s.movable(r, st, id)
	if !ok {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	res := s.moveSandbox(r.Context(), st, rec, from)
	switch {
	case res.Code == codeMoveInProgress:
		writeJSON(w, http.StatusConflict, res)
	case res.Error != "":
		writeJSON(w, http.StatusConflict, res)
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

// movable resolves a sandbox the caller may move: routed, in their org, and
// visible to them under the same rule as the list.
func (s *Server) movable(r *http.Request, st *store.Store, id string) (store.SandboxRecord, *cluster.Node, bool) {
	from, ok := s.reg.NodeFor(id)
	if !ok {
		return store.SandboxRecord{}, nil, false
	}
	rec, found, err := st.SandboxByID(r.Context(), id)
	if err != nil || !found || rec.OrgID != fastOrg(r) {
		return store.SandboxRecord{}, nil, false
	}
	keyID, keyRole := callerKey(r.Context())
	if !sandbox.Visible(rec.ID, rec.Visibility, rec.CreatedBy, keyID, keyRole == "operator") {
		return store.SandboxRecord{}, nil, false
	}
	return rec, from, true
}

// moveSandbox does the three steps. It never leaves the sandbox without a
// running copy: a failed capture or create leaves the original where it was.
func (s *Server) moveSandbox(ctx context.Context, st *store.Store, rec store.SandboxRecord, from *cluster.Node) moveResult {
	res := moveResult{ID: rec.ID, From: from.ID}
	org := rec.OrgID
	// One move per sandbox, and no destroy while it runs (handleDestroy checks
	// this lock): a destroy between the capture and the copy's create would
	// otherwise leave the copy running under an id its owner already deleted.
	release, ok := s.locks.TryLock(moveLockName(rec.ID), moveLockTTL)
	if !ok {
		res.Error, res.Code = "a move of this sandbox is already in progress", codeMoveInProgress
		return res
	}
	defer release()
	fail := func(step string, err error) moveResult {
		res.Error = step + ": " + err.Error()
		s.log().Warn("sandbox move failed; it stays where it was", "component", "move",
			"sandbox", rec.ID, "from", from.ID, "step", step, "err", err)
		return res
	}

	// A paused sandbox has nothing running to capture: wake it for the move and
	// put it back to sleep on the other side.
	wasPaused := rec.State == "paused"
	if wasPaused {
		if err := s.nodeCall(ctx, from, http.MethodPost, "/v1/sandboxes/"+rec.ID+"/resume", org); err != nil {
			return fail("resume", err)
		}
	}

	// 1. Capture into the artifact store, synchronously.
	name := store.MoveSnapshotName(rec.ID) + "-" + randHex4()
	snap := store.Snapshot{OrgID: org, Name: name, Runtime: from.Runtime,
		SourceSandbox: rec.ID, CreatedAt: s.now().Unix()}
	if err := st.SaveSnapshot(ctx, &snap); err != nil {
		return fail("snapshot", err)
	}
	templateID := store.SnapshotTemplateID(org, name)
	if err := st.BeginTemplateBuild(ctx, store.TemplateBuild{
		BuildID: snap.BuildID, TemplateID: templateID, OrgID: org, CreatedAt: snap.CreatedAt,
	}); err != nil {
		return fail("snapshot", err)
	}
	capCtx, cancel := context.WithTimeout(ctx, snapshotPublishTimeout)
	err := s.captureSnapshot(capCtx, from, rec.ID, snap, templateID)
	cancel()
	if err != nil {
		_ = st.FailTemplateBuild(context.WithoutCancel(ctx), snap.BuildID) //nolint:errcheck // best effort: the build is unreachable either way
		return fail("snapshot", err)
	}

	// 2. Create from it on another node, under the same id. The create path
	// does the snapshot rewrite, placement and routing; the move context lets
	// the id through and keeps it off the source node.
	body, _ := json.Marshal(map[string]string{"snapshot": name, "id": rec.ID})
	createCtx := context.WithValue(withOrg(withOperator(ctx), org), moveCtxKey{}, moveCtx{from: from.ID})
	// The target has to download the capture before it can boot it, so its
	// first answer is usually "still fetching, retry". That is progress, not a
	// failure: keep asking until the download lands or the deadline passes.
	deadline := s.now().Add(moveFetchWait)
	var rw *httptest.ResponseRecorder
	for {
		req := httptest.NewRequestWithContext(createCtx, http.MethodPost, "/v1/sandboxes", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rw = httptest.NewRecorder()
		s.handleCreate(rw, req)
		fetching := rw.Code == http.StatusServiceUnavailable && strings.Contains(rw.Body.String(), `"template_fetching"`)
		if !fetching || s.now().After(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(moveFetchPoll):
		}
	}
	if rw.Code != http.StatusOK && rw.Code != http.StatusCreated {
		return fail("create", errors.New(strings.TrimSpace(rw.Body.String())))
	}
	to, ok := s.reg.NodeFor(rec.ID)
	if !ok || to.ID == from.ID {
		return fail("create", errors.New("the copy was not placed on another node"))
	}
	res.To = to.ID

	// 3. Evict the original. The copy is live and routed from here on.
	if err := s.nodeCall(ctx, from, http.MethodDelete, "/v1/sandboxes/"+rec.ID+"?evict=1", org); err != nil {
		res.SourceLeft = true
		s.log().Error("sandbox moved but its original could not be evicted; both run",
			"component", "move", "sandbox", rec.ID, "from", from.ID, "to", to.ID, "err", err)
	}
	if wasPaused {
		if err := s.nodeCall(ctx, to, http.MethodPost, "/v1/sandboxes/"+rec.ID+"/pause", org); err != nil {
			s.log().Warn("moved a paused sandbox but could not pause it again; it is running",
				"component", "move", "sandbox", rec.ID, "to", to.ID, "err", err)
		}
	}
	s.log().Info("sandbox moved", "component", "move", "sandbox", rec.ID, "from", from.ID, "to", to.ID)
	return res
}

const codeMoveInProgress = "move_in_progress"

// shownTemplate is the template a sandbox is listed under. A moved sandbox
// boots from its move snapshot's template id, which names the capture and not
// what the owner created; it is shown as the template it ran before its first
// move, which each move's capture records as the template it was taken from.
func shownTemplate(ctx context.Context, st *store.Store, rec store.SandboxRecord) string {
	shown := rec.Template
	moves := store.SnapshotTemplateID(rec.OrgID, store.MoveSnapshotName(rec.ID))
	for strings.HasPrefix(shown, moves) {
		name := strings.TrimPrefix(shown, store.SnapshotTemplateID(rec.OrgID, ""))
		t, err := st.FirstSnapshotTemplate(ctx, rec.OrgID, name)
		if err != nil {
			break
		}
		shown = t
	}
	return shown
}

// moveLockTTL outlasts the longest move: the capture's deadline plus the wait
// for the target's fetch. A holder that dies frees it when this runs out.
const moveLockTTL = snapshotPublishTimeout + moveFetchWait + 5*time.Minute

func moveLockName(id string) string { return "move:" + id }

// nodeCall sends one bodiless request to a node and turns a non-2xx into an error.
func (s *Server) nodeCall(ctx context.Context, node *cluster.Node, method, path, org string) error {
	resp, err := s.forward(ctx, node, method, path, nil, "", org)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best effort
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}
