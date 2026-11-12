package orchestrator

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	"github.com/pilldrops-oss/pilldrops/internal/nodetls"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

type heartbeatBody struct {
	ID        string            `json:"id"`
	URL       string            `json:"url"`
	Key       string            `json:"key"`
	Zone      string            `json:"zone"`
	Labels    map[string]string `json:"labels"`
	Version   string            `json:"version"`
	Runtime   string            `json:"runtime"`
	Isolation bool              `json:"isolation"`
	Templates []string          `json:"templates"` // template ids held locally; empty = unrestricted, unless CanFetch
	// CanFetch means the node can fetch a template it does not hold. Older nodes omit it.
	CanFetch bool `json:"can_fetch_templates"`
	// AutoPause means the node pauses idle sandboxes. Older nodes omit it.
	AutoPause bool `json:"auto_pause"`
	// TagNetworks means the node's runtime can partition sandboxes into opt-in
	// network tags. Older nodes omit it, which reads as false: a create naming
	// tags is not placed there.
	TagNetworks bool `json:"tag_networks"`
	// EgressDomains means the node runs the name layer that makes a domain
	// egress rule enforceable. Older nodes omit it, which reads as false: a
	// create whose rules name a domain is not placed there.
	EgressDomains bool `json:"egress_domains"`
	// Volumes means the node can create and attach standalone volumes. Older
	// nodes omit it: no volume is placed there.
	Volumes bool `json:"volumes"`
	// Checkpoint, NamedSnapshot and Standby are the capture-plane bits. Older
	// nodes omit them, which reads as false: a create from a snapshot is not
	// placed there and the fleet answer does not claim the verb.
	Checkpoint    bool `json:"checkpoint"`
	NamedSnapshot bool `json:"named_snapshot"`
	Standby       bool `json:"standby"`
	// MultiPort, L4Forward and NestedDocker are reported for the fleet
	// capability answer. Older nodes omit them, which reads as false.
	MultiPort    bool `json:"multiport"`
	L4Forward    bool `json:"l4_forward"`
	NestedDocker bool `json:"nested_docker"`
	// CPUFeatures is the node's dispatch-relevant instruction set. Absent from
	// a pre-upgrade node, which reads as "unknown" and is not filtered on.
	CPUFeatures []string         `json:"cpu_features"`
	Cap         cluster.Capacity `json:"capacity"`
	// TLSSPKI is base64(SHA-256(SPKI)) of the certificate this node serves. Older nodes omit it.
	TLSSPKI string `json:"tls_spki,omitempty"`
	// TS is the node's own clock when it composed this beat, Unix ms. Older nodes omit it.
	TS int64 `json:"ts,omitempty"`
}

// heartbeatAuthorized accepts only the token derived for this id+zone from
// NODE_KEY_SECRET, so a compromised node cannot heartbeat as another.
// Callers must check nodeKeySecret is non-empty first.
func (s *Server) heartbeatAuthorized(r *http.Request, id, zone string) bool {
	got := bearer(r)
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.nodeHeartbeatToken(id, zone))) == 1
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if !s.hbIPLimit.allow(httpx.ClientIP(r)) {
		w.Header().Set("Retry-After", "1")
		writeErr(w, 429, "heartbeat rate limit exceeded")
		return
	}
	// Decoded before authentication because the per-node credential depends on
	// the id in the body; the rate limit above bounds the cost.
	var b heartbeatBody
	if json.NewDecoder(r.Body).Decode(&b) != nil || b.ID == "" || b.URL == "" {
		writeErr(w, 400, "id and url required")
		return
	}
	// Checked before the credential branches below so a removed id is
	// refused whichever path would otherwise accept it, including the
	// NODE_KEY_SECRET-unset dev-insecure fallback that skips heartbeatAuthorized entirely.
	if s.nodeRemoved(r.Context(), b.ID) {
		writeErr(w, 403, "node removed: this id was revoked by an operator and will not be re-registered")
		return
	}
	if s.nodeKeySecret == "" {
		// Fail closed: without NODE_KEY_SECRET every node would share one credential.
		if !devInsecure() {
			writeErr(w, 503, "orchestrator not configured: NODE_KEY_SECRET is not set "+
				"(refusing heartbeats — without it node credentials are derived from ORCHESTRATOR_TOKEN, "+
				"which every node holds, so any node could heartbeat as any other. "+
				"Set NODE_KEY_SECRET=$(openssl rand -hex 32) and re-join nodes, or DEV_INSECURE=1 for local dev)")
			return
		}
	} else if !s.heartbeatAuthorized(r, b.ID, b.Zone) {
		writeErr(w, 401, "invalid node token")
		return
	}
	if !s.hbNodeLimit.allow(b.ID) {
		w.Header().Set("Retry-After", "1")
		writeErr(w, 429, "heartbeat rate limit exceeded for this node")
		return
	}
	// Derive the key server-side; a body-supplied key must not set the node's identity.
	key := s.nodeKeyFor(b.ID, b.Zone)
	// Refuse at registration rather than at first forward, so the scheduler
	// never places a tenant on a node that would then fail the create.
	if b.TLSSPKI == "" && nodetls.RequireSecureHop() {
		s.logger.WarnContext(r.Context(), "heartbeat refused: node advertises no TLS pin",
			"component", "orchestrator", "node_id", b.ID, "client_ip", httpx.ClientIP(r))
		writeErr(w, 426, nodetls.RequireSecureHopEnv+"=1 is set on this control plane and this node "+
			"advertises no TLS pin (tls_spki). Upgrade the node to a build that serves TLS on its API "+
			"port, or unset "+nodetls.RequireSecureHopEnv+" until the fleet has rolled")
		return
	}
	if err := s.reg.Upsert(cluster.NodeUpsert{
		ID: b.ID, URL: b.URL, Key: key, Zone: b.Zone, Labels: b.Labels,
		Version: b.Version, Cap: b.Cap, TLSSPKI: b.TLSSPKI, BeatMS: b.TS,
	}, s.now()); err != nil {
		if errors.Is(err, cluster.ErrStaleHeartbeat) {
			// Warn because a node's clock does not go backwards: this is a replay or a broken clock.
			s.logger.WarnContext(r.Context(), "heartbeat refused: stale or replayed",
				"component", "orchestrator", "node_id", b.ID, "beat_ms", b.TS,
				"client_ip", httpx.ClientIP(r), "err", err)
			writeErr(w, 409, "stale heartbeat refused: "+err.Error())
			return
		}
		if errors.Is(err, cluster.ErrNodeTLSPinTakeover) {
			s.logger.WarnContext(r.Context(), "heartbeat refused: tls pin takeover",
				"component", "orchestrator", "node_id", b.ID, "zone", b.Zone,
				"claimed_spki", b.TLSSPKI, "client_ip", httpx.ClientIP(r))
			writeErr(w, 409, "node tls pin takeover refused: this node id is already registered with a "+
				"different, still-live TLS pin. If this node genuinely re-keyed, it will be accepted once "+
				"the previous registration ages out (NodeTTL)")
			return
		}
		if errors.Is(err, cluster.ErrNodeURLTakeover) {
			// Either the node moved (retry succeeds once the old address ages
			// out) or its token is in someone else's hands.
			s.logger.WarnContext(r.Context(), "heartbeat refused: url takeover",
				"component", "orchestrator", "node_id", b.ID, "zone", b.Zone,
				"claimed_url", b.URL, "client_ip", httpx.ClientIP(r))
			writeErr(w, 409, "node url takeover refused: this node id is already registered at a different, "+
				"still-live url. If this node genuinely moved, it will be accepted once the previous "+
				"registration ages out (NodeTTL)")
			return
		}
		writeErr(w, 503, "node registry unavailable")
		return
	}
	s.reg.SetNodeMeta(b.ID, cluster.NodeMeta{
		Runtime: b.Runtime, Isolation: b.Isolation, Templates: b.Templates,
		CanFetch: b.CanFetch, AutoPause: b.AutoPause, TagNetworks: b.TagNetworks,
		EgressDomains: b.EgressDomains, Volumes: b.Volumes,
		Checkpoint: b.Checkpoint, NamedSnapshot: b.NamedSnapshot, Standby: b.Standby,
		MultiPort: b.MultiPort, L4Forward: b.L4Forward, NestedDocker: b.NestedDocker,
		CPUFeatures: b.CPUFeatures,
	})
	writeJSON(w, 200, map[string]any{"ok": true, "ttl_seconds": int(cluster.NodeTTL.Seconds())})
}

