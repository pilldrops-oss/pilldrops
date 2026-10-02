package cluster

import (
	"slices"
	"time"
)

// Capacity is a node's live resource snapshot, mirrored from its GET /v1/host.
type Capacity struct {
	Cores          int     `json:"cpu_cores"`
	CPUPercent     float64 `json:"cpu_percent"`
	Load1          float64 `json:"load1"`
	MemTotalMB     int     `json:"mem_total_mb"`
	MemAvailMB     int     `json:"mem_available_mb"`
	MemCommittedMB int     `json:"mem_committed_mb"`
	DiskFreeGB     float64 `json:"disk_free_gb"`
	Active         int     `json:"sandboxes_active"`
	VCPUCommitted  int     `json:"vcpu_committed"`
}

// Node is a registered sandboxd instance the orchestrator can place work on.
type Node struct {
	ID      string            `json:"id"`
	URL     string            `json:"url"`
	Key     string            `json:"-"`
	Zone    string            `json:"zone"`
	Labels  map[string]string `json:"labels,omitempty"`
	Version string            `json:"-"`
	// Runtime is "firecracker", "docker" or "process"; Isolation reports whether it has a real boundary.
	Runtime   string `json:"runtime,omitempty"`
	Isolation bool   `json:"isolation"`
	// Templates is the set the node holds locally. nil means it does not
	// enumerate and boots anything; [] means it enumerates and holds nothing.
	Templates []string `json:"templates,omitempty"`
	// CanFetchTemplates reports a shared artifact store, so the node can obtain
	// any published template it does not hold.
	CanFetchTemplates bool `json:"can_fetch_templates,omitempty"`
	// AutoPause reports that the node pauses idle sandboxes (its runtime can
	// snapshot and its idle timer is on).
	AutoPause bool `json:"auto_pause,omitempty"`
	// TagNetworks reports that this node's runtime can put two sandboxes that
	// share a network tag in reach of each other.
	TagNetworks bool `json:"tag_networks,omitempty"`
	// EgressDomains reports that this node runs the name layer that makes a
	// domain egress rule enforceable.
	EgressDomains bool `json:"egress_domains,omitempty"`
	// Checkpoint, NamedSnapshot and Standby are the capture-plane bits, carried
	// so the fleet capability answer can report them and placement can filter
	// on NamedSnapshot. On firecracker NamedSnapshot needs snapshots AND
	// per-sandbox namespaces, so it is not implied by the runtime name.
	Checkpoint    bool `json:"checkpoint,omitempty"`
	NamedSnapshot bool `json:"named_snapshot,omitempty"`
	Standby       bool `json:"standby,omitempty"`
	// MultiPort, L4Forward and NestedDocker are the node answer's remaining
	// verbs, carried for the same reason as the three above: the fleet answer
	// is the only one a tenant reaches, and a verb missing from it cannot be
	// checked before it is used. Placement does not filter on them.
	MultiPort    bool `json:"multiport,omitempty"`
	L4Forward    bool `json:"l4_forward,omitempty"`
	NestedDocker bool `json:"nested_docker,omitempty"`
	// CPUFeatures is the node's dispatch-relevant instruction set, so a memory
	// snapshot is placed where the code paths its guest already chose actually
	// exist. nil means the node did not say; see providesCPU.
	CPUFeatures []string `json:"cpu_features,omitempty"`
	Cap         Capacity `json:"capacity"`
	// TLSSPKI is hidden like Key: publishing which nodes lack a pin maps where to sit on the wire.
	TLSSPKI   string    `json:"-"`
	LastSeen  time.Time `json:"last_seen"`
	FirstSeen time.Time `json:"-"`
	Cordoned  bool      `json:"-"`
	Draining  bool      `json:"-"`
	// URLChanges flapping every heartbeat is the signature of two boxes sharing a NODE_ID.
	URLChanges      int       `json:"-"`
	LastURLChangeAt time.Time `json:"-"`
}

