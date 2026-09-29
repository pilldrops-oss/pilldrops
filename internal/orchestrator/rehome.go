package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// A paused sandbox outlives its node when the node archives pause snapshots to
// the bucket (SNAPSHOT_ARCHIVE=bucket): once its node is reaped, the first
// request for it rehomes it onto a live node, which pulls the snapshot and
// takes the record, and the request carries on there. Lazily, as E2B resumes:
// a fleet of paused sandboxes is not re-downloaded because one node died.

// rehomeWait bounds a rehome: the snapshot download is the whole of it.
const rehomeWait = 10 * time.Minute

// orphanedPaused returns the caller's paused row for id when its node is no
// longer in the fleet, under the same org and visibility rule as the list.
func (s *Server) orphanedPaused(r *http.Request, id string) (store.SandboxRecord, bool) {
	st := s.st.Load()
	if st == nil {
		return store.SandboxRecord{}, false
	}
	rec, ok, err := st.SandboxByID(r.Context(), id)
	if err != nil || !ok || rec.State != "paused" || rec.OrgID != fastOrg(r) {
		return store.SandboxRecord{}, false
	}
	if _, alive := s.reg.Node(rec.NodeID); alive {
		return store.SandboxRecord{}, false // its node is here: not orphaned, just unrouted
	}
	keyID, keyRole := callerKey(r.Context())
	if !sandbox.Visible(rec.ID, rec.Visibility, rec.CreatedBy, keyID, keyRole == "operator") {
		return store.SandboxRecord{}, false
	}
	return rec, true
}

// rehome places an orphaned paused sandbox on a live node. It answers the
// request itself and returns false when it cannot.
func (s *Server) rehome(w http.ResponseWriter, r *http.Request, rec store.SandboxRecord) (*cluster.Node, bool) {
	release, ok := s.locks.TryLock("rehome:"+rec.ID, rehomeWait)
	if !ok {
		w.Header().Set("Retry-After", "10")
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "this sandbox's node is gone and it is being restored on another node; retry shortly",
			"code":  "sandbox_rehoming",
		})
		return nil, false
	}
	defer release()
	ctx, cancel := context.WithTimeout(withOperator(r.Context()), rehomeWait)
	defer cancel()

	place := cluster.Request{VCPU: rec.VCPU, MemMB: rec.MemMiB, Template: rec.Template}
	var lastErr error
	for tries := 0; tries < maxCreateTries; tries++ {
		node := s.reg.PickAndReserve(place, s.now(), cluster.NodeTTL)
		if node == nil {
			break
		}
		// Paused, it holds no memory; the hold only kept placement honest while asking.
		s.reg.Release(node.ID, rec.MemMiB)
		err := s.nodeCall(ctx, node, http.MethodPost, "/v1/sandboxes/"+rec.ID+"/rehome", rec.OrgID)
		if err == nil {
			s.reg.Place(rec.ID, node.ID)
			s.log().Info("rehomed a paused sandbox whose node is gone", "component", "rehome",
				"sandbox", rec.ID, "from", rec.NodeID, "to", node.ID)
			return node, true
		}
		lastErr = err
		place.Avoid = append(place.Avoid, node.ID)
		s.log().Warn("a node could not rehome a paused sandbox", "component", "rehome",
			"sandbox", rec.ID, "node", node.ID, "err", err)
	}
	if lastErr == nil {
		lastErr = errors.New("no live node can take it")
	}
	// No node found an archive: the sandbox went with its node after all.
	if st := s.st.Load(); st != nil && strings.Contains(lastErr.Error(), "no archived snapshot") {
		_ = st.MarkSandboxLost(context.WithoutCancel(ctx), rec.ID) //nolint:errcheck // best effort: the answer below says it either way
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{
		"error": "sandbox " + rec.ID + " was paused on a node that is gone and could not be restored " +
			"elsewhere: " + lastErr.Error(),
		"code": "sandbox_rehome_failed",
	})
	return nil, false
}
