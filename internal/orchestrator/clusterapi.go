package orchestrator

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// zoneAgg accumulates a zone's totals while iterating the fleet.
type zoneAgg struct {
	nodes     int
	cores     int
	memFreeMB int
	active    int
	headroom  int // standard sandboxes still schedulable in this zone
	maxStd    int // standard-sandbox capacity of schedulable nodes
}

// handleClusterStats reports fleet capacity, per-zone headroom and a scale recommendation.
func (s *Server) handleClusterStats(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	nodes := s.reg.Nodes()

	var totalNodes, healthyNodes, availNodes, fullNodes, cordonedNodes, unreachableNodes int
	var coresTotal, memTotalMB, memAvailMB, active, headroom int
	zones := map[string]*zoneAgg{}

	for _, n := range nodes {
		totalNodes++
		st := n.Status(now, cluster.NodeTTL)
		switch st {
		case "available":
			availNodes++
		case "full":
			fullNodes++
		case "cordoned", "draining":
			cordonedNodes++
		case "unreachable":
			unreachableNodes++
		}
		if n.Healthy(now, cluster.NodeTTL) {
			healthyNodes++
		}
		coresTotal += n.Cap.Cores
		memTotalMB += n.Cap.MemTotalMB
		memAvailMB += n.Cap.MemAvailMB
		active += n.Cap.Active

		z := zones[n.Zone]
		if z == nil {
			z = &zoneAgg{}
			zones[n.Zone] = z
		}
		z.nodes++
		z.cores += n.Cap.Cores
		z.memFreeMB += n.Cap.MemAvailMB
		z.active += n.Cap.Active
		if n.Schedulable(now, cluster.NodeTTL) {
			fits := n.StandardFits(now, cluster.NodeTTL)
			headroom += fits
			z.headroom += fits
			z.maxStd += n.StandardCapacity()
		}
	}

	zoneNames := make([]string, 0, len(zones))
	for name := range zones {
		zoneNames = append(zoneNames, name)
	}
	sort.Strings(zoneNames)
	zoneOut := make([]map[string]any, 0, len(zoneNames))
	anyZoneFull := false
	for _, name := range zoneNames {
		z := zones[name]
		zStatus := zoneStatus(z.headroom, z.maxStd)
		if zStatus == "full" {
			anyZoneFull = true
		}
		zoneOut = append(zoneOut, map[string]any{
			"zone": name, "nodes": z.nodes, "cores": z.cores,
			"mem_free_gb": mbToGB(z.memFreeMB), "sandboxes_active": z.active,
			"headroom_sandboxes": z.headroom, "status": zStatus,
		})
	}

	rec := scaleRecommendation(headroom, active, healthyNodes, anyZoneFull, nodes, now)

	writeJSON(w, 200, map[string]any{
		"nodes_total": totalNodes, "nodes_healthy": healthyNodes,
		"nodes_available": availNodes, "nodes_full": fullNodes,
		"nodes_cordoned": cordonedNodes, "nodes_unreachable": unreachableNodes,
		"cores_total":  coresTotal,
		"mem_total_gb": mbToGB(memTotalMB), "mem_used_gb": mbToGB(memTotalMB - memAvailMB),
		"mem_free_gb":      mbToGB(memAvailMB),
		"sandboxes_active": active, "headroom_sandboxes": headroom,
		"zones": zoneOut, "recommendation": rec,
	})
}

// zoneStatus classifies a zone by headroom fraction: "ok" >=20%, "tight" 5-20%, "full" <5%.
func zoneStatus(headroom, maxStd int) string {
	if maxStd <= 0 {
		return "full"
	}
	frac := float64(headroom) / float64(maxStd)
	switch {
	case frac >= 0.20:
		return "ok"
	case frac >= 0.05:
		return "tight"
	default:
		return "full"
	}
}