func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	nodes := s.reg.Nodes()
	out := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, nodeView(n, now))
	}
	writeJSON(w, 200, map[string]any{"nodes": out})
}

// nodeView renders a node for the fleet view. url_conflict flags an active
// NODE_ID collision (3+ URL changes within the TTL) and self-clears once the
// flapping stops.
func nodeView(n *cluster.Node, now time.Time) map[string]any {
	conflict := n.URLChanges >= 3 && now.Sub(n.LastURLChangeAt) < cluster.NodeTTL
	return map[string]any{
		"id": n.ID, "url": n.URL, "zone": n.Zone, "labels": n.Labels,
		"healthy": n.Healthy(now, cluster.NodeTTL), "status": n.Status(now, cluster.NodeTTL),
		"runtime": n.Runtime, "isolation": n.Isolation, "version": n.Version,
		"first_seen": n.FirstSeen.Unix(), "last_seen": n.LastSeen.Unix(),
		"capacity":     n.Cap,
		"url_changes":  n.URLChanges,
		"url_conflict": conflict,
		// Placement inputs: prefers a node that holds the template, falls back to one that can fetch it.
		"templates":           n.Templates,
		"can_fetch_templates": n.CanFetchTemplates,
		"auto_pause":          n.AutoPause,
		// A boolean rather than the pin itself: the operator needs to know the
		// rollout is complete, and the pin would be a fingerprint for scanners.
		"tls_pinned": n.TLSSPKI != "",
	}
}

// wantRuntime resolves the runtime a create is pinned to from an explicit
// runtime or an isolation alias; "" means any runtime.
func wantRuntime(runtime, isolation string) string {
	switch strings.ToLower(strings.TrimSpace(runtime)) {
	case "firecracker", "docker", "process":
		return strings.ToLower(strings.TrimSpace(runtime))
	}
	switch strings.ToLower(strings.TrimSpace(isolation)) {
	case "vm", "microvm", "firecracker", "hardware":
		return "firecracker"
	case "container", "docker":
		return "docker"
	}
	return ""
}

// templateRuntime returns the node runtime a template requires, or "" when any will do.
func templateRuntime(name string) string {
	if t, ok := templates.Resolve(name); ok {
		// Image-backed service templates only run under docker. Keyed on the
		// template, not req.Image, because the catalogue supplies the image.
		if t.Image != "" {
			return "docker"
		}
		// The browser template's start.sh only runs under the firecracker guest init.
		if t.ID == "browser" {
			return "firecracker"
		}
		// A container daemon inside the sandbox needs a kernel of its own; a
		// shared-kernel node refuses it at create (Caps.NestedDocker).
		if t.ID == "docker" {
			return "firecracker"
		}
	}
	return ""
}
