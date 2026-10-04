// Package cluster is the fleet's coordination core: the join protocol, the node
// table, the sandbox-to-node route catalog, the best-fit scheduler and the
// reconciler's leader lease. Stores have in-memory and Redis implementations.
package cluster

import (
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"
)

// NodeTTL is how long after its last heartbeat a node stays schedulable.
const NodeTTL = 20 * time.Second

// ReservationTTL must exceed the heartbeat interval so the node's reported
// capacity reflects the new sandbox before the hold expires.
const ReservationTTL = 15 * time.Second

// DeadNodeTTL is how long after its last heartbeat a node is reaped. Well
// beyond NodeTTL so a blip drops a node from scheduling without destroying its placements.
const DeadNodeTTL = 2 * time.Minute

// Registry tracks the fleet's nodes and each sandbox's placement. It holds no
// state of its own, so any replica answers for any node. Safe for concurrent use.
type Registry struct {
	nodes  NodeStore
	routes RouteStore

	// listTTL > 0 caches the node list this long. A shared store is read on
	// every create otherwise: two HGETALLs and a decode per node, which at a
	// thousand nodes is a megabyte per create. Heartbeats refresh the data every
	// 5 s, so a one-second copy is no staler than the data already is; this
	// replica's own writes drop it at once.
	listTTL  time.Duration
	cacheMu  sync.Mutex
	cached   []*Node
	cachedAt time.Time

	// routeFallback names a sandbox's node when the route catalog does not:
	// a wiped or restarted Redis loses every route, and until the nodes'
	// inventories rebuild them each sandbox would read as not found.
	routeFallback func(sandboxID string) (nodeID string, ok bool)
}

// SetRouteFallback installs where NodeFor looks after a route miss; the
// control plane points it at the sandbox rows, which record the owning node.
func (r *Registry) SetRouteFallback(f func(sandboxID string) (string, bool)) { r.routeFallback = f }

// NewRegistry builds a registry backed by in-memory stores.
func NewRegistry() *Registry {
	return &Registry{nodes: newMemNodeStore(), routes: newMemRouteStore()}
}

// NewRegistryWithRoutes builds a registry with the given route catalog and an in-memory node table.
func NewRegistryWithRoutes(routes RouteStore) *Registry {
	return &Registry{nodes: newMemNodeStore(), routes: routes}
}

// NewRegistryShared builds a registry with both catalogs shared.
func NewRegistryShared(nodes NodeStore, routes RouteStore) *Registry {
	return &Registry{nodes: nodes, routes: routes, listTTL: sharedListTTL}
}

// sharedListTTL bounds how stale another replica's view of a node write can be.
const sharedListTTL = time.Second

// logStoreErr reports a store failure. Failed reads degrade to "no nodes",
// which fails the scheduler closed with a 503.
func logStoreErr(op string, err error) {
	if err != nil {
		slog.Error("node store operation failed", "component", "registry", "op", op, "err", err)
	}
}

// list returns every known node, id-sorted, or nil when the store is unreadable.
// Callers get their own copies: PickAndReserve adjusts capacity in place.
func (r *Registry) list() []*Node {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	if r.cached == nil || time.Since(r.cachedAt) >= r.listTTL {
		ns, err := r.nodes.List()
		if err != nil {
			logStoreErr("list", err)
			return nil
		}
		r.cached, r.cachedAt = sortNodes(ns), time.Now()
	}
	out := make([]*Node, len(r.cached))
	for i, n := range r.cached {
		c := *n
		out[i] = &c
	}
	return out
}

// invalidate drops the cached list after this replica changed a node.
func (r *Registry) invalidate() {
	r.cacheMu.Lock()
	r.cached = nil
	r.cacheMu.Unlock()
}

