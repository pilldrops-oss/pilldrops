package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/egress"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// keyedCreateContext carries the caller's Idempotency-Key to the node, which
// claims it before any irreversible side effect. A caller who sent none gets
// none: nothing is minted here.
//
// This tier used to mint one, on the reasoning that a failure after the bytes
// leave is indistinguishable from a request that never arrived. True, but a
// minted key protects no retry that exists — forward() never replays a POST
// except through notDelivered, which fires only on a dial error, where nothing
// was delivered and no key is needed, and the caller cannot present a key it was
// never shown, so its own retry arrives as a new request with a new key either
// way. What the mint did buy was real: a row in the node's idempotency table for
// every create, and a store blip on that row answering 503 "idempotency check
// failed" for a guarantee nobody had asked for. Measured at 1 create in 15 to 1
// in 30, and logged at neither tier.
//
// A POST retry added here later must mint its key HERE, beside the retry it
// makes safe.
func keyedCreateContext(r *http.Request) context.Context {
	if idem := strings.TrimSpace(r.Header.Get("Idempotency-Key")); idem != "" {
		return withIdempotencyKey(r.Context(), idem)
	}
	return r.Context()
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(keyedCreateContext(r))
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, 413, "request body exceeds the size limit")
			return
		}
		writeErr(w, 400, "read body: "+err.Error())
		return
	}
	// Domain egress rules are refused here as well as on the node, when NO node
	// can enforce them: answering before placement is a better error than a 501
	// from whichever node the create happened to land on. When some node does
	// run the name layer, the create passes and placement is filtered onto one
	// (wantsDomains, below). Address and CIDR rules always pass through and are
	// enforced in the sandbox's netns. The code string is duplicated from
	// httpapi rather than importing node code across the tier line.
	var peek struct {
		Network *egress.Rules `json:"network"`
	}
	wantsDomains := json.Unmarshal(body, &peek) == nil && peek.Network.HasDomains()
	// A chosen id is a move, which only this tier's own move path makes: the
	// node honours it from the control plane's key, so a caller's must stop here.
	move, moving := r.Context().Value(moveCtxKey{}).(moveCtx)
	var idPeek struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &idPeek) == nil && idPeek.ID != "" && !moving {
		writeErr(w, http.StatusBadRequest, "id is chosen by the platform; to move a sandbox use POST /v1/sandboxes/{id}/move")
		return
	}
	if wantsDomains && !s.reg.AnyWithCapability(cluster.CapEgressDomains, s.now(), cluster.NodeTTL) {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "domain egress rules need the name layer that reads TLS SNI and the HTTP " +
				"Host header, and no online node runs it, so the create is refused rather than " +
				"returning a sandbox that looks confined and is not — address and CIDR rules " +
				"are enforced and work today",
			"code": "egress_not_enforced",
		})
		return
	}
	// Resolve secret_refs into env before admission so a create naming a
	// secret it cannot have fails on the secret, not later on capacity.
	body, err = s.resolveSecretRefs(r, body)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// A create naming a snapshot is rewritten into an ordinary create of the
	// snapshot's own template id, at the snapshot's own size and runtime, before
	// admission — so the plan is charged for what will actually be provisioned.
	var fromSnapshot *store.SnapshotView
	{
		var peek sandbox.Spec
		_ = json.Unmarshal(body, &peek) //nolint:errcheck // best-effort peek; a malformed body fails for real when the node parses it
		// snap-* ids are snapshot captures, booted only by naming the snapshot
		// (or by this tier's own move); named directly, a tenant could boot
		// another sandbox's capture.
		if templates.Internal(peek.Template) && !moving {
			writeErr(w, http.StatusBadRequest, "template "+peek.Template+" is a snapshot capture; create from a snapshot by its name instead")
			return
		}
		if peek.Snapshot != "" {
			resolved, ok := s.resolveSnapshotCreate(w, r, body, peek)
			if !ok {
				return
			}
			body, fromSnapshot = resolved.Body, &resolved.View
		}
	}
	// admitBody, not admit: the body is already drained above.
	// The moved sandbox already holds its place; the org being at its cap must
	// not strand it on a node that is going away.
	releaseSlot, msg, ok := func() {}, "", true
	if !moving {
		releaseSlot, msg, ok = s.admitBody(r, body)
	}
	if !ok {
		// A limit that could not be read is a 503, not a 429 the tenant would act on.
		if msg == admissionUnavailable {
			w.Header().Set("Retry-After", "5")
			writeErr(w, http.StatusServiceUnavailable, msg)
			return
		}
		writeErr(w, 429, msg)
		return
	}
	defer releaseSlot()
	// Peek at the requested size to schedule; the node does the real sizing.
	var req sandbox.Spec
	_ = json.Unmarshal(body, &req) //nolint:errcheck // best-effort peek only; a malformed body fails for real when the node parses it below
	memMB := req.MemoryMB
	if memMB <= 0 {
		memMB = 1024 // conservative estimate when the caller omits size
	}
	runtime := wantRuntime(req.Runtime, req.Isolation)
	// A template pinned to one runtime overrides; a conflicting explicit pin is a 400.
	if tr := templateRuntime(req.Template); tr != "" {
		if runtime != "" && runtime != tr {
			writeErr(w, 400, "template "+req.Template+" requires the "+tr+" runtime and cannot run under "+runtime)
			return
		}
		runtime = tr
	}
	// Only the docker runtime can run an image with its own entrypoint.
	if req.Image != "" {
		if runtime != "" && runtime != "docker" {
			writeErr(w, 400, "image-backed template "+req.Template+" requires the docker runtime and cannot run under "+runtime)
			return
		}
		runtime = "docker"
	}
	// Nodes advertise canonical template ids, so resolve the alias. An
	// image-backed create is exempt from template matching.
	canonTemplate := ""
	if req.Image == "" && req.Template != "" {
		canonTemplate = req.Template
		if t, ok := templates.Resolve(req.Template); ok && t.ID != "" {
			canonTemplate = t.ID
		}
	}
	// A snapshot's template id is already canonical — no catalogue entry has
	// that name — and its runtime is pinned so a memory image never lands on a
	// Docker node. Both ride the ordinary Request; the scheduler learns nothing new.
	if fromSnapshot != nil {
		canonTemplate = store.SnapshotTemplateID(fromSnapshot.OrgID, fromSnapshot.Name)
		runtime = fromSnapshot.Runtime
	}
	// Reserved atomically so a concurrent create cannot pick the same node
	// before the heartbeat reflects this one. Released on any failure below.
	// The capability filters: a node without the bit would take the create and
	// then answer 501, which is not a placement at all. TagNetworks had the
	// filter and no caller setting it, so the bit was inert.
	//
	// Applied only once some node advertises the capability. A heartbeat field a
	// node's build predates arrives absent, which reads as false, and filtering
	// on that would refuse every create from a snapshot for the length of a
	// rolling upgrade.
	// ponytail: "nobody advertises it" is read as a pre-upgrade fleet rather
	// than as a fleet that lacks it. The cost is the all-off fleet, where
	// placement stays unfiltered and the node answers 501 as it does today.
	// Carry a tri-state through the heartbeat if that case starts mattering.
	place := cluster.Request{VCPU: req.VCPU, MemMB: memMB, Zone: req.Zone, Runtime: runtime, Template: canonTemplate,
		Avoid: []string{move.from}}
	if fromSnapshot == nil {
		place.NoBuild = s.noReadyBuild(r.Context(), canonTemplate)
	}
	if fromSnapshot != nil && s.reg.AnyWithCapability(cluster.CapNamedSnapshot, s.now(), cluster.NodeTTL) {
		place.NamedSnapshot = true
	}
	// A memory image restores only on a CPU providing at least what captured
	// it; anything poorer loads the image and then kills the guest with SIGILL.
	// Unconditional, unlike the capability bits above: a node that reports no
	// features is let through by the filter itself, so there is nothing to gate
	// on a rolling upgrade.
	if fromSnapshot != nil {
		place.CPUFeatures = fromSnapshot.CPUFeatures
	}
	if len(req.Networks) > 0 && s.reg.AnyWithCapability(cluster.CapTagNetworks, s.now(), cluster.NodeTTL) {
		place.TagNetworks = true
	}
	// Unconditional, unlike the bits above: the refusal higher up already
	// established that some node advertises it, so this can only narrow.
	place.EgressDomains = wantsDomains
	// A node can refuse for a reason of its own that its last heartbeat did not
	// show yet: full, no boot slot, disk pressure. It says so with X-Node-Busy,
	// and the create goes to the next node rather than back to the caller.
	var node *cluster.Node
	var resp *http.Response
	var rb []byte
	for tries := 1; ; tries++ {
		next := s.reg.PickAndReserve(place, s.now(), cluster.NodeTTL)
		if next == nil && resp != nil {
			break // every node that could take it was busy: relay the last refusal
		}
		node = next
		if node == nil {
			if req.Image != "" && !s.reg.AnyByRuntime("docker", s.now(), cluster.NodeTTL) {
				// On an fc-only fleet the docker pin makes this a capability miss, not capacity.
				s.metrics.recordSchedule(schedNoRuntime)
				writeErr(w, 400, "template "+req.Template+" runs a service image ("+req.Image+") and needs a docker-runtime node — none is online")
				return
			}
			// A template pinned to a runtime no online node runs is a capability
			// miss, not capacity: retrying never helps, so it is a 400 naming the
			// runtime rather than a 503 telling the caller to wait.
			if runtime != "" && !s.reg.AnyByRuntime(runtime, s.now(), cluster.NodeTTL) {
				s.metrics.recordSchedule(schedNoRuntime)
				writeErr(w, 400, "template "+req.Template+" requires the "+runtime+
					" runtime — no "+runtime+" node is online")
				return
			}
			// "Nothing can boot this template" is a 400; "capacity is tight" is a retryable 503.
			if canonTemplate != "" {
				if supported, bootable := s.reg.TemplateSupport(place, s.now(), cluster.NodeTTL); !supported {
					msg := "template " + req.Template + " is not available on any online node"
					if len(bootable) > 0 {
						msg += " — bootable now: " + strings.Join(bootable, ", ")
					}
					s.metrics.recordSchedule(schedNoTemplate)
					writeErr(w, 400, msg)
					return
				}
			}
			// A snapshot no online CPU can run is a capability miss, not capacity:
			// retrying never helps and "no node has capacity" would send an operator
			// looking for RAM. Named, because the answer is to add a node of at
			// least the capturing one's generation.
			if len(place.CPUFeatures) > 0 && !s.reg.AnyProvidingCPU(place, s.now(), cluster.NodeTTL) {
				s.metrics.recordSchedule(schedNoRuntime)
				writeErr(w, 400, "snapshot "+req.Snapshot+" was captured on a CPU providing "+
					strings.Join(place.CPUFeatures, ", ")+" and no online node provides all of them. "+
					"Its guest already chose code paths for those instructions, so a poorer CPU would "+
					"return a sandbox that dies with SIGILL rather than one that runs")
				return
			}
			// A sandbox bigger than any machine in the fleet is a permanent refusal,
			// not pressure: no amount of waiting frees memory a node does not have,
			// so a retryable 503 sends the caller round a loop that cannot end.
			if biggest := s.reg.LargestMemMB(s.now(), cluster.NodeTTL); biggest > 0 && memMB > biggest {
				s.metrics.recordSchedule(schedTooBig)
				writeErr(w, 400, fmt.Sprintf("this sandbox asks for %d MiB of memory and the largest online node has %d MiB",
					memMB, biggest))
				return
			}
			// A domain allowlist pinned to a runtime none of whose nodes runs the
			// name layer (docker never does) is a capability miss, not capacity.
			// Unpinned, the refusal at the top already established some node can
			// enforce it, so a miss here is those nodes being full: a 503.
			if place.EgressDomains && runtime != "" && !s.anyEgressDomainsOn(runtime) {
				s.metrics.recordSchedule(schedNoRuntime)
				writeJSON(w, http.StatusNotImplemented, map[string]string{
					"error": "the " + runtime + " runtime does not support egress allowlists that name a domain: " +
						"only firecracker nodes run the name layer that enforces them, so create the " +
						`sandbox with "runtime":"firecracker"`,
					"code": "egress_not_enforced",
				})
				return
			}
			// Only this branch means "add nodes".
			s.metrics.recordSchedule(schedNoCapacity)
			w.Header().Set("Retry-After", "5")
			if runtime != "" {
				writeErr(w, 503, "no "+runtime+" node has capacity for this sandbox")
				return
			}
			writeErr(w, 503, "no node has capacity for this sandbox")
			return
		}
		s.metrics.recordSchedule(schedPlaced)
		r2, err := s.forward(r.Context(), node, "POST", "/v1/sandboxes", body, r.Header.Get("Content-Type"), fastOrg(r))
		if err != nil {
			s.reg.Release(node.ID, memMB) // create never happened, free the hold
			writeErr(w, 502, "node create failed: "+err.Error())
			return
		}
		rb, err = io.ReadAll(r2.Body)
		_ = r2.Body.Close()
		if err != nil {
			// Treat a truncated body like a transport failure; the next heartbeat reconciles.
			s.reg.Release(node.ID, memMB)
			writeErr(w, 502, "reading node create response: "+err.Error())
			return
		}
		resp = r2
		if resp.StatusCode == 201 {
			break
		}
		s.reg.Release(node.ID, memMB) // node rejected the create, free the hold
		if resp.Header.Get("X-Node-Busy") == "" || tries == maxCreateTries {
			break
		}
		s.log().InfoContext(r.Context(), "node refused a create as busy, placing it elsewhere",
			"component", "orchestrator", "node", node.ID, "status", resp.StatusCode, "try", tries)
		place.Avoid = append(place.Avoid, node.ID)
	}
	if resp.StatusCode == 201 {
		var created struct {
			ID     string `json:"id"`
			AppURL string `json:"app_url"`
			URL    string `json:"url"` // legacy fallback
		}
		if json.Unmarshal(rb, &created) == nil && created.ID != "" {
			s.reg.Place(created.ID, node.ID)
			if st := s.st.Load(); st != nil {
				_ = st.SetSandboxAttribution(r.Context(), created.ID, store.ClassifySource(req.Client), req.Client) //nolint:errcheck // best-effort: a failure here just means attribution shows up later, not never
			}
			// Persisted so every replica agrees; the in-memory map is the no-store
			// fallback. Before the links are minted: the flag is part of the
			// generation their tokens are bound to.
			if req.Public {
				if err := s.setPublicPreview(r.Context(), created.ID, true); err != nil {
					s.log().ErrorContext(r.Context(), "could not persist public preview; the caller is about to be handed a link that will not work",
						"component", "orchestrator", "sandbox", created.ID, "err", err)
				}
			}
			// Rewrite the preview URL to come back through the orchestrator.
			prev := created.AppURL
			if prev == "" {
				prev = created.URL
			}
			to, viewer := s.previewLinks(r, prev)
			rb = s.rewriteURL(r, rb, prev, to, viewer, node.ID)
			if req.Public {
				fields := map[string]any{"public": true, "public_preview": true}
				if port, ok := platformPortIn(req); ok {
					fields["warning"] = fmt.Sprintf("public: true does not share this sandbox's preview: port %d is a "+
						"terminal or desktop port, which gives shell access, so it always needs an access token "+
						"from the owner. Mint one with GET /v1/sandboxes/%s/preview-token?port=%d", port, created.ID, port)
				}
				rb = injectJSONFields(rb, fields)
			}
			rb = s.withPreviewGeneration(r.Context(), rb, created.ID)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(rb) //nolint:errcheck // best-effort: status/headers are already committed, nothing left to do on a write failure
}

// anyEgressDomainsOn reports whether a schedulable node of runtime runs the
// name layer, as AnyByRuntime does for the runtime alone.
func (s *Server) anyEgressDomainsOn(runtime string) bool {
	now := s.now()
	for _, n := range s.reg.Nodes() {
		if n.Schedulable(now, cluster.NodeTTL) && n.Runtime == runtime && n.EgressDomains {
			return true
		}
	}
	return false
}

// platformPortIn returns the first terminal or desktop port the create would
// preview, its app port or a requested ports entry: public: true never
// shares those, so the caller is warned rather than handed a link that 403s.
func platformPortIn(req sandbox.Spec) (int, bool) {
	var tmpl sandbox.TemplateDefaults
	if t, ok := templates.Resolve(req.Template); ok {
		tmpl.Port = t.Port
	}
	for _, p := range append([]int{sandbox.AppPort(req, tmpl)}, req.Ports...) {
		if rt.IsPlatformPort(p) {
			return p, true
		}
	}
	return 0, false
}

// maxCreateTries bounds how many nodes one create is offered to when each
// refuses as busy, so a saturated fleet answers in a few hops, not N.
const maxCreateTries = 3

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	// Fast path: answer from the shared store rather than round-trip through the nodes.
	if s.listFromStore(w, r) {
		return
	}
	// Nodes share one Postgres and each restores every record on boot, so the
	// same sandbox can surface from several nodes. Prefer a running copy.
	byID := map[string]json.RawMessage{}
	var order []string         // ids in first-seen order
	var anon []json.RawMessage // records without an id
	var unreachable []string   // nodes that could not be read
	stateOf := func(raw json.RawMessage) string {
		var m struct {
			State  string `json:"state"`
			Status string `json:"status"`
		}
		_ = json.Unmarshal(raw, &m) //nolint:errcheck // best-effort: a record that doesn't parse just falls through to Status below (both zero-value "" is handled by the caller)
		if m.State != "" {
			return m.State
		}
		return m.Status
	}
	// Fan out concurrently; aggregate in registry order so output is deterministic.
	nodes := s.reg.Healthy(s.now())
	type nodePage struct {
		sandboxes []json.RawMessage
		failed    bool
	}
	pages := make([]nodePage, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func(i int, n *cluster.Node) {
			defer wg.Done()
			resp, err := s.forward(r.Context(), n, "GET", "/v1/sandboxes", nil, "", fastOrg(r))
			if err != nil {
				pages[i].failed = true
				return
			}
			b, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close() //nolint:errcheck // best-effort: body already (attempted to be) fully read above
			if err != nil {
				pages[i].failed = true
				return
			}
			var page struct {
				Sandboxes []json.RawMessage `json:"sandboxes"`
			}
			if resp.StatusCode >= 300 || json.Unmarshal(b, &page) != nil {
				pages[i].failed = true
				return
			}
			pages[i].sandboxes = page.Sandboxes
		}(i, n)
	}
	wg.Wait()
	for i, n := range nodes {
		if pages[i].failed {
			unreachable = append(unreachable, n.ID)
			continue
		}
		for _, raw := range pages[i].sandboxes {
			var meta struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(raw, &meta) //nolint:errcheck // best-effort: a record that doesn't parse just falls through as anonymous below
			if meta.ID == "" {
				anon = append(anon, raw)
				continue
			}
			if prev, ok := byID[meta.ID]; !ok {
				byID[meta.ID] = raw
				order = append(order, meta.ID)
			} else if stateOf(raw) == "running" && stateOf(prev) != "running" {
				byID[meta.ID] = raw // prefer the running copy over a paused ghost
			}
		}
	}
	all := make([]json.RawMessage, 0, len(order)+len(anon))
	for _, id := range order {
		all = append(all, byID[id])
	}
	all = append(all, anon...)
	out := map[string]any{"sandboxes": all}
	if len(unreachable) > 0 {
		out["incomplete"] = true
		out["unreachable_nodes"] = unreachable
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleDestroy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.locks != nil && s.locks.Held(moveLockName(id)) {
		w.Header().Set("Retry-After", "10")
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "this sandbox is being moved to another node; destroy it once the move finishes",
			"code":  "sandbox_moving",
		})
		return
	}
	node, ok := s.reg.NodeFor(id)
	if !ok {
		// A lost sandbox has no node to ask; destroying it is forgetting it.
		if rec, lost := s.lostSandbox(r, id); lost {
			if st := s.st.Load(); st != nil {
				now := s.now().Unix()
				_ = st.RecordHistory(r.Context(), store.HistoryRecord{ //nolint:errcheck // best-effort: the row delete below is what matters
					ID: rec.ID, OrgID: rec.OrgID, Template: rec.Template, Node: rec.NodeID,
					ExitState: store.StateLost, CreatedAt: rec.CreatedAt, DestroyedAt: now,
					DurationSecs: now - rec.CreatedAt, VCPU: rec.VCPU, MemMiB: rec.MemMiB,
				})
				if err := st.ForgetSandbox(r.Context(), id); err != nil {
					writeErr(w, http.StatusServiceUnavailable, "could not forget the lost sandbox: "+err.Error())
					return
				}
			}
			writeJSON(w, http.StatusOK, map[string]any{"id": id, "state": "destroyed", "was": store.StateLost})
			return
		}
		writeErr(w, 404, "sandbox not found")
		return
	}
	resp, err := s.forward(r.Context(), node, "DELETE", "/v1/sandboxes/"+id, nil, "", fastOrg(r))
	if err != nil {
		writeErr(w, 502, "node destroy failed: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort: relayed to the client below
	// Unplace only on success: the node 404s another org's sandbox, and
	// unplacing on that would let an id guess strip a running sandbox's route.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		s.reg.Unplace(id)
	}
	relay(w, resp)
}

