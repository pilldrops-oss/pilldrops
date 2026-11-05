package nodeboot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/hoststats"
	"github.com/pilldrops-oss/pilldrops/internal/httpapi"
	"github.com/pilldrops-oss/pilldrops/internal/nodetls"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
)

// localhostURL reports whether u addresses this machine. Plaintext to
// localhost is exempt from the secure-hop check so local dev and tests need no TLS.
func localhostURL(u string) bool {
	rest := u
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	host, _, err := net.SplitHostPort(rest)
	if err != nil {
		host = rest
	}
	if i := strings.IndexAny(host, "/"); i >= 0 {
		host = host[:i]
	}
	switch host {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	return false
}

// Version is the build version reported in every heartbeat, stamped via
// -ldflags -X; "dev" means an unstamped local build.
var Version = "dev"

// advertisedTemplates returns the heartbeat "templates" value and whether to
// send it. The scheduler reads absent as "boots anything" and [] as "holds
// nothing", and nil marshals to null (absent), so an empty scan is forced to [].
func advertisedTemplates(bootable func() []string) ([]string, bool) {
	if bootable == nil {
		return nil, false
	}
	ids := bootable()
	if ids == nil {
		ids = []string{}
	}
	return ids, true
}

// LocalTemplates returns the reporter for the template ids this node holds on
// disk, or nil for runtimes that boot anything. It is the disk scan only, never
// unioned with TEMPLATE=, which would advertise images nobody has built.
func LocalTemplates(runtimeName string, published func() []string) func() []string {
	if runtimeName != "firecracker" {
		return nil
	}
	return published
}

// NodeClass is the "class" label from NODE_LABELS, for stamping onto metering
// rows. Read from the same labels the heartbeat advertises so the meter and
// the scheduler cannot disagree. Empty means unclassified.
func NodeClass() string { return parseLabels(os.Getenv("NODE_LABELS"))["class"] }

// parseLabels parses "k=v,k=v", returning nil when nothing parses so the field
// is omitted from JSON.
func parseLabels(s string) map[string]string {
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			continue
		}
		out[k] = strings.TrimSpace(v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// heartbeatFailure logs a failing heartbeat when the reason changes, not on every
// beat. An empty reason means the beat succeeded.
type heartbeatFailure struct{ last string }

func (f *heartbeatFailure) note(reason, nodeID string) {
	if reason != "" && reason != f.last {
		slog.Error("heartbeat failing: the orchestrator will not schedule this node",
			"component", "node", "node_id", nodeID, "reason", reason)
	}
	f.last = reason
}

// StartHeartbeat posts this node's capacity to ORCHESTRATOR_URL every 5s; no-op
// when unset. bootable is re-evaluated per beat so a newly published image is
// advertised without a restart.
func StartHeartbeat(ctx context.Context, d *daemon.Daemon, dataDir, selfURL, runtimeName string, isolation bool, bootable func() []string, canFetchTemplates bool) {
	orch := os.Getenv("ORCHESTRATOR_URL")
	if orch == "" {
		return
	}
	// The heartbeat carries the bearer token and the TLS pin that secures every
	// later orchestrator-to-node call, so a plaintext hop undoes all of it.
	if !strings.HasPrefix(orch, "https://") && !localhostURL(orch) {
		if nodetls.RequireSecureHop() {
			slog.Error("refusing to heartbeat over a plaintext ORCHESTRATOR_URL: it carries this "+
				"node's bearer token and the TLS pin securing every call back to it. "+
				"Put the control plane behind HTTPS, or unset "+nodetls.RequireSecureHopEnv,
				"component", "node", "orchestrator", orch)
			return
		}
		slog.Warn("ORCHESTRATOR_URL is plaintext: this node's bearer token and TLS pin cross the "+
			"network in the clear. Set "+nodetls.RequireSecureHopEnv+"=1 once the control plane is on HTTPS",
			"component", "node", "orchestrator", orch)
	}
	token := os.Getenv("ORCHESTRATOR_TOKEN")
	// Empty when no identity could be established; an enforcing control plane refuses that.
	spki := NodeTLSSPKI(dataDir)
	nodeID := NodeID()
	nodeURL := os.Getenv("NODE_URL")
	if nodeURL == "" {
		nodeURL = selfURL
	}
	zone := os.Getenv("NODE_ZONE")
	if zone == "" {
		zone = "default"
	}
	labels := parseLabels(os.Getenv("NODE_LABELS"))

	client := &http.Client{Timeout: 8 * time.Second}
	// The replay guard wants every beat newer than the last one accepted, so
	// the node never sends an older one: after its clock stepped back, the
	// control plane refused its beats (and marked it down) until the clock
	// caught up. Only an accepted beat counts, so a refused one from a clock
	// that ran too far ahead is not carried forward once the clock is fixed.
	var accepted int64 // send runs on one goroutine
	var failure heartbeatFailure
	send := func() {
		ctx, cancel := context.WithTimeout(ctx, heartbeatTimeout)
		defer cancel()
		st := hoststats.Sample(dataDir)
		caps, _ := d.RuntimeCaps()
		autoPause := httpapi.AutoPause(caps, d.HibernateTimeoutDefault())
		boxes := d.List("")
		vcpu, memMB := 0, 0
		for _, b := range boxes {
			vcpu += b.VCPU
			memMB += b.MemMiB
		}
		payload := map[string]any{
			// No "key": the orchestrator derives it from id, zone and the shared token.
			"id": nodeID, "url": nodeURL,
			"zone": zone, "labels": labels, "version": Version,
			"runtime": runtimeName, "isolation": isolation,
			"auto_pause": autoPause,
			// Placement reads this: a create naming network tags may only land
			// on a node whose runtime can actually partition them.
			"tag_networks": caps.TagNetworks,
			// Same, for a create whose egress rules name a domain: only a node
			// running the name layer can enforce one.
			"egress_domains": caps.EgressDomains,
			// Placement and the fleet capability answer read these: a create
			// naming a snapshot may only land where one can actually be booted,
			// and a tenant asking what the fleet can do never sees this node's
			// own /v1/capabilities.
			"checkpoint": caps.Checkpoint, "named_snapshot": caps.NamedSnapshot,
			"standby": caps.Standby,
			// Not placement predicates, unlike everything above: these travel
			// only so the fleet's /v1/capabilities can answer for them. They
			// are exactly the verbs a caller checks before using — publishing a
			// second port, forwarding raw TCP, running dockerd inside the guest
			// — and the fleet answer is the only one a tenant ever reaches.
			"multiport": caps.MultiPort, "l4_forward": caps.L4Forward,
			"nested_docker": caps.NestedDocker,
			"capacity": map[string]any{
				"cpu_cores": st.Cores, "cpu_percent": st.CPUPercent, "load1": st.LoadAvg[0],
				"mem_total_mb": st.MemTotalMB, "mem_available_mb": st.MemAvailMB,
				"mem_committed_mb": memMB, "disk_free_gb": st.DiskFreeGB,
				"sandboxes_active": len(boxes), "vcpu_committed": vcpu,
			},
		}
		if ids, answering := advertisedTemplates(bootable); answering {
			payload["templates"] = ids
		}
		// Both fields are omitted rather than sent empty so the payload matches
		// what a pre-upgrade node sends.
		if spki != "" {
			payload["tls_spki"] = spki
		}
		if canFetchTemplates {
			payload["can_fetch_templates"] = true
		}
		// Placement reads this: a memory snapshot restores only on a CPU that
		// provides at least what the capturing one did. Omitted rather than sent
		// empty, so "this node predates the record" and "this host reports no
		// features" stay the same absent field and neither is read as a CPU
		// with nothing on it.
		if feats := hoststats.CPUFeatures(); len(feats) > 0 {
			payload["cpu_features"] = feats
		}
		// Replay guard. Milliseconds, since a restart beats immediately after
		// the previous tick and seconds could not tell that from a replay.
		ts := max(time.Now().UnixMilli(), accepted+1)
		payload["ts"] = ts
		body, _ := json.Marshal(payload)
		req, err := http.NewRequestWithContext(ctx, "POST", orch+"/internal/heartbeat", bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err != nil {
			failure.note("orchestrator unreachable", nodeID)
			return // try again next tick
		}
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			failure.note(fmt.Sprintf("status %d", resp.StatusCode), nodeID)
			return
		}
		accepted = ts
		failure.note("", nodeID)
	}

	slog.Info("clustering on; heartbeating to the orchestrator",
		"component", "node", "orchestrator", orch, "node_id", nodeID)
	go func() {
		send() // register immediately
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(heartbeatInterval()):
				send()
			}
		}
	}()
}

// heartbeatInterval is 5 s, jittered by up to a second either way. On a fixed
// tick, nodes that start together (a fleet-wide restart or upgrade) beat
// together forever: measured with 500 simulated nodes, every cycle arrived as
// one burst and a fifth of the beats were refused.
func heartbeatInterval() time.Duration {
	return 4*time.Second + time.Duration(rand.Int64N(int64(2*time.Second)))
}

// heartbeatTimeout is under the shortest interval (4 s) so beats cannot overlap.
const heartbeatTimeout = 3500 * time.Millisecond