// Request is the placement a caller wants.
type Request struct {
	VCPU  int
	MemMB int
	Zone  string
	// Avoid are node ids placement must not choose: the node a move is
	// leaving, and nodes that already refused this create as busy.
	Avoid []string
	// Runtime, when non-empty, restricts placement to nodes running exactly that runtime.
	Runtime string
	// Template is the canonical id (aliases resolved by the caller); nodes
	// advertising a template list must include it.
	Template string
	// NoBuild reports that the artifact store has no ready build of Template,
	// so a node that would have to fetch it cannot boot it.
	NoBuild bool
	// TagNetworks restricts placement to nodes that can partition network tags.
	// Set it only when the create names tags: a node without the capability
	// would accept the sandbox and refuse the tags with a 501.
	TagNetworks bool
	// EgressDomains restricts placement to nodes that run the name layer. Set
	// it only when the create's rules name a domain: same argument as
	// TagNetworks, and the node would otherwise take the sandbox and refuse the
	// rules with a 501.
	EgressDomains bool
	// NamedSnapshot restricts placement to nodes that can boot a sandbox from a
	// published capture. Set it when the create names a snapshot: the same
	// argument as TagNetworks, and a sharper one, because on firecracker the
	// capability is snapshots AND netns and the runtime pin alone does not
	// promise either — the node would take the create and answer 501.
	NamedSnapshot bool
	// CPUFeatures restricts placement to nodes whose CPU provides all of these.
	// Set from the snapshot's record when a create names one.
	//
	// A memory image is a frozen process, and its processes chose their code
	// paths from the capturing host's CPUID before they were frozen. Restore it
	// somewhere poorer and Firecracker loads it happily and the guest then dies
	// on the first instruction the host does not implement -- measured on the
	// two-node fleet as SIGILL on an AVX2 VPBROADCASTB. The node refuses such a
	// restore; this is what makes the refusal rare rather than routine, and it
	// is a subset test, not equality: poorer-to-richer is fine.
	CPUFeatures []string
}

// providesCPU reports whether this node's CPU provides every feature in want.
//
// A node reporting nothing is let through: its build predates the record, and
// filtering on an absent field would refuse every snapshot create for the
// length of a rolling upgrade -- the same allowance the NamedSnapshot filter
// makes. The node's own restore check is the backstop that keeps that
// allowance from being a silent SIGILL.
func (n *Node) providesCPU(want []string) bool {
	if len(want) == 0 || len(n.CPUFeatures) == 0 {
		return true
	}
	has := make(map[string]bool, len(n.CPUFeatures))
	for _, f := range n.CPUFeatures {
		has[f] = true
	}
	for _, f := range want {
		if !has[f] {
			return false
		}
	}
	return true
}

// holdsTemplate reports whether the node can boot tmpl without a fetch. nil
// Templates means the node does not enumerate and boots anything; [] means it
// enumerates and holds nothing. Collapsing the two would put a freshly joined
// Firecracker node into the preferred tier.
func (n *Node) holdsTemplate(tmpl string) bool {
	if tmpl == "" {
		return true
	}
	if n.Templates == nil {
		return true
	}
	if len(n.Templates) == 0 {
		return false
	}
	for _, t := range n.Templates {
		if t == tmpl {
			return true
		}
	}
	return false
}

// canBoot reports whether the node can boot req's template under req's runtime
// pin at all, by holding it or by fetching a build the store has.
func (n *Node) canBoot(req Request) bool {
	if req.Runtime != "" && n.Runtime != req.Runtime {
		return false
	}
	return n.holdsTemplate(req.Template) || n.CanFetchTemplates && !req.NoBuild
}

// standardRequest is the reference footprint for status and headroom math.
var standardRequest = Request{VCPU: 1, MemMB: 1024}

const (
	// FitMarginMB is headroom for host processes, the pool and lazy guest growth.
	FitMarginMB = 512
	// overcommitCap bounds committed RAM to this multiple of physical.
	overcommitCap = 2.0
)

// Healthy reports whether a node has heartbeat within ttl.
func (n *Node) Healthy(now time.Time, ttl time.Duration) bool {
	return now.Sub(n.LastSeen) <= ttl
}

// Schedulable reports whether the node is healthy and neither cordoned nor draining.
func (n *Node) Schedulable(now time.Time, ttl time.Duration) bool {
	return n.Healthy(now, ttl) && !n.Cordoned && !n.Draining
}

// Status derives the operator-facing node status: unreachable, draining,
// cordoned, full or available.
func (n *Node) Status(now time.Time, ttl time.Duration) string {
	switch {
	case !n.Healthy(now, ttl):
		return "unreachable"
	case n.Draining:
		return "draining"
	case n.Cordoned:
		return "cordoned"
	case !n.fits(standardRequest):
		return "full"
	default:
		return "available"
	}
}