// scaleRecommendation says how many nodes to add to restore 20% headroom, with a severity level.
func scaleRecommendation(headroom, active, healthyNodes int, anyZoneFull bool, nodes []*cluster.Node, now time.Time) map[string]any {
	maxStd := 0
	for _, n := range nodes {
		if n.Schedulable(now, cluster.NodeTTL) {
			maxStd += n.StandardCapacity()
		}
	}
	want := int(0.20 * float64(maxStd))
	level, message, suggested := "ok", "Cluster has healthy headroom.", 0
	switch {
	case healthyNodes == 0 || anyZoneFull:
		level = "critical"
		message = "Cluster is out of capacity — add nodes now."
	case headroom < want:
		level = "warn"
		message = "Headroom is running low — consider adding a node."
	}
	if level != "ok" {
		perNode := avgStandardCapacity(nodes, now)
		if perNode <= 0 {
			perNode = 1
		}
		deficit := want - headroom
		if deficit < perNode {
			deficit = perNode
		}
		suggested = (deficit + perNode - 1) / perNode
		if suggested < 1 {
			suggested = 1
		}
	}
	return map[string]any{"level": level, "message": message, "suggested_nodes": suggested}
}

// avgStandardCapacity is the mean standard-sandbox capacity of healthy nodes, 0 when none.
func avgStandardCapacity(nodes []*cluster.Node, now time.Time) int {
	sum, count := 0, 0
	for _, n := range nodes {
		if n.Healthy(now, cluster.NodeTTL) {
			sum += n.StandardCapacity()
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return sum / count
}

func mbToGB(mb int) int { return mb / 1024 }

func (s *Server) handleCordon(w http.ResponseWriter, r *http.Request) {
	if !s.requireFleetOperator(w, r) {
		return
	}
	s.setNodeFlag(w, r, s.reg.SetCordon, true)
}

// handleUncordon returns a node to service: it clears the drain flag as well
// as the cordon. Nothing else ever cleared a drain, so a node drained for
// maintenance could only come back by being removed and re-joined as a new node.
func (s *Server) handleUncordon(w http.ResponseWriter, r *http.Request) {
	if !s.requireFleetOperator(w, r) {
		return
	}
	s.reg.SetDrain(r.PathValue("id"), false)
	s.setNodeFlag(w, r, s.reg.SetCordon, false)
}

func (s *Server) handleDrain(w http.ResponseWriter, r *http.Request) {
	if !s.requireFleetOperator(w, r) {
		return
	}
	id := r.PathValue("id")
	if !s.reg.SetDrain(id, true) {
		writeErr(w, 404, "node not found")
		return
	}
	// ?migrate=1 moves every sandbox off the node, one at a time, before
	// answering. A sandbox whose move fails stays where it was and is listed.
	// ponytail: synchronous and serial, so a big node makes a long request;
	// a background job with progress if drains get routine.
	var moved []moveResult
	if r.URL.Query().Get("migrate") == "1" {
		if st := s.st.Load(); st != nil && s.blobStore() != nil {
			from, _ := s.reg.Node(id)
			for _, sbID := range s.reg.PlacementsOn(id) {
				rec, found, err := st.SandboxByID(r.Context(), sbID)
				if err == nil && !found {
					// A route with no sandbox behind it is left over from a
					// destroy; there is nothing to move, only a route to drop.
					s.reg.Unplace(sbID)
					continue
				}
				if err != nil || from == nil {
					moved = append(moved, moveResult{ID: sbID, From: id, Error: "no record to move"})
					continue
				}
				moved = append(moved, s.moveSandbox(r.Context(), st, rec, from))
			}
		} else {
			writeErr(w, http.StatusServiceUnavailable,
				"migrate needs the shared database and an artifact store (ORCHESTRATOR_ARTIFACT_DIR)")
			return
		}
	}
	// The node is safe to remove once no sandboxes remain on it.
	remaining := s.reg.PlacementsOn(id)
	out := map[string]any{
		"ok":                  true,
		"draining":            id,
		"sandboxes_remaining": len(remaining),
		"remaining":           remaining,
	}
	if moved != nil {
		out["moved"] = moved
	}
	writeJSON(w, 200, out)
}

// setNodeFlag applies an operator flag (cordon/drain) to a node by id.
func (s *Server) setNodeFlag(w http.ResponseWriter, r *http.Request, fn func(string, bool) bool, on bool) {
	if !fn(r.PathValue("id"), on) {
		writeErr(w, 404, "node not found")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleRemoveNode(w http.ResponseWriter, r *http.Request) {
	if !s.requireFleetOperator(w, r) {
		return
	}
	if !s.reg.Remove(r.PathValue("id")) {
		writeErr(w, 404, "node not found")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// nodeKeyFor derives a node's API key from id+zone with HMAC-SHA256 over
// NODE_KEY_SECRET, so the key never has to travel in the heartbeat.
func (s *Server) nodeKeyFor(id, zone string) string {
	if zone == "" {
		zone = "default"
	}
	mac := hmac.New(sha256.New, s.nodeKeyDerivationSecret())
	_, _ = mac.Write([]byte(id + "|" + zone))
	return "sf_node_" + hex.EncodeToString(mac.Sum(nil))[:32]
}

// nodeHeartbeatToken is the per-node bearer for /internal/heartbeat, so a
// compromised node cannot heartbeat as another. Domain-separated from nodeKeyFor.
func (s *Server) nodeHeartbeatToken(id, zone string) string {
	if zone == "" {
		zone = "default"
	}
	mac := hmac.New(sha256.New, s.nodeKeyDerivationSecret())
	_, _ = mac.Write([]byte("hb|" + id + "|" + zone))
	return "sf_hb_" + hex.EncodeToString(mac.Sum(nil))[:32]
}

// nodeKeyDerivationSecret is control-plane only; nodes get their own key at
// join and never the secret, so they cannot derive each other's. An empty
// secret is refused at boot and in handleHeartbeat.
func (s *Server) nodeKeyDerivationSecret() []byte {
	return []byte(s.nodeKeySecret)
}

// firstHealthy returns any healthy node to proxy shared endpoints to.
func (s *Server) firstHealthy() (*cluster.Node, bool) {
	nodes := s.reg.Healthy(s.now())
	if len(nodes) == 0 {
		return nil, false
	}
	return nodes[0], true
}

// proxyToNode relays a request for a shared, node-owned resource, trying each
// healthy node in turn since one can pass the heartbeat TTL with its port down.
func (s *Server) proxyToNode(w http.ResponseWriter, r *http.Request, method, path string) {
	nodes := s.reg.Healthy(s.now())
	if len(nodes) == 0 {
		writeErr(w, 503, "no healthy node")
		return
	}
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	ctype, org := r.Header.Get("Content-Type"), fastOrg(r)
	var lastErr error
	for _, node := range nodes {
		resp, err := s.forward(r.Context(), node, method, path, body, ctype, org)
		if err != nil {
			lastErr = err
			// A non-idempotent request that timed out may have executed; failing
			// over would run it twice.
			if replaySafe(method) || notDelivered(err) {
				continue
			}
			break
		}
		defer resp.Body.Close()
		relay(w, resp)
		return
	}
	writeErr(w, 502, "node request failed: "+lastErr.Error())
}

// handleLimitsProxy and handleUsageProxy relay to any healthy node; the signed
// X-Org-Override scopes the answer to the customer.
func (s *Server) handleLimitsProxy(w http.ResponseWriter, r *http.Request) {
	s.proxyToNode(w, r, "GET", "/v1/limits")
}

func (s *Server) handleUsageProxy(w http.ResponseWriter, r *http.Request) {
	if s.usageFromStore(w, r) {
		return
	}
	s.proxyToNode(w, r, "GET", "/v1/usage")
}

func (s *Server) handleEventsProxy(w http.ResponseWriter, r *http.Request) {
	s.proxyToNode(w, r, "GET", "/v1/events")
}

func (s *Server) handleConcurrencyProxy(w http.ResponseWriter, r *http.Request) {
	s.proxyToNode(w, r, "GET", "/v1/concurrency")
}

// handleImageBuild builds a custom image once, on one node, and publishes the
// result to the shared artifact store. The context is spooled to disk and read
// before a node is picked so an oversize one is a 413, not a 503.
func (s *Server) handleImageBuild(w http.ResponseWriter, r *http.Request) {
	// capBody exempts this route from the generic body cap, so this is the only one.
	ctxFile, ctxSize, err := s.spoolBuildContext(w, r)
	if err != nil {
		return // spoolBuildContext already answered
	}
	defer func() {
		_ = ctxFile.Close()
		_ = os.Remove(ctxFile.Name())
	}()

	// runtime=firecracker publishes a template rootfs, runtime=docker an image
	// tarball. The caller chooses; inferring it from a mixed fleet would make
	// the same request produce different artifact kinds.
	wantRuntime := strings.TrimSpace(r.URL.Query().Get("runtime"))
	healthy := s.reg.Healthy(s.now())
	node, ok := pickBuildNode(healthy, wantRuntime)
	if !ok {
		writeErr(w, 503, noBuildNodeMessage(healthy, wantRuntime))
		return
	}

	// runtime= becomes the node's payload=; the rest of the query passes through.
	q := r.URL.Query()
	q.Del("runtime")
	if p := payloadForRuntime(wantRuntime); p != "" {
		q.Set("payload", p)
	}
	path := "/v1/images/build"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	// The node bounds the build at 15 minutes; this is the outer bound.
	// nodeHopLong rather than a bare client so this hop is pinned like the rest.
	base, longClient, err := s.nodeHopLong(node, 20*time.Minute)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}

	resp := imageBuildResponse{Build: imageBuildStep{Node: node.ID}}
	req, err := http.NewRequestWithContext(r.Context(), "POST", base+path, ctxFile)
	if err != nil {
		writeErr(w, 500, "build request: "+err.Error())
		return
	}
	// net/http cannot infer an *os.File's length and would send it chunked.
	req.ContentLength = ctxSize
	if node.Key != "" {
		req.Header.Set("Authorization", "Bearer "+node.Key)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	// This builds its own request rather than going through forwardOnce, so it
	// asks the one signing site to stamp it; a refusal fails the build.
	if err := s.setOrgAndKey(r.Context(), req, node, "POST", path, fastOrg(r)); err != nil {
		writeErr(w, 503, err.Error())
		return
	}
	nodeResp, err := longClient.Do(req)
	if err != nil {
		resp.Build.Err = "unreachable: " + err.Error()
		resp.Publish.Err = "not attempted: the build did not succeed"
		s.writeImageBuild(w, resp, len(healthy))
		return
	}
	var out nodeBuildResult
	b, _ := io.ReadAll(io.LimitReader(nodeResp.Body, 1<<20))
	nodeResp.Body.Close()
	_ = json.Unmarshal(b, &out)
	resp.Tag, resp.Log = out.Tag, out.Log
	resp.Build.OK = out.OK
	resp.Build.Payload = out.Payload
	resp.Build.Err = errFromLog(out.OK, nodeResp.StatusCode)
	if !out.OK {
		// Prefer the node's message: it names the missing prerequisite.
		if msg := strings.TrimSpace(out.Err); msg != "" {
			resp.Build.Err = msg
		}
		resp.Publish.Err = "not attempted: the build did not succeed"
		s.writeImageBuild(w, resp, len(healthy))
		return
	}

	s.publishBuiltImage(r, node, out, fastOrg(r), &resp.Publish)
	s.writeImageBuild(w, resp, len(healthy))
}

// nodeBuildResult is the node's POST /v1/images/build response.
type nodeBuildResult struct {
	OK  bool   `json:"ok"`
	Tag string `json:"tag"`
	Log string `json:"log"`
	Err string `json:"error"`

	// Payload is what the node actually produced (artifact.NameImage or
	// NameRootfs); publishBuiltImage cross-checks it against the request.
	Payload string `json:"payload"`

	// TemplateID and EnvdSHA256 are set only for a rootfs build.
	TemplateID string `json:"template_id"`
	EnvdSHA256 string `json:"envd_sha256"`
}

// pickBuildNode chooses the node a build runs on. A requested runtime has no
// fallback, since the artifact kinds are not interchangeable. With no
// preference a docker node is preferred because custom images only launch there.
func pickBuildNode(healthy []*cluster.Node, want string) (*cluster.Node, bool) {
	if want = strings.TrimSpace(want); want != "" {
		for _, n := range healthy {
			if n.Runtime == want {
				return n, true
			}
		}
		return nil, false
	}
	for _, n := range healthy {
		if n.Runtime == "docker" {
			return n, true
		}
	}
	if len(healthy) > 0 {
		return healthy[0], true
	}
	return nil, false
}

// noBuildNodeMessage says why no node was picked, naming the runtimes the fleet has.
func noBuildNodeMessage(healthy []*cluster.Node, want string) string {
	if len(healthy) == 0 {
		return "no healthy node to build on"
	}
	if want == "" {
		return "no healthy node to build on"
	}
	have := map[string]int{}
	for _, n := range healthy {
		r := n.Runtime
		if r == "" {
			r = "unknown"
		}
		have[r]++
	}
	kinds := make([]string, 0, len(have))
	for r, n := range have {
		kinds = append(kinds, fmt.Sprintf("%s×%d", r, n))
	}
	sort.Strings(kinds)
	return fmt.Sprintf("no healthy %s node to build on; this fleet has %s. "+
		"A build is not relayed to a different runtime: the artifact it produces "+
		"(a template rootfs vs a Docker image tarball) is not interchangeable",
		want, strings.Join(kinds, ", "))
}

// payloadForRuntime is the artifact name to ask the node for; empty lets the node choose.
func payloadForRuntime(want string) string {
	switch want {
	case "firecracker":
		return artifact.NameRootfs
	case "docker":
		return artifact.NameImage
	default:
		return ""
	}
}

// spoolBuildContext reads the capped request body to a temp file and answers
// the request itself on error.
func (s *Server) spoolBuildContext(w http.ResponseWriter, r *http.Request) (*os.File, int64, error) {
	f, err := os.CreateTemp("", "sf-buildctx-*")
	if err != nil {
		writeErr(w, 500, "stage build context: "+err.Error())
		return nil, 0, err
	}
	n, err := io.Copy(f, http.MaxBytesReader(w, r.Body, s.maxBuildContextBytes))
	if err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, 413, "build context exceeds the size limit")
			return nil, 0, err
		}
		writeErr(w, 400, "read build context: "+err.Error())
		return nil, 0, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		writeErr(w, 500, "stage build context: "+err.Error())
		return nil, 0, err
	}
	return f, n, nil
}

func errFromLog(ok bool, status int) string {
	if ok {
		return ""
	}
	return fmt.Sprintf("build failed (HTTP %d)", status)
}

// fanOut best-effort replays a mutating request to every other healthy node so
// shared flags converge; the primary's response is authoritative.
func (s *Server) fanOut(primary *cluster.Node, method, path string, body []byte, ctype string) {
	for _, n := range s.reg.Healthy(s.now()) {
		if n.ID == primary.ID {
			continue
		}
		if resp, err := s.forward(context.Background(), n, method, path, body, ctype); err == nil {
			resp.Body.Close()
		}
	}
}

func (s *Server) handleImagesList(w http.ResponseWriter, r *http.Request) {
	s.proxyToNode(w, r, "GET", "/v1/images")
}

func (s *Server) handleHistoryList(w http.ResponseWriter, r *http.Request) {
	if s.historyFromStore(w, r) {
		return
	}
	s.proxyToNode(w, r, "GET", "/v1/history")
}

func (s *Server) handleHistoryGet(w http.ResponseWriter, r *http.Request) {
	s.proxyToNode(w, r, "GET", "/v1/history/"+r.PathValue("id"))
}

func (s *Server) handleImagePatch(w http.ResponseWriter, r *http.Request) {
	s.proxyMutateShared(w, r, "PATCH", "/v1/images/"+r.PathValue("id"))
}

// proxyMutateShared forwards a mutating request to the first healthy node, then
// fans it out to the rest so shared state converges.
func (s *Server) proxyMutateShared(w http.ResponseWriter, r *http.Request, method, path string) {
	node, ok := s.firstHealthy()
	if !ok {
		writeErr(w, 503, "no healthy node")
		return
	}
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	ctype := r.Header.Get("Content-Type")
	resp, err := s.forward(r.Context(), node, method, path, body, ctype)
	if err != nil {
		writeErr(w, 502, "node request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	// Only converge the fleet when the primary accepted the change.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		s.fanOut(node, method, path, body, ctype)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(rb)
}

// requireFleetOperator answers 403 and returns false for a customer credential.
//
// Cordon, drain, node removal and join-token minting change the fleet itself,
// not one org's resources: draining takes a machine out of service for every
// tenant, and a join token lets a machine of the caller's choosing enter the
// fleet and be handed other tenants' sandboxes. They were registered behind
// s.auth alone, which authenticates a caller without asking who they are, so
// any tenant key could do all four. The org egress routes already gate the
// same way (see requireEgressOperator); these did not.
func (s *Server) requireFleetOperator(w http.ResponseWriter, r *http.Request) bool {
	if isOperator(r) {
		return true
	}
	writeErr(w, http.StatusForbidden, "operator role required")
	return false
}

// authOperator is auth plus requireFleetOperator, for routes that read or
// change the whole fleet rather than one org's resources.
func (s *Server) authOperator(next http.HandlerFunc) http.HandlerFunc {
	return s.auth(func(w http.ResponseWriter, r *http.Request) {
		if s.requireFleetOperator(w, r) {
			next(w, r)
		}
	})
}
