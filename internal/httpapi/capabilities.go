package httpapi

import (
	"net/http"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/entitlements"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
)

// runtimeDescriber is the daemon's view of its runtime. The HTTP layer must not
// import a runtime package to describe one, so keep this narrow.
type runtimeDescriber interface {
	RuntimeName() string
	// RuntimeCaps returns ok=false when no runtime is configured at all.
	RuntimeCaps() (rt.Caps, bool)
	// IdleTimeoutDefault is the node-wide standby timer; 0 or less means never.
	IdleTimeoutDefault() time.Duration
	// HibernateTimeoutDefault is the node-wide disk-pause timer; 0 or less means never.
	HibernateTimeoutDefault() time.Duration
}

var _ runtimeDescriber = (*daemon.Daemon)(nil)

// nodeCapabilities is this node's answer to GET /v1/capabilities.
type nodeCapabilities struct {
	// Scope is "node", as opposed to the fleet aggregate the control plane serves at the same path.
	Scope string `json:"scope"`

	Runtime string `json:"runtime"`
	// IsolationLevel is strong, shared or trusted, mapped by runtime.LevelFor.
	IsolationLevel rt.IsolationLevel `json:"isolation_level"`
	// Isolation is the raw Caps bit the heartbeat reports and the scheduler filters on.
	Isolation bool `json:"isolation"`

	// Snapshot means pause frees the sandbox's RAM; false means SIGSTOP with memory resident.
	Snapshot bool `json:"snapshot"`
	// MultiPort means the preview URL can reach any guest port, not only the declared app port.
	MultiPort bool `json:"multiport"`
	// L4Forward means a guest port can be published on a raw public TCP/UDP
	// port. Whether the deployment has an ingress to publish it on is a
	// control-plane property, reported by the fleet answer.
	L4Forward bool `json:"l4_forward"`
	// NestedDocker means a sandbox on this node can run a container daemon of
	// its own. It follows from owning a kernel and is not configurable; the
	// `docker` template is refused at create where this is false.
	NestedDocker bool `json:"nested_docker"`
	// Desktop means this node's engine can host an X server. It is not the
	// per-sandbox answer: a sandbox also needs an image holding the screen
	// software, which its own has_desktop reports.
	Desktop bool `json:"desktop"`
	// TagNetworks means two sandboxes that named the same network tag can reach
	// each other on this node. False means a create naming tags is refused here.
	TagNetworks bool `json:"tag_networks"`
	// Volumes means a create may attach standalone volumes (POST /v1/volumes).
	Volumes bool `json:"volumes"`
	// EgressDomains means a sandbox's egress rules may name a DOMAIN on this
	// node, matched by TLS SNI on 443 and the HTTP Host header on 80. False
	// means only addresses and CIDRs are enforceable here and a rule set naming
	// a domain is refused, never accepted and left unevaluated.
	EgressDomains bool `json:"egress_domains"`
	// Checkpoint means the sandbox can hold named save points and be rolled back
	// to one in place. What comes back differs by runtime; see docs/runtimes.md.
	Checkpoint bool `json:"checkpoint"`
	// NamedSnapshot means a capture can be published under an org-owned name and
	// new sandboxes stamped out of it. Narrower than Checkpoint on firecracker,
	// which also needs per-sandbox namespaces to give a child the baked address.
	NamedSnapshot bool `json:"named_snapshot"`
	// Standby means an idle sandbox can be frozen in RAM and thawed in
	// milliseconds, keeping its processes and open sockets.
	Standby bool `json:"standby"`
	// AutoPause is true when an idle sandbox is paused by the node: the runtime
	// can snapshot and the idle timer is on. Reported explicitly so a client can
	// tell false from absent.
	AutoPause bool `json:"auto_pause"`

	// WarmRestoreStorageMB is the one disk size (MB) the warm boot path serves; 0 means no warm path.
	// A create resolving to any other size cold boots.
	WarmRestoreStorageMB int `json:"warm_restore_storage_mb"`

	// Templates is what this node holds locally, same reporter as the heartbeat.
	// TemplatesEnumerated distinguishes "enumerates and holds nothing" (a cold node)
	// from "does not enumerate" (boots anything); both marshal to an empty array.
	Templates           []string `json:"templates"`
	TemplatesEnumerated bool     `json:"templates_enumerated"`

	// Features names the optional command groups this deployment serves, beyond
	// the core sandbox API every server has. Never null: absent means a server
	// from before this field, which a client cannot judge.
	Features []string `json:"features"`

	Limits nodeLimits `json:"limits"`
}