// fits reports whether req fits within the fit margin and over-commit ceiling.
func (n *Node) fits(req Request) bool {
	if n.Cap.MemAvailMB < req.MemMB+FitMarginMB {
		return false
	}
	if n.Cap.MemTotalMB > 0 {
		committed := float64(n.Cap.MemCommittedMB+req.MemMB) / float64(n.Cap.MemTotalMB)
		if committed > overcommitCap {
			return false
		}
	}
	return true
}

// Pick chooses the best-fit schedulable node for req: least free RAM that still
// fits, ties to lower load. Preference order is template held locally, then
// in-zone, then fit, because a fetch costs seconds on the create path while a
// worse fit costs megabytes. Returns nil if nothing fits.
func Pick(nodes []*Node, req Request, now time.Time, ttl time.Duration) *Node {
	var holdsInZone, holdsAnyZone, fetchInZone, fetchAnyZone *Node

	for _, n := range nodes {
		if !n.Schedulable(now, ttl) || !n.fits(req) || slices.Contains(req.Avoid, n.ID) {
			continue
		}
		if !n.canBoot(req) {
			continue
		}
		// A node without the capability would take the sandbox and then refuse
		// its tags with a 501, so it is not a placement at all.
		if req.TagNetworks && !n.TagNetworks {
			continue
		}
		if req.EgressDomains && !n.EgressDomains {
			continue
		}
		if req.NamedSnapshot && !n.NamedSnapshot {
			continue
		}
		// A node that would take the memory image and kill the guest with
		// SIGILL is not a placement either.
		if !n.providesCPU(req.CPUFeatures) {
			continue
		}

		if n.holdsTemplate(req.Template) {
			holdsAnyZone = betterFit(holdsAnyZone, n)
			if req.Zone != "" && n.Zone == req.Zone {
				holdsInZone = betterFit(holdsInZone, n)
			}
			continue
		}
		fetchAnyZone = betterFit(fetchAnyZone, n)
		if req.Zone != "" && n.Zone == req.Zone {
			fetchInZone = betterFit(fetchInZone, n)
		}
	}

	for _, pick := range []*Node{holdsInZone, holdsAnyZone, fetchInZone, fetchAnyZone} {
		if pick != nil {
			return pick
		}
	}
	return nil
}

// hotLoad is the load per core above which a node is passed over while a
// cooler one fits. Best-fit on memory alone stacked every sandbox of a burst
// on one node while the others idled (6 of 6 in the rig's D1c run).
// ponytail: load1 lags a burst by a heartbeat and a minute's average; score
// on committed vCPU per core too if spikes still stack.
const hotLoad = 0.8

func (n *Node) hot() bool { return n.Cap.Cores > 0 && n.Cap.Load1/float64(n.Cap.Cores) > hotLoad }

func betterFit(best, n *Node) *Node {
	if best != nil && best.hot() != n.hot() {
		if n.hot() {
			return best
		}
		return n
	}
	if best == nil ||
		n.Cap.MemAvailMB < best.Cap.MemAvailMB ||
		(n.Cap.MemAvailMB == best.Cap.MemAvailMB && n.Cap.Load1 < best.Cap.Load1) {
		return n
	}
	return best
}

// StandardFits returns how many more standard sandboxes fit on n; 0 when not schedulable.
func (n *Node) StandardFits(now time.Time, ttl time.Duration) int {
	if !n.Schedulable(now, ttl) {
		return 0
	}
	step := standardRequest.MemMB
	byAvail := (n.Cap.MemAvailMB - FitMarginMB) / step
	if byAvail < 0 {
		byAvail = 0
	}
	byCommit := byAvail
	if n.Cap.MemTotalMB > 0 {
		ceiling := int(overcommitCap * float64(n.Cap.MemTotalMB))
		byCommit = (ceiling - n.Cap.MemCommittedMB) / step
		if byCommit < 0 {
			byCommit = 0
		}
	}
	if byCommit < byAvail {
		return byCommit
	}
	return byAvail
}

// StandardCapacity returns the node's total capacity in standard sandboxes, ignoring commitment.
func (n *Node) StandardCapacity() int {
	step := standardRequest.MemMB
	byTotal := (n.Cap.MemTotalMB - FitMarginMB) / step
	if byTotal < 0 {
		byTotal = 0
	}
	ceiling := int(overcommitCap*float64(n.Cap.MemTotalMB)) / step
	if ceiling < byTotal {
		return ceiling
	}
	return byTotal
}