// PickAndReserve chooses the best-fit schedulable node, counting in-flight
// reservations, and reserves req's memory on it. Returns nil if nothing fits;
// on create failure the caller must Release. Read-then-write, not a
// transaction: two replicas can both pass within one Redis round trip, and the
// node's own maxActive backstops that. A Lua script would close it.
func (r *Registry) PickAndReserve(req Request, now time.Time, ttl time.Duration) *Node {
	nodes := r.list()
	if len(nodes) == 0 {
		return nil
	}
	reserved, err := r.nodes.ReservedMB(now)
	if err != nil {
		// Scheduling blind to outstanding holds would over-place the fleet.
		logStoreErr("reserved", err)
		return nil
	}
	for _, n := range nodes {
		if res := reserved[n.ID]; res > 0 {
			n.Cap.MemAvailMB -= res
			n.Cap.MemCommittedMB += res
		}
	}
	node := Pick(nodes, req, now, ttl)
	if node == nil {
		return nil
	}
	if err := r.nodes.Reserve(node.ID, req.MemMB, now.Add(ReservationTTL)); err != nil {
		logStoreErr("reserve", err)
		return nil
	}
	return node
}

// Release drops one reservation of memMB from a node. No-op if none match.
func (r *Registry) Release(nodeID string, memMB int) {
	logStoreErr("unreserve", r.nodes.Unreserve(nodeID, memMB))
}

// Upsert registers a node or refreshes its heartbeat. The error is returned,
// not just logged, so ErrNodeURLTakeover reaches the HTTP handler.
func (r *Registry) Upsert(u NodeUpsert, now time.Time) error {
	defer r.invalidate()
	err := r.nodes.Upsert(u, now)
	logStoreErr("upsert", err)
	return err
}

// SetNodeMeta records what a heartbeat says about the node's runtime: name,
// isolation, bootable templates (nil means unrestricted), template fetching,
// idle auto-pause, tag networks and the capture-plane bits. No-op for an
// unknown id.
func (r *Registry) SetNodeMeta(id string, m NodeMeta) {
	defer r.invalidate()
	logStoreErr("setmeta", r.nodes.SetMeta(id, m))
}

// TemplateSupport reports whether any schedulable node can boot req's template
// under its runtime pin, by Pick's own rule, plus the sorted union of templates
// the restricted nodes advertise.
func (r *Registry) TemplateSupport(req Request, now time.Time, ttl time.Duration) (bool, []string) {
	supported := false
	seen := map[string]bool{}
	for _, n := range r.list() {
		if !n.Schedulable(now, ttl) {
			continue
		}
		if n.canBoot(req) {
			supported = true
		}
		for _, t := range n.Templates {
			seen[t] = true
		}
	}
	union := make([]string, 0, len(seen))
	for t := range seen {
		union = append(union, t)
	}
	sort.Strings(union)
	return supported, union
}

// AnyByRuntime reports whether any schedulable node runs the given runtime.
func (r *Registry) AnyByRuntime(runtime string, now time.Time, ttl time.Duration) bool {
	for _, n := range r.list() {
		if n.Schedulable(now, ttl) && n.Runtime == runtime {
			return true
		}
	}
	return false
}

// AnyProvidingCPU reports whether any schedulable node's CPU provides req's
// feature set. It answers the question that separates "add a newer node" from
// "add any node": a snapshot no online CPU can run is a permanent refusal,
// not a retryable capacity shortage.
func (r *Registry) AnyProvidingCPU(req Request, now time.Time, ttl time.Duration) bool {
	for _, n := range r.list() {
		if n.Schedulable(now, ttl) && !slices.Contains(req.Avoid, n.ID) && n.providesCPU(req.CPUFeatures) {
			return true
		}
	}
	return false
}

// LargestMemMB is the memory of the biggest schedulable node, 0 when none is
// online or none reports its size. It separates "wait for room" from "no
// machine here is that big": the second never comes true by waiting.
//
// Total, not available: free memory comes back, a machine's size does not.
func (r *Registry) LargestMemMB(now time.Time, ttl time.Duration) int {
	most := 0
	for _, n := range r.list() {
		if n.Schedulable(now, ttl) && n.Cap.MemTotalMB > most {
			most = n.Cap.MemTotalMB
		}
	}
	return most
}

// Capability names AnyWithCapability accepts. A small closed set rather than a
// predicate argument: the caller asks "can anything do this", and the answer
// decides a 400 from a 503.
type Capability string

const (
	CapTagNetworks   Capability = "tag_networks"
	CapEgressDomains Capability = "egress_domains"
	CapNamedSnapshot Capability = "named_snapshot"
)