// nodeLimits are the ceilings a create is held to: the caller's plan and this node's admission limit.
type nodeLimits struct {
	MaxVCPU       int `json:"max_vcpu"`
	MaxMemMB      int `json:"max_mem_mb"`
	MaxConcurrent int `json:"max_concurrent"`
	SessionTTLHrs int `json:"session_ttl_hrs"`
	MaxStorageMB  int `json:"max_storage_mb"`
	// NodeMaxActive is this node's own ceiling (MAX_ACTIVE) across all orgs; the smaller of it and the plan applies.
	NodeMaxActive int `json:"node_max_active"`
}

// SetLocalTemplates injects the reporter for templates this node holds locally.
// Pass the same func the heartbeat uses; nil means this node does not enumerate.
func (s *Server) SetLocalTemplates(fn func() []string) { s.localTemplates = fn }

// handleCapabilities answers GET /v1/capabilities for this node.
// handleWhoami names the caller on a node with no control plane, which is what
// `pilldrops login --key` checks a key against.
func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	principal := "key"
	if operatorPrincipal(r) {
		principal = "operator"
	}
	writeJSON(w, http.StatusOK, map[string]string{"org": orgID(r), "principal": principal})
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	// Refuse rather than describe: a zero Caps would read as isolation_level "trusted".
	if s.d == nil {
		writeErrCode(w, http.StatusServiceUnavailable, "no_runtime",
			"this node has no runtime configured and cannot report capabilities")
		return
	}
	caps, ok := buildNodeCapabilities(s.d, s.localTemplates,
		s.orgPlan(r.Context(), orgID(r)), s.maxActive)
	if !ok {
		writeErrCode(w, http.StatusServiceUnavailable, "no_runtime",
			"this node has no runtime configured and cannot report capabilities")
		return
	}
	writeJSON(w, http.StatusOK, caps)
}

// AutoPause is the one rule for the node bit and the heartbeat: idle sandboxes
// reach the disk tier only where pause frees RAM and the hibernate timer is
// running. It reads the HIBERNATE timer, not the idle one: idle now means
// standby, which holds the RAM rather than freeing it.
func AutoPause(caps rt.Caps, hibernate time.Duration) bool {
	return caps.Snapshot && hibernate > 0
}

// buildNodeCapabilities builds the capabilities document; ok=false when the describer has no runtime.
func buildNodeCapabilities(rd runtimeDescriber, local func() []string, plan entitlements.Plan, maxActive int) (nodeCapabilities, bool) {
	caps, ok := rd.RuntimeCaps()
	if !ok {
		return nodeCapabilities{}, false
	}
	name := rd.RuntimeName()

	tmpl, enumerated := []string{}, false
	if local != nil {
		enumerated = true
		if ids := local(); ids != nil {
			tmpl = ids
		}
	}

	return nodeCapabilities{
		Scope:                "node",
		Runtime:              name,
		IsolationLevel:       rt.LevelFor(name, caps.Isolation),
		Isolation:            caps.Isolation,
		Snapshot:             caps.Snapshot,
		MultiPort:            caps.MultiPort,
		L4Forward:            caps.L4Forward,
		NestedDocker:         caps.NestedDocker,
		Desktop:              caps.Desktop,
		TagNetworks:          caps.TagNetworks,
		Volumes:              caps.Volumes,
		EgressDomains:        caps.EgressDomains,
		Checkpoint:           caps.Checkpoint,
		NamedSnapshot:        caps.NamedSnapshot,
		Standby:              caps.Standby,
		AutoPause:            AutoPause(caps, rd.HibernateTimeoutDefault()),
		WarmRestoreStorageMB: caps.WarmRestoreStorageMB,
		Templates:            tmpl,
		TemplatesEnumerated:  enumerated,
		Features:             nodeFeatures(caps),
		Limits: nodeLimits{
			MaxVCPU:       plan.MaxVCPU,
			MaxMemMB:      plan.MaxMemMB,
			MaxConcurrent: plan.MaxConcurrent,
			SessionTTLHrs: plan.SessionTTLHrs,
			MaxStorageMB:  plan.MaxStorageMB,
			NodeMaxActive: maxActive,
		},
	}, true
}

// nodeFeatures lists the optional groups this node serves: volumes need a
// runtime that can attach them, so a Docker node does not offer the commands.
func nodeFeatures(caps rt.Caps) []string {
	if caps.Volumes {
		return []string{"volume", "webhook"}
	}
	return []string{"webhook"}
}
