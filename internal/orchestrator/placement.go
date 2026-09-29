package orchestrator

import (
	"cmp"
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// RebuildPlacements records the placement of every sandbox each healthy node
// reports and returns the count. Additive only: a briefly unreachable node
// must not orphan its sandboxes, so removal happens only on DELETE.
func (s *Server) RebuildPlacements(now time.Time) int {
	// In parallel: one call per node, each up to 3 s, so a serial loop let a
	// few slow nodes stretch every tick past its 10 s interval.
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, rebuildFanout)
	count := 0
	for _, node := range s.reg.Healthy(now) {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			inv := s.nodeInventory(node)
			ids := make([]string, 0, len(inv))
			for id := range inv {
				s.reg.Place(id, node.ID)
				ids = append(ids, id)
			}
			// A node reaped during a partition and back now: its sandboxes
			// still run, so the rows the reap marked lost take back the state
			// the node reports.
			if st := s.st.Load(); st != nil {
				if n, err := st.ReviveLost(context.Background(), node.ID, inv); err == nil && n > 0 {
					s.log().Info("sandboxes back with their node", "component", "placement",
						"node", node.ID, "count", n)
				}
			}
			mu.Lock()
			count += len(ids)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return count
}

// rebuildFanout caps concurrent inventory calls from one reconcile pass.
const rebuildFanout = 32

// nodeInventory asks one node for every sandbox it holds across all orgs.
// GET /v1/sandboxes is scoped to the caller's org and would miss customer
// sandboxes; the admin list is org-blind. Falls back for nodes without it.
func (s *Server) nodeInventory(node *cluster.Node) map[string]string {
	if inv, ok := s.listSandboxIDs(node, "/v1/admin/sandboxes"); ok {
		return inv
	}
	inv, _ := s.listSandboxIDs(node, "/v1/sandboxes")
	return inv
}

// listSandboxIDs returns the ids a node reports at path. ok is false when the
// listing did not happen; an empty list from a healthy node is ok=true.
// listSandboxIDs returns a node's sandboxes as id -> state.
func (s *Server) listSandboxIDs(node *cluster.Node, path string) (map[string]string, bool) {
	resp, err := s.forward(context.Background(), node, "GET", path, nil, "")
	if err != nil {
		return nil, false
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false
	}
	var page struct {
		Sandboxes []struct {
			ID     string `json:"id"`
			State  string `json:"state"`
			Status string `json:"status"`
		} `json:"sandboxes"`
	}
	if json.Unmarshal(b, &page) != nil {
		return nil, false
	}
	inv := make(map[string]string, len(page.Sandboxes))
	for _, sb := range page.Sandboxes {
		if sb.ID != "" {
			inv[sb.ID] = cmp.Or(sb.State, sb.Status)
		}
	}
	return inv, true
}

// StartPlacementReconciler rebuilds placements once immediately and then every
// interval until stop is closed.
func (s *Server) StartPlacementReconciler(interval time.Duration, stop <-chan struct{}) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		if n := s.RebuildPlacements(s.now()); n > 0 {
			s.log().Info("recovered sandbox placements from nodes",
				"component", "placement", "count", n)
		}
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				// Leader-only so N replicas do not all fan out to every node every tick.
				if !s.isReconcileLeader() {
					continue
				}
				s.RebuildPlacements(s.now())
				// Cleaning dead nodes' placements makes their sandboxes 404 instead of hang.
				if reaped := s.reg.ReapDeadNodes(s.now(), cluster.DeadNodeTTL); len(reaped) > 0 {
					s.log().Info("reaped dead nodes and cleaned their placements",
						"component", "placement", "nodes", reaped)
					s.markLost(reaped)
				}
				// ReserveSlot only reaps expired slots for orgs still creating;
				// this covers orgs that have gone quiet.
				if st := s.st.Load(); st != nil {
					if n, err := st.PurgeExpiredSlots(context.Background()); err != nil {
						s.log().Warn("could not purge expired admission slots",
							"component", "admission", "err", err)
					} else if n > 0 {
						s.log().Info("purged expired admission slots",
							"component", "admission", "count", n)
					}
				}
			}
		}
	}()
}

// isReconcileLeader is always true without a Redis lease.
func (s *Server) isReconcileLeader() bool {
	if s.leader == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return s.leader.IsLeader(ctx)
}

// markLost flips a reaped node's live sandbox rows to lost, so they stop
// counting against their org's concurrency and read as gone, not running.
func (s *Server) markLost(nodes []string) {
	st := s.st.Load()
	if st == nil {
		return
	}
	for _, id := range nodes {
		n, err := st.MarkNodeSandboxesLost(context.Background(), id)
		if err != nil {
			s.log().Warn("could not mark a reaped node's sandboxes lost",
				"component", "placement", "node", id, "err", err)
			continue
		}
		if n > 0 {
			s.log().Warn("sandboxes lost with their node",
				"component", "placement", "node", id, "count", n)
		}
	}
}