// AnyWithCapability reports whether any schedulable node has the capability.
// Distinguishes "nothing can ever do this" from "nothing has room right now".
func (r *Registry) AnyWithCapability(c Capability, now time.Time, ttl time.Duration) bool {
	for _, n := range r.list() {
		if !n.Schedulable(now, ttl) {
			continue
		}
		switch c {
		case CapTagNetworks:
			if n.TagNetworks {
				return true
			}
		case CapEgressDomains:
			if n.EgressDomains {
				return true
			}
		case CapNamedSnapshot:
			if n.NamedSnapshot {
				return true
			}
		}
	}
	return false
}

// SetCordon flips a node's cordoned flag. Returns false if the node is unknown.
func (r *Registry) SetCordon(id string, on bool) bool {
	defer r.invalidate()
	found, err := r.nodes.SetFlag(id, FlagCordoned, on)
	logStoreErr("cordon", err)
	return found
}

// SetDrain flips a node's draining flag. Returns false if the node is unknown.
func (r *Registry) SetDrain(id string, on bool) bool {
	defer r.invalidate()
	found, err := r.nodes.SetFlag(id, FlagDraining, on)
	logStoreErr("drain", err)
	return found
}

// Remove forgets a node along with its reservations and placements. Returns
// false if the node was already unknown.
func (r *Registry) Remove(id string) bool {
	defer r.invalidate()
	found, err := r.nodes.Delete(id)
	logStoreErr("delete", err)
	if !found {
		return false
	}
	_, _ = r.routes.UnplaceNode(id)
	return true
}

// PlacementsOn returns the sandbox ids currently placed on a node, sorted.
func (r *Registry) PlacementsOn(nodeID string) []string {
	out, _ := r.routes.PlacementsOn(nodeID)
	sort.Strings(out)
	return out
}

// ReapDeadNodes removes nodes not seen within deadAfter along with their
// placements and reservations, and returns the reaped ids. Run leader-only.
func (r *Registry) ReapDeadNodes(now time.Time, deadAfter time.Duration) []string {
	defer r.invalidate()
	var reaped []string
	for _, n := range r.list() {
		if now.Sub(n.LastSeen) > deadAfter {
			if found, err := r.nodes.Delete(n.ID); err != nil {
				logStoreErr("reap", err)
			} else if found {
				reaped = append(reaped, n.ID)
			}
		}
	}
	for _, id := range reaped {
		_, _ = r.routes.UnplaceNode(id)
	}
	sort.Strings(reaped)
	return reaped
}

// Nodes returns all known nodes, healthy or not, id-sorted.
func (r *Registry) Nodes() []*Node { return r.list() }

// Healthy returns the nodes that heartbeat within NodeTTL, id-sorted because
// callers treat position 0 as the primary.
func (r *Registry) Healthy(now time.Time) []*Node {
	var out []*Node
	for _, n := range r.list() {
		if n.Healthy(now, NodeTTL) {
			out = append(out, n)
		}
	}
	return out
}

// Node returns a copy of one node by id.
func (r *Registry) Node(id string) (*Node, bool) {
	n, ok, err := r.nodes.Get(id)
	if err != nil {
		logStoreErr("get", err)
		return nil, false
	}
	return n, ok
}

// Place records that a sandbox now lives on a node.
func (r *Registry) Place(sandboxID, nodeID string) {
	_ = r.routes.Place(sandboxID, nodeID)
}

// Unplace forgets a sandbox's placement.
func (r *Registry) Unplace(sandboxID string) {
	_ = r.routes.Unplace(sandboxID)
}

// NodeFor returns the node owning a sandbox; ok is false if the sandbox is
// unknown or its node is gone.
func (r *Registry) NodeFor(sandboxID string) (*Node, bool) {
	nodeID, ok, err := r.routes.NodeFor(sandboxID)
	if err != nil || !ok {
		if r.routeFallback == nil {
			return nil, false
		}
		if nodeID, ok = r.routeFallback(sandboxID); !ok {
			return nil, false
		}
		n, ok := r.Node(nodeID)
		if ok {
			_ = r.routes.Place(sandboxID, nodeID) // best-effort: the next miss just asks again
		}
		return n, ok
	}
	return r.Node(nodeID)
}
