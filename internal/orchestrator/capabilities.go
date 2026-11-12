package orchestrator

import (
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// fleetCapabilities is the GET /v1/capabilities answer for the whole fleet,
// registered explicitly so the catch-all proxy does not return one node's answer.
type fleetCapabilities struct {
	Scope string `json:"scope"` // "fleet"

	// IsolationLevels is every level some counted node provides, strongest first.
	IsolationLevels []rt.IsolationLevel `json:"isolation_levels"`
	// Runtimes is the sorted union of engine names behind those levels.
	Runtimes []string `json:"runtimes"`
	// Templates is the union of what counted nodes hold locally, bootable with no fetch.
	Templates []string `json:"templates"`
	// CanFetchTemplates is true when some counted node has a shared artifact store.
	CanFetchTemplates bool `json:"can_fetch_templates"`
	// AutoPause is true only when every counted node with a runtime pauses idle
	// sandboxes: a client cannot choose its node, so one node that never pauses
	// makes the promise false.
	AutoPause bool `json:"auto_pause"`

	// The next several are ANY over counted nodes, not ALL, and for the opposite
	// reason to AutoPause: these are verbs a caller asks for, and placement
	// filters a create that needs one onto a node that has it. True means the
	// fleet can do this; false means nothing can and the verb should be greyed
	// out. Each existed only on the node answer, which tenants do not reach.
	//
	// TagNetworks means two sandboxes naming the same tag can reach each other.
	TagNetworks bool `json:"tag_networks"`
	// EgressDomains means a sandbox's egress rules may name a domain, matched
	// by TLS SNI on 443 and the HTTP Host header on 80. False means only
	// addresses and CIDRs are enforceable and a rule set naming a domain is
	// refused — never accepted and left unevaluated.
	EgressDomains bool `json:"egress_domains"`
	// Checkpoint means a sandbox can hold named save points and roll back to one.
	Checkpoint bool `json:"checkpoint"`
	// NamedSnapshot means a capture can be published under an org-owned name and
	// new sandboxes stamped out of it. Narrower than Checkpoint on firecracker,
	// which also needs per-sandbox namespaces.
	NamedSnapshot bool `json:"named_snapshot"`
	// Standby means an idle sandbox can be frozen in RAM and thawed in
	// milliseconds, keeping its processes and open sockets.
	Standby bool `json:"standby"`
	// MultiPort means more than one guest port can be previewed at once.
	MultiPort bool `json:"multiport"`
	// L4Forward means a raw TCP port can be forwarded to the guest. The node
	// half only: whether this deployment has an ingress to publish it on is
	// CustomDomains, below.
	L4Forward bool `json:"l4_forward"`
	// NestedDocker means a sandbox can run its own dockerd, which needs a
	// kernel of its own. The `docker` template is refused where this is false.
	NestedDocker bool `json:"nested_docker"`

	// CustomDomains is true when a customer hostname can be attached, which
	// needs a PORTAL_SANDBOX_DOMAIN to CNAME at. A control-plane property, not
	// a node one: a bare-node deployment has nowhere to point a record.
	CustomDomains bool `json:"custom_domains"`
	// ShareLinks is true when a local app can be given a public link: a share
	// domain is set and a replica is declared as its ingress. A deployment
	// setting like CustomDomains, so it is not in Features.
	ShareLinks bool `json:"share_links"`

	// Features names the optional command groups this deployment serves, beyond
	// the core sandbox API every server has. Never null: absent means a server
	// from before this field, which a client cannot judge.
	Features []string `json:"features"`

	Nodes fleetNodeCounts `json:"nodes"`
}

// fleetNodeCounts says which population produced the answer. Counted and
// Schedulable differ when a node is cordoned or draining.
type fleetNodeCounts struct {
	// Total is every node in the registry, healthy or not.
	Total int `json:"total"`
	// Counted is the healthy population this answer was computed from.
	Counted int `json:"counted"`
	// Schedulable is how many of those would take a placement now.
	Schedulable int `json:"schedulable"`
}

// aggregateCapabilities computes the fleet answer from a node list. Everything
// here is carried by the heartbeat; nothing is guessed from the runtime name,
// which would be wrong for every opt-in flag (FC_MEM_SNAPSHOTS, FC_NETNS).
// AutoPause is the AND over counted nodes with a runtime because it happens to
// a sandbox unbidden; the capability verbs below are the OR, because placement
// routes a create that needs one onto a node that has it.
func aggregateCapabilities(nodes []*cluster.Node, now time.Time, ttl time.Duration) fleetCapabilities {
	out := fleetCapabilities{Scope: "fleet", Features: []string{"control-plane", "volume"}}
	withRuntime, allPause := 0, true
	out.Nodes.Total = len(nodes)

	levels := map[rt.IsolationLevel]bool{}
	runtimes := map[string]bool{}
	tmpl := map[string]bool{}

	for _, n := range nodes {
		// Healthy, not Schedulable: a cordoned KVM node still offers its
		// isolation level. Capability, not capacity.
		if !n.Healthy(now, ttl) {
			continue
		}
		out.Nodes.Counted++
		if n.Schedulable(now, ttl) {
			out.Nodes.Schedulable++
		}
		// An empty runtime would collapse into "shared" via LevelFor's unknown case.
		if n.Runtime == "" {
			continue
		}
		levels[rt.LevelFor(n.Runtime, n.Isolation)] = true
		runtimes[n.Runtime] = true
		if n.CanFetchTemplates {
			out.CanFetchTemplates = true
		}
		withRuntime++
		allPause = allPause && n.AutoPause
		out.TagNetworks = out.TagNetworks || n.TagNetworks
		out.EgressDomains = out.EgressDomains || n.EgressDomains
		out.Checkpoint = out.Checkpoint || n.Checkpoint
		out.NamedSnapshot = out.NamedSnapshot || n.NamedSnapshot
		out.Standby = out.Standby || n.Standby
		out.MultiPort = out.MultiPort || n.MultiPort
		out.L4Forward = out.L4Forward || n.L4Forward
		out.NestedDocker = out.NestedDocker || n.NestedDocker
		for _, t := range n.Templates {
			tmpl[t] = true
		}
	}

	out.AutoPause = withRuntime > 0 && allPause

	// Strongest first; alphabetical would read as a ranking and is not one.
	out.IsolationLevels = []rt.IsolationLevel{}
	for _, l := range rt.IsolationLevels() {
		if levels[l] {
			out.IsolationLevels = append(out.IsolationLevels, l)
		}
	}
	out.Runtimes = sortedKeys(runtimes)
	out.Templates = sortedKeys(tmpl)
	return out
}

// sortedKeys returns a set's members sorted, never nil, so JSON emits [] not null.
func sortedKeys(m map[string]bool) []string {
	return append(make([]string, 0, len(m)), slices.Sorted(maps.Keys(m))...)
}

// handleCapabilities answers GET /v1/capabilities for the fleet.
func (s *Server) handleCapabilities(w http.ResponseWriter, _ *http.Request) {
	caps := aggregateCapabilities(s.reg.Nodes(), s.now(), cluster.NodeTTL)
	caps.CustomDomains = strings.TrimSpace(s.sandboxDomain) != ""
	caps.ShareLinks = shareLinksOn()
	writeJSON(w, 200, caps)
}