func (s *Server) handleSandboxGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	node, ok := s.reg.NodeFor(id)
	if !ok {
		// Paused on a node that is gone: it still exists, and the first action
		// on it restores it elsewhere. A read does not start that download.
		if rec, orphaned := s.orphanedPaused(r, id); orphaned {
			writeJSON(w, http.StatusOK, map[string]any{
				"id": rec.ID, "name": rec.Name, "template": rec.Template, "state": "paused",
				"vcpu": rec.VCPU, "memory_mb": rec.MemMiB, "created_at": rec.CreatedAt,
				"note": "its node is gone; resuming restores it on another node from its archived snapshot",
			})
			return
		}
		if _, lost := s.lostSandbox(r, id); lost {
			writeJSON(w, http.StatusGone, map[string]string{
				"error": "sandbox " + id + " was on a node that stopped answering and was removed " +
					"from the fleet; DELETE it to clear the record",
				"code": "sandbox_lost",
			})
			return
		}
		writeErr(w, 404, "sandbox not found")
		return
	}
	resp, err := s.forward(r.Context(), node, "GET", "/v1/sandboxes/"+id, nil, "", fastOrg(r))
	if err != nil {
		writeErr(w, 502, "node request failed: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort: relayed to the client below
	// The body is opened, not relayed: the node mints url/terminal_url/
	// desktop_url against its own host and stamps its hop ?ptok= on each, and
	// relaying that hands the caller a credential this tier must not pass on
	// (previewLinks) plus a link it cannot open. Forwards are this tier's rows,
	// which the node has never heard of, so they are merged in the same pass.
	if resp.StatusCode == http.StatusOK {
		var doc struct {
			AppURL string `json:"app_url"`
			URL    string `json:"url"`
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil || json.Unmarshal(body, &doc) != nil {
			// The body is consumed, so falling through would serve a truncated
			// answer. Say so instead.
			writeErr(w, http.StatusBadGateway, "node answer could not be read")
			return
		}
		prev := doc.AppURL
		if prev == "" {
			prev = doc.URL
		}
		body = s.pointLinksHere(r, body, prev, node.ID)
		extra := map[string]any{"public_preview": s.isPublicPreview(r.Context(), id)}
		if fw := s.forwardsFor(r, id); fw != nil {
			extra["forwards"] = fw
		}
		body = s.withPreviewGeneration(r.Context(), injectJSONFields(body, extra), id)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body) //nolint:errcheck // best-effort: headers are committed
		return
	}
	relay(w, resp)
}

// pointLinksHere rewrites a node's answer so every link in it names this tier.
// prev is the preview URL the node minted for the sandbox's own port, "" when
// the body carries none.
//
// No viewer_url, deliberately: this tier has only ever minted one on create and
// fork, and the CLI's open verb reads viewer_url before desktop_url, so one here
// would silently send `sandbox open` on a desktop sandbox back to the app port.
// url stays bare, as it is on create; the token belongs on viewer_url and on the
// platform-port links that have no viewer_url of their own.
func (s *Server) pointLinksHere(r *http.Request, body []byte, prev, nodeID string) []byte {
	to, _ := s.previewLinks(r, prev)
	return s.rewriteURL(r, body, prev, to, "", nodeID)
}

func (s *Server) handleSandboxRename(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	node, ok := s.reg.NodeFor(id)
	if !ok {
		writeErr(w, 404, "sandbox not found")
		return
	}
	var body []byte
	if r.Body != nil {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			writeErr(w, 400, "read body: "+err.Error())
			return
		}
		body = b
	}
	resp, err := s.forward(r.Context(), node, "PATCH", "/v1/sandboxes/"+id, body, r.Header.Get("Content-Type"), fastOrg(r))
	if err != nil {
		writeErr(w, 502, "node request failed: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort: relayed to the client below
	// set-port and rename both land here, and set-port's whole answer is the
	// new preview link. Relayed verbatim it was the node's own address, which
	// on a fleet is not a host the caller can reach.
	if resp.StatusCode == http.StatusOK {
		var doc struct {
			AppURL string `json:"app_url"`
			URL    string `json:"url"`
		}
		rb, err := io.ReadAll(resp.Body)
		if err != nil || json.Unmarshal(rb, &doc) != nil {
			writeErr(w, http.StatusBadGateway, "node answer could not be read")
			return
		}
		prev := doc.AppURL
		if prev == "" {
			prev = doc.URL
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(s.pointLinksHere(r, rb, prev, node.ID)) //nolint:errcheck // best-effort: headers are committed
		return
	}
	relay(w, resp)
}

func (s *Server) handleSandboxProxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	node, ok := s.reg.NodeFor(id)
	if !ok {
		rec, orphaned := s.orphanedPaused(r, id)
		if !orphaned {
			writeErr(w, 404, "sandbox not found")
			return
		}
		if node, ok = s.rehome(w, r, rec); !ok {
			return
		}
	}
	path := "/v1/sandboxes/" + id + "/" + r.PathValue("rest")
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	// Stream the body through; a large upload must not be buffered in the control plane.
	resp, err := s.forwardStream(r.Context(), node, r.Method, path, r.Body,
		r.Header.Get("Content-Type"), r.Header, fastOrg(r))
	if err != nil {
		writeErr(w, 502, "node request failed: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // best-effort: relayed to the client below
	relay(w, resp)
}

// lostSandbox returns the caller's row for id when it is marked lost, applying
// the same org and visibility rule as the list, so a lost sandbox is no more
// visible than a live one.
func (s *Server) lostSandbox(r *http.Request, id string) (store.SandboxRecord, bool) {
	st := s.st.Load()
	if st == nil {
		return store.SandboxRecord{}, false
	}
	rec, ok, err := st.SandboxByID(r.Context(), id)
	if err != nil || !ok || rec.State != store.StateLost || rec.OrgID != fastOrg(r) {
		return store.SandboxRecord{}, false
	}
	keyID, keyRole := callerKey(r.Context())
	if !sandbox.Visible(rec.ID, rec.Visibility, rec.CreatedBy, keyID, keyRole == "operator") {
		return store.SandboxRecord{}, false
	}
	return rec, true
}

// moveCtx marks a create as the second half of a move: it may carry an id and
// must not land on the node it is leaving.
type moveCtx struct{ from string }

type moveCtxKey struct{}
