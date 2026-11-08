package httpapi

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pilldrops-oss/pilldrops/internal/egress"
	"github.com/pilldrops-oss/pilldrops/internal/entitlements"
	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	"github.com/pilldrops-oss/pilldrops/internal/pdconfig"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// codeTemplateFetchFailed marks a create that failed because this node could not
// fetch an existing template from the artifact store. Neither a 400 nor a plain
// retryable 503, so it gets its own code.
const codeTemplateFetchFailed = "template_fetch_failed"

// codeTemplateFetching marks a create refused because this node is still
// downloading the image. Retryable and expected, unlike codeTemplateFetchFailed:
// a named snapshot is 1.6-1.8 GB and the first create on a node that does not
// hold it yet cannot be served.
const codeTemplateFetching = "template_fetching"

// templateFetchWait is how long a create waits for a cold image before it
// answers "still downloading, retry". Short on purpose, and well inside the
// control plane's overall node-client deadline: a tenant learns an image is
// downloading in seconds rather than after the transport gives up with nothing
// to act on. Waiting longer buys nothing — a 1.6 GB snapshot does not land in
// any wait worth holding a request open for.
//
// Exceeding it refuses this one create; the download keeps running detached and
// the next create waits on the same one, so a retry loop converges rather than
// restarting the transfer.
const templateFetchWait = 15 * time.Second

// codeEgressUnenforced marks a create refused because this node cannot enforce
// the egress rules it carried. Address and CIDR rules are enforced in the
// kernel chain of the sandbox's netns; a domain also needs the name layer that
// reads SNI/Host, which runs only on the firecracker/FC_NETNS path. Refused
// rather than dropped, because a sandbox that reads as confined and reaches
// anything is worse than a create that fails.
const codeEgressUnenforced = "egress_not_enforced"

// codePortNotPublished: a single-port runtime was asked to preview a port the
// container never published.
const codePortNotPublished = "port_not_published"

// codeSlugTaken: another live sandbox holds the requested slug.
const codeSlugTaken = "slug_taken"

// codeTagNetworksUnsupported: this node's runtime cannot partition sandboxes
// into network tags. Same shape as the egress refusal and for the same reason:
// the alternative is a sandbox whose row claims a network it is not on.
const codeTagNetworksUnsupported = "tag_networks_unsupported"

// tenantMayBoot reports whether a tenant may create from tmpl and snapshot: a
// catalogue template, or its own org's named snapshot. Another org's snapshot,
// a rehome capture and a move capture (a reserved snapshot name) are not.
func tenantMayBoot(org, tmpl, snapshot string) bool {
	if store.ReservedSnapshotName(snapshot) {
		return false
	}
	if !templates.Internal(tmpl) {
		return true
	}
	own := store.SnapshotTemplateID(org, "")
	name, ok := strings.CutPrefix(tmpl, own)
	return ok && name != "" && !store.ReservedSnapshotName(name)
}

// createReq is the node's create request, the shared sandbox.Spec plus any
// node-only fields.
type createReq struct {
	sandbox.Spec
	// Volumes attach standalone volumes at boot: [{"id": "vol-…", "path": "/data"}].
	Volumes []struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	} `json:"volumes,omitempty"`
}

// persistentCount counts an org's persistent sandboxes. A zero ExpiresAt is the
// same marker the reaper reads.
func persistentCount(boxes []daemon.SandboxInfo) int {
	n := 0
	for _, sb := range boxes {
		if sb.ExpiresAt.IsZero() {
			n++
		}
	}
	return n
}

// expiresOrNil renders a zero ExpiresAt as JSON null instead of the year-1 sentinel.
func expiresOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// mergePilldropsConfig fills unset request fields from the repo's committed config.
func mergePilldropsConfig(req createReq, c pdconfig.Config) createReq {
	if req.Template == "" {
		req.Template = c.Template
	}
	if req.Start == "" {
		req.Start = c.Start
	}
	if len(req.Setup) == 0 {
		req.Setup = c.Setup
	}
	if req.Port == 0 {
		req.Port = c.Port
	}
	if req.VCPU == 0 {
		req.VCPU = c.Resources.VCPU
	}
	if req.MemoryMB == 0 {
		req.MemoryMB = c.Resources.MemoryMB
	}
	if req.StorageMB == 0 {
		req.StorageMB = c.Resources.StorageMB
	}
	return req
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	// Isolation preflight failed at boot: never run untrusted code on this node.
	if !s.isolationOK.Load() {
		nodeBusy(w)
		writeErr(w, http.StatusServiceUnavailable, "node isolation preflight failed — refusing new sandboxes")
		return
	}
	var body createReq
	if !decodeBody(r, &body) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// Merge a committed pilldrops.json under the request before defaulting and
	// validation. A bad file is a 400, never a silent fallback.
	if len(body.Files) > 0 {
		paths := make([]string, len(body.Files))
		for i, f := range body.Files {
			paths[i] = f.Path
		}
		if i, ok := pdconfig.FindIn(paths); ok {
			ids := make([]string, 0)
			for _, t := range templates.Catalog() {
				ids = append(ids, t.ID)
			}
			cfg, err := pdconfig.Parse(body.Files[i].Content, ids)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			body = mergePilldropsConfig(body, cfg)
		}
	}
	if err := egress.CheckWildcards(body.Network); err != nil {
		writeErr(w, http.StatusBadRequest, "network: "+err.Error())
		return
	}
	// Here as well as in daemon.Create, which every boot path passes through:
	// this one runs before the idempotency claim, so a refused create leaves no
	// key behind. Driven by the runtime's capability rather than being static,
	// or the refusal outlives the thing it was standing in for.
	if caps, _ := s.d.RuntimeCaps(); body.Network.HasDomains() && !caps.EgressDomains {
		writeErrCode(w, http.StatusNotImplemented, codeEgressUnenforced,
			daemon.ErrEgressDomainsUnsupported.Error())
		return
	}
	if body.Template == "" {
		body.Template = s.templates[0]
	}
	// agent_mode belongs to agent workstations, which this server does not run.
	if strings.TrimSpace(body.AgentMode) != "" {
		writeErr(w, http.StatusBadRequest, "agent_mode is not supported by this server")
		return
	}
	// Caller-supplied names are checked before the platform appends its own.
	for _, kv := range body.Env {
		name, _, _ := strings.Cut(kv, "=")
		if err := sandbox.ValidEnvName(name); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	// Choosing an id is moving a sandbox: the control plane's call alone.
	if body.ID != "" && roleOf(r) != "operator" {
		writeErr(w, http.StatusForbidden, "only the control plane may create under a chosen id")
		return
	}
	// A capture is another sandbox's root image: only the control plane's move
	// boots one. Refused as an unknown template, so its existence is not told.
	if roleOf(r) != "operator" && !tenantMayBoot(orgID(r), body.Template, body.Snapshot) {
		writeErr(w, http.StatusBadRequest, "unknown template or snapshot")
		return
	}
	if !operatorPrincipal(r) && (templates.ForeignCustomImage(orgID(r), body.Template) || templates.ForeignCustomImage(orgID(r), body.Image)) {
		writeErr(w, http.StatusBadRequest, "unknown template or image")
		return
	}
	name, err := body.Validate(knownTemplate, sanitizeSandboxName, validateRepo)
	if err != nil {
		writeSandboxErr(w, err)
		return
	}
	// Validate has already refused anything else; "" resolves to "org", which
	// is what every sandbox was before visibility existed.
	visibility, _ := sandbox.ValidVisibility(body.Visibility)
	if err := s.portPublishable(body.Port, body.Template, firstNonEmptyPorts(body.Ports, templatePorts(body.Template))); err != nil {
		writeErrCode(w, http.StatusBadRequest, codePortNotPublished, err.Error())
		return
	}
	// Refuse before boot rather than let a template boot into a runtime that
	// cannot host it: without a kernel of its own, dockerd has no cgroup
	// hierarchy to mount and no capability to build a bridge. See
	// docs/runtimes.md §Boundaries.
	if err := s.nestedDockerSupported(body.Template); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Cheap pre-check so the common duplicate never boots; the claim after
	// boot is the race-safe one.
	if body.Slug != "" {
		if _, taken, err := s.st.SandboxBySlug(r.Context(), body.Slug); err != nil {
			writeErr(w, http.StatusServiceUnavailable, "could not check the slug: "+err.Error())
			return
		} else if taken {
			writeErrCode(w, http.StatusConflict, codeSlugTaken, "slug "+strconv.Quote(body.Slug)+" is taken by another sandbox")
			return
		}
	}
	// The image allowlist is a node-local operator control, not plan policy.
	if body.Image != "" {
		if err := imageAllowed(body.Image, s.imageAllowlist, s.imageRequireDigest); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	org := orgID(r)
	// Claim the Idempotency-Key before booting anything so a retried create
	// cannot produce a second sandbox.
	guard, done := s.beginIdempotent(r.Context(), w, org, r.Header.Get("Idempotency-Key"), http.StatusCreated)
	if done {
		return
	}
	// Releases the key unless finish() commits it, so a failed create is retryable.
	defer guard.release()
	// The plan is read from the ledger, never the request body.
	plan := s.orgPlan(r.Context(), org)
	// Reserve the plan's concurrency slot atomically so two simultaneous creates
	// cannot both pass a stale count. s.maxActive still caps the host globally.
	capRelease, ok := s.d.ReserveConcurrency(org, plan.MaxConcurrent)
	if !ok {
		w.Header().Set("Retry-After", "5")
		writeErr(w, http.StatusTooManyRequests,
			fmt.Sprintf("plan '%s' allows %d concurrent sandboxes — pause one or upgrade", plan.ID, plan.MaxConcurrent))
		return
	}
	defer capRelease()
	if s.maxActive > 0 && len(s.d.List("")) >= s.maxActive {
		w.Header().Set("Retry-After", "5")
		nodeBusy(w)
		writeErr(w, http.StatusTooManyRequests, "sandbox capacity reached")
		return
	}
	// Before the fetch, not only before the boot. The engine refuses a create,
	// a fork, a checkpoint and a snapshot boot on a full filesystem, and all
	// four of those guards sit below this line — while the fetch above them is
	// the largest write the node makes: a named snapshot materialises a memory
	// image, a machine state and a disk out of the artifact store, and then the
	// boot copies all three again. Measured on a full 8 GB filesystem: a create
	// from a 1.6 GB snapshot was refused, but only after the fetch had run the
	// disk out of space, leaving a partial capture behind.
	if s.d.DiskPressure() {
		w.Header().Set("Retry-After", "60")
		nodeBusy(w)
		// Worded like vm.ErrDiskPressure on purpose: one refusal, whichever
		// layer noticed first.
		writeErr(w, http.StatusServiceUnavailable, "server disk pressure, try again later")
		return
	}
	// ResolveFetch, not Resolve: the create path is the only caller allowed to
	// pull a template from the artifact store before boot.
	fetchCtx, cancelFetch := context.WithTimeout(r.Context(), templateFetchWait)
	tmpl, fetch, ferr := templates.ResolveFetchTimed(fetchCtx, body.Template)
	cancelFetch()
	if ferr != nil {
		switch {
		case errors.Is(ferr, templates.ErrFetchInProgress):
			// Not a fault. Answering now — rather than holding the request until
			// the control plane's transport gives up — is the whole point: the
			// caller gets a reason and a retry hint instead of a bare 503 after
			// 30 seconds of silence.
			w.Header().Set("Retry-After", "15")
			writeErrCode(w, http.StatusServiceUnavailable, codeTemplateFetching,
				ferr.Error()+" — this node does not hold it yet; the download is running and a retry will succeed once it lands")
			return
		case !errors.Is(ferr, templates.ErrUnknownTemplate):
			writeErrCode(w, http.StatusServiceUnavailable, codeTemplateFetchFailed, ferr.Error())
			return
		case body.Image != "":
			// An explicit image needs no template; carry on with the zero Template.
		case body.Snapshot != "":
			// A snapshot's capture is not always a rootfs in the image
			// directory: on a container node it is a committed image, held by
			// the runtime's own image tier, which this one cannot see. The
			// runtime resolves it at boot and says so if it cannot.
		default:
			// Answer the 400 here, not in daemon.Create: only a Firecracker node
			// raises ErrTemplateUnknown from a boot, so a typo would boot on docker.
			writeErr(w, http.StatusBadRequest, "unknown template: "+body.Template)
			return
		}
	}
	// A nil override means the template default applies, not allow-all. A read
	// failure is fatal so a database blip cannot widen a sandbox's network.
	orgEgress, _, err := s.orgEgressOverride(r.Context(), org)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "could not read this org's network policy: "+err.Error())
		return
	}
	res, err := sandbox.Resolve(body.Spec, templateDefaults(tmpl), planLimits(plan),
		sandbox.Fleet{PersistentCount: persistentCount(s.d.List(org))}, orgEgress)
	if err != nil {
		writeSandboxErr(w, err)
		return
	}
	res.Name = name

	sb, err := s.d.Create(r.Context(), daemon.CreateSpec{
		Template:         res.TemplateID,
		Name:             res.Name,
		OrgID:            org,
		Timeout:          res.TTL,
		IdleTimeout:      res.IdleTimeout,
		HibernateTimeout: res.HibernateTimeout,
		VCPU:             res.VCPU,
		MemMiB:           res.MemMiB,
		StorageMB:        res.StorageMB,
		// Service templates declare image and ports in the catalogue; the request
		// still wins. The catalogue image skips imageAllowed on purpose, since the
		// allowlist exists for caller-supplied images.
		Image: cmp.Or(body.Image, tmpl.Image),
		Ports: firstNonEmptyPorts(body.Ports, tmpl.Ports),
		Env:   res.Env,
		// The guest learns its own preview root; the daemon appends the id.
		PreviewBase: s.previewRoot(r),
		// Resolved from the template default, the org override and the request,
		// which may only narrow. The runtime refuses what it cannot enforce.
		Network: res.Network,
		// Validated by Spec.Validate; empty is the default and reaches no peer.
		Networks: res.Networks,
		Volumes:  volumeMounts(body),
		// Set by the control plane's create-from-snapshot rewrite, which has
		// already resolved the name to this node's template id and fixed the
		// size; the node never resolves a snapshot name itself.
		Snapshot: body.Snapshot,
		ID:       body.ID,
		// The creator is the caller's key, never anything in the body: a
		// request cannot name someone else as the owner of what it creates.
		// Empty (an agent token, an unsigned hop) leaves the sandbox owned by
		// nobody, which the visibility rule reads as org-visible.
		CreatedBy:  keyIDOf(r),
		Visibility: visibility,
	})
	if err != nil {
		// A policy this node cannot enforce is the caller's to act on: pick a
		// node that can, or create without one. Same code as the domain refusal.
		if errors.Is(err, daemon.ErrEgressDomainsUnsupported) {
			writeErrCode(w, http.StatusNotImplemented, codeEgressUnenforced, err.Error())
			return
		}
		if errors.Is(err, daemon.ErrEgressUnsupported) {
			writeErrCode(w, http.StatusNotImplemented, codeEgressUnenforced, err.Error())
			return
		}
		if errors.Is(err, daemon.ErrTagNetworksUnsupported) {
			writeErrCode(w, http.StatusNotImplemented, codeTagNetworksUnsupported,
				"runtime "+s.d.RuntimeName()+": "+err.Error())
			return
		}
		if errors.Is(err, daemon.ErrBadMoveID) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		if code := volumeStatus(err); code != 0 {
			writeErr(w, code, err.Error())
			return
		}
		// Checks the daemon makes at create, such as a volume's mount path.
		var inv *sandbox.InvalidError
		if errors.As(err, &inv) {
			writeErr(w, http.StatusBadRequest, inv.Reason)
			return
		}
		// A path-traversal-shaped id gets a bare 400, never the template inventory.
		if errors.Is(err, rt.ErrInvalidTemplateName) {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		// A capture taken against another build of this template's root image is a
		// permanent refusal for this node, not a retryable 503: every restore of it
		// here would come back 201 with a guest whose binaries read EIO.
		// Same shape, same reason: a capture needing instructions this CPU does
		// not implement restores here as a 201 and a guest that takes SIGILL.
		if errors.Is(err, rt.ErrRootfsMismatch) || errors.Is(err, rt.ErrCPUTooPoor) {
			nodeBusy(w) // another node's image or CPU may serve it
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		// No rootfs on this node is a 400 naming what is bootable, not a retryable 503.
		if errors.Is(err, rt.ErrTemplateUnknown) {
			msg := "template " + res.TemplateID + " has no image on this node"
			if ids := templates.PublishedIDs(); len(ids) > 0 {
				msg += " — bootable here: " + strings.Join(ids, ", ")
			}
			writeErr(w, http.StatusBadRequest, msg)
			return
		}
		if errors.Is(err, rt.ErrNodeBusy) {
			nodeBusy(w)
		}
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	s.metrics.incCreate()

	if body.Slug != "" {
		if err := s.d.SetSlug(r.Context(), sb.ID, body.Slug); err != nil {
			// Lost the race for the name: the sandbox must not outlive a 409.
			_ = s.d.Destroy(context.WithoutCancel(r.Context()), sb.ID)
			if errors.Is(err, daemon.ErrSlugTaken) {
				writeErrCode(w, http.StatusConflict, codeSlugTaken, "slug "+strconv.Quote(body.Slug)+" is taken by another sandbox")
				return
			}
			writeErr(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		sb.Slug = body.Slug
	}

	// Best-effort attribution (agent vs human); body `client` wins over the header.
	client := body.Client
	if client == "" {
		client = r.Header.Get("X-Pilldrops-Client")
	}
	_ = s.st.SetSandboxAttribution(r.Context(), sb.ID, store.ClassifySource(client), client)

	// Audit the launch config with env values and file contents stripped; the
	// operator surface must never hold customer secrets.
	files := make([]map[string]any, 0, len(body.Files))
	for _, f := range body.Files {
		files = append(files, map[string]any{"path": f.Path, "size": len(f.Content)})
	}
	idleSeconds := sandbox.IdleTimeoutSeconds(res.IdleTimeout)
	hibernateSeconds := sandbox.IdleTimeoutSeconds(res.HibernateTimeout)
	// Record the resolved values, not the unresolved ask.
	s.auditDetail(r, "sandbox.create", sb.ID, map[string]any{
		"template": sb.Template, "image": body.Image,
		"vcpu": res.VCPU, "mem_mb": res.MemMiB, "storage_mb": res.StorageMB,
		"timeout": int64(res.TTL / time.Second), "idle_timeout": idleSeconds,
		"hibernate_timeout": hibernateSeconds,
		"ports":             body.Ports, "port": res.Port,
		"start_cmd": body.Start, "setup": body.Setup,
		"repo": body.Repo, "repo_ref": body.RepoRef,
		"env_keys": envKeys(body.Env), "files": files,
		"client": client, "user_agent": r.UserAgent(),
	})

	resp := map[string]any{
		"id": sb.ID, "template": sb.Template, "name": sb.Name, "status": sb.State,
		"created_at": sb.CreatedAt, "expires_at": expiresOrNil(sb.ExpiresAt),
		"created_by":        sb.CreatedBy,
		"visibility":        sb.Visibility,
		"idle_timeout":      idleSeconds,
		"hibernate_timeout": hibernateSeconds,
		"terminal_url":      s.appURL(r, sb.ID, rt.TerminalPort, org),
		// How this machine came up: "snapshot" means it woke out of a memory
		// image with its processes already running, "cold" that it booted a
		// kernel. Reported on every create, not only a snapshot one, because the
		// difference is what a caller times.
		"boot_type": sb.BootType,
		"boot_ms":   sb.BootMs,
		// Both halves: this engine can host a screen, and this sandbox's image
		// holds the software to serve one. Either alone would mislead.
		"has_desktop": s.hasDesktop(sb),
	}
	// Only where one actually comes up. It used to be emitted unconditionally,
	// on the reasoning that 6080 is routable on every sandbox — true, and it
	// answers "App is not responding on port 6080" on all but a handful, while
	// being the first link a client picks up. A link that cannot open is worse
	// than no link.
	if s.hasDesktop(sb) {
		resp["desktop_url"] = s.appURL(r, sb.ID, rt.DesktopPort, org)
	}
	// Legal, applied, and probably not what the author meant — the loudest being
	// "the allow list is empty, so this policy denies all egress". Resolve
	// computes these and nothing used to read them, so a deny-only rule set came
	// back 201 and the sandbox had no network at all with no word said.
	if len(res.NetworkAdvisories) > 0 {
		resp["network_advisories"] = res.NetworkAdvisories
	}
	if body.Snapshot != "" {
		// Disk or memory, said plainly: a Docker snapshot is a committed
		// filesystem and the machine it stamps out boots cold.
		resp["snapshot"] = body.Snapshot
		resp["snapshot_kind"] = "memory"
		if sb.BootType != "snapshot" {
			resp["snapshot_kind"] = "disk"
		}
	}
	if sb.Slug != "" {
		resp["slug"] = sb.Slug
	}
	if fetch.Fetched {
		// Absent on a warm create, so its presence is the signal.
		resp["fetch_ms"] = fetch.Duration.Milliseconds()
		slog.Info("create fetched its template from the artifact store",
			"component", "artifact", "sandbox_id", sb.ID, "template", sb.Template,
			"fetch_ms", fetch.Duration.Milliseconds())
	}

	// Templates with a built-in server publish their port here; deploys with a
	// start command publish via StartServer below.
	if body.Start == "" {
		port := body.Port
		if port <= 0 {
			port = tmpl.Port
		}
		if port > 0 {
			if err := s.d.SetPublicPort(r.Context(), sb.ID, port); err == nil {
				resp["url"] = s.appURL(r, sb.ID, port, org)
			}
		}
	}

	// The clone runs first as an ordinary setup step so it lands in the deploy log.
	setup := body.Setup
	var repoDir string
	if body.Repo != "" {
		var cloneCmd string
		cloneCmd, repoDir = buildCloneCmd(body.Repo, body.RepoRef, envHasKey(body.Env, "GIT_TOKEN"))
		setup = append([]string{cloneCmd}, setup...)
	}

	if len(body.Files) > 0 || len(setup) > 0 || body.Start != "" {
		fs, ferr := s.d.FS(sb.ID)
		if ferr != nil {
			writeErr(w, http.StatusServiceUnavailable, ferr.Error())
			return
		}
		port := body.Port
		if port == 0 {
			port = 8000
		}
		if body.AsyncSetup {
			// The deploy must outlive this request.
			s.deploys.start(sb.ID)
			ctx := context.WithoutCancel(r.Context())
			go s.runDeploy(ctx, sb.ID, fs, body.Files, setup, body.Start, port)
			resp["deploy_state"] = string(DeployRunning)
			if body.Start != "" {
				resp["app_url"] = s.appURL(r, sb.ID, port, org)
			}
		} else {
			state, deployLog := s.deploySteps(r.Context(), sb.ID, fs, body.Files, setup, body.Start, port, nil)
			if body.Start != "" && state != DeployFailed {
				resp["app_url"] = s.appURL(r, sb.ID, port, org)
			}
			resp["deploy_log"] = deployLog
			resp["deploy_state"] = string(state)
		}
		if repoDir != "" {
			resp["repo_dir"] = repoDir
		}
	}

	guard.finish(resp)
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	all := s.d.List(orgID(r))
	keyID, operator := keyIDOf(r), operatorPrincipal(r)
	visible := make([]daemon.SandboxInfo, 0, len(all))
	for _, sb := range all {
		if sandbox.Visible(sb.ID, sb.Visibility, sb.CreatedBy, keyID, operator) {
			visible = append(visible, sb)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": s.decorate(visible)})
}

// hasDesktop is the per-sandbox answer clients act on: the runtime can host an
// X server, this sandbox's image holds the screen stack, AND this sandbox asked
// for it. Reporting a subset would enable a Desktop button that opens nothing or
// grey one that would have worked.
//
// The third clause is what was missing. `base` holds the stack and every runtime
// here can host a screen, so base reported has_desktop=true — but nothing sets
// PD_DESKTOP=1 on it, envd starts no screen, and the 6080 link `sandbox open`
// handed out led to a port nothing was ever listening on. See DesktopBaked for
// what this cannot see and why that is the right way round.
func (s *Server) hasDesktop(sb daemon.SandboxInfo) bool {
	caps, ok := s.d.RuntimeCaps()
	return ok && caps.Desktop && templates.HasDesktopStack(sb.Template) &&
		templates.DesktopBaked(sb.Template)
}

// decorate renders sandboxes for the list response with the fields derived from
// the node rather than stored on the sandbox.
// ponytail: a marshal round trip per row keeps SandboxInfo free of node-derived
// fields; give it a real response type if a list of thousands ever shows up hot.
func (s *Server) decorate(list []daemon.SandboxInfo) []map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, sb := range list {
		m := map[string]any{}
		if raw, err := json.Marshal(sb); err == nil {
			_ = json.Unmarshal(raw, &m)
		}
		m["has_desktop"] = s.hasDesktop(sb)
		out = append(out, m)
	}
	return out
}

// deploySteps runs the files/setup/start work for both the synchronous and
// background paths. track is nil on the synchronous path.
func (s *Server) deploySteps(
	ctx context.Context,
	id string,
	fs rt.FileSystem,
	files []sandbox.File,
	setup []string,
	start string,
	port int,
	track func(DeployStep, string),
) (DeployState, []string) {
	var log []string
	state := DeployDone
	note := func(step DeployStep, line string) {
		log = append(log, line)
		if track != nil {
			track(step, line)
		}
	}

	for _, f := range files {
		if err := fs.Write(ctx, f.Path, []byte(f.Content)); err != nil {
			state = DeployFailed
			note(DeployStep{Cmd: "write " + f.Path, Exit: -1, Err: err.Error()}, "write "+f.Path+": "+err.Error())
		} else {
			note(DeployStep{Cmd: "write " + f.Path}, "wrote "+f.Path)
		}
	}

	for _, cmd := range setup {
		t0 := time.Now()
		res, err := s.d.Exec(ctx, id, rt.ExecSpec{Cmd: cmd, Timeout: 120 * time.Second})
		ms := time.Since(t0).Milliseconds()
		switch {
		case err != nil:
			state = DeployFailed
			note(DeployStep{Cmd: cmd, Exit: -1, Err: err.Error(), MS: ms}, "setup '"+cmd+"': "+err.Error())
		case res.ExitCode != 0:
			state = DeployFailed
			note(DeployStep{Cmd: cmd, Exit: res.ExitCode, MS: ms}, fmt.Sprintf("setup '%s' exit=%d", cmd, res.ExitCode))
		default:
			note(DeployStep{Cmd: cmd, MS: ms}, "setup '"+cmd+"' ok")
		}
	}

	if start != "" {
		t0 := time.Now()
		if err := s.d.StartServer(ctx, id, start, port, nil); err != nil {
			state = DeployFailed
			note(DeployStep{Cmd: start, Exit: -1, Err: err.Error(), MS: time.Since(t0).Milliseconds()}, "start: "+err.Error())
		} else {
			note(DeployStep{Cmd: start, MS: time.Since(t0).Milliseconds()}, "started: "+start)
		}
	}
	return state, log
}

// runDeploy is deploySteps in the background, recording progress as it goes.
func (s *Server) runDeploy(
	ctx context.Context,
	id string,
	fs rt.FileSystem,
	files []sandbox.File,
	setup []string,
	start string,
	port int,
) {
	state, _ := s.deploySteps(ctx, id, fs, files, setup, start, port, func(step DeployStep, line string) {
		s.deploys.step(id, step, line)
	})
	s.deploys.finish(id, state)
}

// handleDeployStatus reports a background deploy; an unknown id is "unknown", not "done".
func (s *Server) handleDeployStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	p, known := s.deploys.get(id)
	if !known {
		writeJSON(w, http.StatusOK, map[string]any{"state": "unknown", "steps": []DeployStep{}, "log": []string{}})
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sb, ok := s.d.Get(id)
	if !ok || !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	previewPort := sb.PublicPort
	if previewPort == 0 {
		previewPort = 8000
	}
	resp := map[string]any{
		"id": sb.ID, "template": sb.Template, "name": sb.Name, "status": sb.State,
		"created_at": sb.CreatedAt, "expires_at": expiresOrNil(sb.ExpiresAt),
		"created_by":        sb.CreatedBy,
		"visibility":        sb.Visibility,
		"idle_timeout":      sandbox.IdleTimeoutSeconds(sb.IdleTimeout),
		"hibernate_timeout": sandbox.IdleTimeoutSeconds(sb.HibernateTimeout),
		"url":               s.appURL(r, sb.ID, previewPort, sb.OrgID),
		"terminal_url":      s.appURL(r, sb.ID, rt.TerminalPort, sb.OrgID),
		"has_desktop":       s.hasDesktop(sb),
		"boot_type":         sb.BootType,
		"boot_ms":           sb.BootMs,
	}
	if s.hasDesktop(sb) {
		resp["desktop_url"] = s.appURL(r, sb.ID, rt.DesktopPort, sb.OrgID)
	}
	if sb.Slug != "" {
		resp["slug"] = sb.Slug
	}
	// Both omitted when the sandbox joined no network: absent is the default
	// and reads as "reaches no peer", which an empty list does not.
	if len(sb.Networks) > 0 {
		resp["networks"] = sb.Networks
		// The node is part of the answer: reach is per node, so a peer with a
		// shared tag elsewhere is neither resolvable nor reachable.
		resp["node"] = s.nodeID
		if sb.PrivateIP != "" {
			resp["private_ip"] = sb.PrivateIP
		}
	}
	// Storage is omitted, not zeroed, when the runtime cannot measure it.
	if sb.StorageMB > 0 {
		resp["storage_mb"] = sb.StorageMB
	}
	if used, ok := s.d.StorageUsedMB(r.Context(), sb.ID); ok {
		resp["storage_used_mb"] = used
	}
	writeJSON(w, http.StatusOK, resp)
}

const (
	// defaultForegroundExecTimeout applies to a synchronous exec that sends no timeout.
	defaultForegroundExecTimeout = 120 * time.Second
	// maxForegroundExecTimeout caps a foreground exec; longer work should use Detach.
	// It is httpx.NodeSyncCeiling because the control plane's node transport
	// sizes its first-byte bound from the same constant: a ceiling the hop
	// cannot carry is a command that completes here and 502s there.
	maxForegroundExecTimeout = httpx.NodeSyncCeiling
)

// resolveExecTimeout picks the exec deadline; detached execs pass theirs through untouched.
func resolveExecTimeout(reqSeconds int, detach bool) time.Duration {
	t := time.Duration(reqSeconds) * time.Second
	if detach {
		return t
	}
	if reqSeconds <= 0 {
		return defaultForegroundExecTimeout
	}
	if t > maxForegroundExecTimeout {
		return maxForegroundExecTimeout
	}
	return t
}

// maxExecStdinBytes bounds one exec's stdin; it travels whole in one message.
// ponytail: whole stdin in one message; a bidi stream if someone pipes more.
const maxExecStdinBytes = 64 << 20

// decodeExecStdin turns the request's base64 stdin into bytes, answering 400
// on bad encoding and 413 over maxExecStdinBytes.
func decodeExecStdin(w http.ResponseWriter, b64 string) ([]byte, bool) {
	if b64 == "" {
		return nil, true
	}
	if base64.StdEncoding.DecodedLen(len(b64)) > maxExecStdinBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "stdin exceeds the 64 MiB exec stdin limit")
		return nil, false
	}
	stdin, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "stdin must be base64")
		return nil, false
	}
	if len(stdin) > maxExecStdinBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "stdin exceeds the 64 MiB exec stdin limit")
		return nil, false
	}
	return stdin, true
}

// execRaw answers a raw: true stream: stdout bytes as they arrive, stderr
// dropped, exit code in the X-Exit-Code trailer. ponytail: stdout-only; an
// interleaved stderr needs a framed encoding, add when a consumer asks.
func (s *Server) execRaw(w http.ResponseWriter, r *http.Request, id string, spec rt.ExecSpec, flusher http.Flusher) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Trailer", "X-Exit-Code")
	w.WriteHeader(http.StatusOK)
	exitCode := -1
	err := s.d.ExecStream(r.Context(), id, spec, func(f rt.StreamFrame) {
		if len(f.Stdout) > 0 {
			_, _ = w.Write(f.Stdout)
			flusher.Flush()
		}
		if f.Exited {
			exitCode = f.ExitCode
		}
	})
	if err != nil {
		s.metrics.incExecError()
		exitCode = -1
	} else {
		s.metrics.incExec()
	}
	w.Header().Set("X-Exit-Code", strconv.Itoa(exitCode))
	s.auditDetail(r, "sandbox.exec_raw", id, map[string]any{"cmd": spec.Cmd, "exit_code": exitCode})
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	var body struct {
		Cmd     string            `json:"cmd"`
		Cwd     string            `json:"cwd"`
		Timeout int               `json:"timeout"`
		Env     map[string]string `json:"env"`
		Detach  bool              `json:"detach"`
		Stdin   string            `json:"stdin"` // base64, delivered whole; at most maxExecStdinBytes
	}
	if !decodeBody(r, &body) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Cmd == "" {
		writeErr(w, http.StatusBadRequest, "cmd required")
		return
	}
	stdin, ok := decodeExecStdin(w, body.Stdin)
	if !ok {
		return
	}
	started := time.Now()
	res, err := s.d.Exec(r.Context(), id, rt.ExecSpec{
		Cmd: body.Cmd, Cwd: body.Cwd, Timeout: resolveExecTimeout(body.Timeout, body.Detach),
		Env: mergeExecEnv(s.configEnvFor(r.Context(), id), body.Env), Detach: body.Detach,
		Stdin: stdin,
	})
	if err != nil {
		s.metrics.incExecError()
		writeErr(w, statusForErr(err), err.Error())
		return
	}
	s.metrics.incExec()
	s.auditDetail(r, "sandbox.exec", id, map[string]any{
		"cmd": body.Cmd, "exit_code": res.ExitCode,
		"duration_ms": time.Since(started).Milliseconds(),
	})
	if body.Detach {
		writeJSON(w, http.StatusOK, map[string]any{"pid": res.Pid, "detached": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stdout": res.Stdout, "stderr": res.Stderr,
		"exit_code": res.ExitCode, "truncated": res.Truncated,
	})
}

// handleExecStream runs a command and streams stdout/stderr as Server-Sent
// Events, one JSON object per frame:
//
//	{"type":"stdout","data":"..."}   {"type":"stderr","data":"..."}
//	{"type":"exit","code":0,"truncated":false}   {"type":"error","error":"..."}
//
// Only Firecracker streams live; other runtimes send a single buffered frame.
func (s *Server) handleExecStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	var body struct {
		Cmd     string            `json:"cmd"`
		Cwd     string            `json:"cwd"`
		Timeout int               `json:"timeout"`
		Env     map[string]string `json:"env"`
		Stdin   string            `json:"stdin"` // base64, at most maxExecStdinBytes
		// Raw lifts the guest's 10 MiB output cap and answers with stdout
		// bytes only (application/octet-stream, exit code in X-Exit-Code).
		Raw bool `json:"raw"`
	}
	if !decodeBody(r, &body) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Cmd == "" {
		writeErr(w, http.StatusBadRequest, "cmd required")
		return
	}
	stdin, ok := decodeExecStdin(w, body.Stdin)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	if body.Raw {
		s.execRaw(w, r, id, rt.ExecSpec{Cmd: body.Cmd, Cwd: body.Cwd,
			Env:     mergeExecEnv(s.configEnvFor(r.Context(), id), body.Env),
			Timeout: resolveExecTimeout(body.Timeout, false), Stdin: stdin, RawOutput: true}, flusher)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	// Stored config env sits under the request's own env.
	spec := rt.ExecSpec{Cmd: body.Cmd, Cwd: body.Cwd,
		Env:     mergeExecEnv(s.configEnvFor(r.Context(), id), body.Env),
		Timeout: resolveExecTimeout(body.Timeout, false), Stdin: stdin}
	started := time.Now()
	exitCode := -1
	truncated := false
	err := s.d.ExecStream(r.Context(), id, spec, func(f rt.StreamFrame) {
		truncated = truncated || f.Truncated
		if len(f.Stdout) > 0 {
			send(map[string]any{"type": "stdout", "data": string(f.Stdout)})
		}
		if len(f.Stderr) > 0 {
			send(map[string]any{"type": "stderr", "data": string(f.Stderr)})
		}
		if f.Exited {
			exitCode = f.ExitCode
			// The guest reports truncation in its own frame before the exit
			// frame, so carry it forward or the exit always says false.
			send(map[string]any{"type": "exit", "code": f.ExitCode, "truncated": f.Truncated || truncated})
		}
	})
	if err != nil {
		s.metrics.incExecError()
		send(map[string]any{"type": "error", "error": err.Error()})
		return
	}
	s.metrics.incExec()
	s.auditDetail(r, "sandbox.exec_stream", id, map[string]any{
		"cmd": body.Cmd, "exit_code": exitCode,
		"duration_ms": time.Since(started).Milliseconds(),
	})
}

// handleStart launches a long-lived server on an existing sandbox. Unlike exec,
// the command is recorded as StartCmd and replayed on resume/restart.
func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	var body struct {
		Cmd  string            `json:"cmd"`
		Port int               `json:"port"`
		Env  map[string]string `json:"env"`
	}
	if !decodeBody(r, &body) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Cmd == "" {
		writeErr(w, http.StatusBadRequest, "cmd required")
		return
	}
	port := body.Port
	if port == 0 {
		port = 8000
	}
	if err := s.portPublishable(port, s.templateOf(id), nil); err != nil {
		writeErrCode(w, http.StatusBadRequest, codePortNotPublished, err.Error())
		return
	}
	if err := s.d.StartServer(r.Context(), id, body.Cmd, port, body.Env); err != nil {
		writeErr(w, statusForErr(err), err.Error())
		return
	}
	s.auditDetail(r, "sandbox.start", id, map[string]any{
		"cmd": body.Cmd, "port": port, "env_keys": sortedKeys(body.Env),
	})
	writeJSON(w, http.StatusOK, map[string]any{"started": body.Cmd, "port": port})
}

// maxSandboxNameLen caps a sandbox's human label.
const maxSandboxNameLen = 64

// sanitizeSandboxName trims whitespace and strips control characters. A name
// too long after stripping is rejected, never truncated.
func sanitizeSandboxName(raw string) (string, error) {
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	name := strings.TrimSpace(b.String())
	if utf8.RuneCountInString(name) > maxSandboxNameLen {
		return "", fmt.Errorf("name exceeds %d characters", maxSandboxNameLen)
	}
	return name, nil
}

// handleRename is PATCH /v1/sandboxes/{id}: sets or clears the human label
// and/or the slug, moves the published app port, and/or changes the idle
// auto-pause (same policy as create). Absent fields are left alone, so a
// {port} PATCH does not clear the name. Returns the updated sandbox.
func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	var body struct {
		Name             *string   `json:"name"`
		Slug             *string   `json:"slug"`
		Port             *int      `json:"port"`
		IdleTimeout      *int      `json:"idle_timeout"`
		HibernateTimeout *int      `json:"hibernate_timeout"`
		Networks         *[]string `json:"networks"`
	}
	if !decodeBody(r, &body) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Name == nil && body.Slug == nil && body.Port == nil &&
		body.IdleTimeout == nil && body.HibernateTimeout == nil && body.Networks == nil {
		writeErr(w, http.StatusBadRequest,
			"body must set at least one of name, slug, port, idle_timeout, hibernate_timeout, networks")
		return
	}
	audit := map[string]any{}
	if body.Slug != nil {
		if err := sandbox.ValidSlug(*body.Slug); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if body.Networks != nil {
		if err := sandbox.ValidNetworks(*body.Networks); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if body.Port != nil {
		if *body.Port < 1 || *body.Port > 65535 {
			writeErr(w, http.StatusBadRequest, "port must be 1-65535")
			return
		}
		if err := s.portPublishable(*body.Port, s.templateOf(id), nil); err != nil {
			writeErrCode(w, http.StatusBadRequest, codePortNotPublished, err.Error())
			return
		}
	}
	if body.Name != nil {
		name, err := sanitizeSandboxName(*body.Name)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.d.Rename(r.Context(), id, name); err != nil {
			writeErr(w, statusForErr(err), err.Error())
			return
		}
		audit["name"] = name
	}
	if body.Slug != nil {
		if err := s.d.SetSlug(r.Context(), id, *body.Slug); err != nil {
			if errors.Is(err, daemon.ErrSlugTaken) {
				writeErrCode(w, http.StatusConflict, codeSlugTaken, "slug "+strconv.Quote(*body.Slug)+" is taken by another sandbox")
				return
			}
			writeErr(w, statusForErr(err), err.Error())
			return
		}
		audit["slug"] = *body.Slug
	}
	if body.Port != nil {
		if err := s.d.SetPublicPort(r.Context(), id, *body.Port); err != nil {
			writeErr(w, statusForErr(err), err.Error())
			return
		}
		audit["port"] = *body.Port
	}
	// After the slug: peers resolve a sandbox by its slug, so a PATCH that sets
	// both must publish the new name, not the old one.
	if body.Networks != nil {
		if err := s.d.SetNetworks(r.Context(), id, *body.Networks); err != nil {
			if errors.Is(err, daemon.ErrTagNetworksUnsupported) {
				writeErrCode(w, http.StatusNotImplemented, codeTagNetworksUnsupported,
					"runtime "+s.d.RuntimeName()+": "+err.Error())
				return
			}
			writeErr(w, statusForErr(err), err.Error())
			return
		}
		audit["networks"] = *body.Networks
	}
	if body.IdleTimeout != nil {
		idle := sandbox.IdleTimeout(*body.IdleTimeout)
		if err := s.d.SetIdleTimeout(r.Context(), id, idle); err != nil {
			writeErr(w, statusForErr(err), err.Error())
			return
		}
		audit["idle_timeout"] = sandbox.IdleTimeoutSeconds(idle)
		audit["requested_idle_timeout"] = *body.IdleTimeout
	}
	if body.HibernateTimeout != nil {
		hib := sandbox.HibernateTimeout(*body.HibernateTimeout)
		if err := s.d.SetHibernateTimeout(r.Context(), id, hib); err != nil {
			writeErr(w, statusForErr(err), err.Error())
			return
		}
		audit["hibernate_timeout"] = sandbox.IdleTimeoutSeconds(hib)
		audit["requested_hibernate_timeout"] = *body.HibernateTimeout
	}
	s.auditDetail(r, "sandbox.rename", id, audit)
	sb, _ := s.d.Get(id)
	resp := map[string]any{
		"id": sb.ID, "template": sb.Template, "name": sb.Name, "status": sb.State,
		"created_at": sb.CreatedAt, "expires_at": sb.ExpiresAt,
		"idle_timeout":      sandbox.IdleTimeoutSeconds(sb.IdleTimeout),
		"hibernate_timeout": sandbox.IdleTimeoutSeconds(sb.HibernateTimeout),
	}
	if sb.Slug != "" {
		resp["slug"] = sb.Slug
	}
	if len(sb.Networks) > 0 {
		resp["networks"] = sb.Networks
		resp["node"] = s.nodeID
		if sb.PrivateIP != "" {
			resp["private_ip"] = sb.PrivateIP
		}
	}
	if sb.PublicPort > 0 {
		resp["url"] = s.appURL(r, sb.ID, sb.PublicPort, sb.OrgID)
	}
	writeJSON(w, http.StatusOK, resp)
}

// templateOf is the template a live sandbox was created from, or "".
func (s *Server) templateOf(id string) string {
	sb, _ := s.d.Get(id)
	return sb.Template
}

// templatePorts is a catalogue template's declared ports; nil for unknown ids.
func templatePorts(template string) []int {
	if t, ok := templates.Resolve(template); ok {
		return t.Ports
	}
	return nil
}

// portPublishable refuses a preview port a single-port runtime cannot serve:
// on docker the container publishes 8000 and the declared ports only, so any
// other value would 502 after the fact. 0 and 8000 always pass. ponytail:
// refusing beats a socat relay; add the relay if a customer needs it.
func (s *Server) portPublishable(port int, template string, declared []int) error {
	caps, ok := s.d.RuntimeCaps()
	if port == 0 || port == 8000 || !ok || caps.MultiPort {
		return nil
	}
	// The process runtime (dev only) serves its one app port under whatever
	// number the caller names; only a container has a fixed publish list.
	if s.d.RuntimeName() == "process" {
		return nil
	}
	if slices.Contains(daemon.DeclaredPorts(template, declared), port) {
		return nil
	}
	return fmt.Errorf("port %d is not published: %s publishes 8000 and the template's declared ports only",
		port, s.d.RuntimeName())
}

// nestedDockerSupported refuses a template that runs a container daemon of its
// own on a runtime whose sandboxes share the host kernel. Keyed on the resolved
// catalogue id so an alias cannot walk past it. Not a plan or config question:
// no setting turns it on (docs/runtimes.md §Boundaries).
func (s *Server) nestedDockerSupported(template string) error {
	t, ok := templates.Resolve(template)
	if !ok || t.ID != "docker" {
		return nil
	}
	if caps, ok := s.d.RuntimeCaps(); ok && caps.NestedDocker {
		return nil
	}
	return fmt.Errorf("template docker requires the firecracker runtime; this node runs %s, "+
		"whose sandboxes share the host kernel and cannot run a container daemon", s.d.RuntimeName())
}

func (s *Server) handleDestroy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	org := orgID(r)
	// Must run before owns(): a retried destroy targets a sandbox the first call
	// already removed, so owns() would 404 the retry.
	guard, done := s.beginIdempotent(r.Context(), w, org, scopedIdemKey("destroy", id, r.Header.Get("Idempotency-Key")), http.StatusOK)
	if done {
		return
	}
	defer guard.release()
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	// ?evict=1 is the control plane moving the sandbox: it runs elsewhere now
	// under this id, so only this node's copy goes.
	if r.URL.Query().Get("evict") == "1" {
		if roleOf(r) != "operator" {
			writeErr(w, http.StatusForbidden, "only the control plane may evict a sandbox")
			return
		}
		if err := s.d.Evict(r.Context(), id); err != nil {
			writeErr(w, statusForErr(err), err.Error())
			return
		}
		s.auditDetail(r, "sandbox.evict", id, map[string]any{"reason": "moved"})
		resp := map[string]string{"status": "evicted"}
		guard.finish(resp)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if err := s.d.Destroy(r.Context(), id); err != nil {
		writeErr(w, statusForErr(err), err.Error())
		return
	}
	s.metrics.incDestroy()
	// Per-sandbox row cleanup lives in daemon.StoreAdapter.DeleteSandbox, since
	// the reaper and abandonment sweep never reach this handler.
	s.auditDetail(r, "sandbox.destroy", id, map[string]any{"reason": "user"})
	resp := map[string]string{"status": "destroyed"}
	guard.finish(resp)
	writeJSON(w, http.StatusOK, resp)
}

// handleAudit lists recent audit entries for the caller's org (newest first).
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	limit := httpx.QueryInt(r, "limit", 0)
	entries, err := s.st.ListAudit(r.Context(), orgID(r), limit)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if entries == nil {
		entries = []store.AuditEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// owns gates every sandbox-scoped route: the org still has to match, and
// below the org a private sandbox is its creator's and any operator's. Judged
// from the record, never from the runtime, so a paused sandbox answers the
// same as a running one. Callers answer 404, not 403: to a member who may not
// see it, a private sandbox does not exist.
func (s *Server) owns(r *http.Request, id string) bool {
	sb, ok := s.d.Get(id)
	return ok && sb.OrgID == orgID(r) &&
		sandbox.Visible(sb.ID, sb.Visibility, sb.CreatedBy, keyIDOf(r), operatorPrincipal(r))
}

func statusForErr(err error) int {
	switch {
	case errors.Is(err, rt.ErrNotFound), errors.Is(err, rt.ErrPathNotFound):
		return http.StatusNotFound
	case errors.Is(err, rt.ErrPaused):
		return http.StatusConflict
	case errors.Is(err, rt.ErrProtectedProcess):
		return http.StatusBadRequest
	case errors.Is(err, rt.ErrUnresumable):
		return http.StatusGone
	case volumeStatus(err) != 0:
		return volumeStatus(err)
	default:
		return http.StatusServiceUnavailable
	}
}

// knownTemplate reports whether a create may proceed past validation. Resolve
// never consults the artifact store, so a node with a store tier defers to
// ResolveFetch, which still answers 400 for an id no tier has.
func knownTemplate(id string) bool {
	if _, ok := templates.Resolve(id); ok {
		return true
	}
	return templates.HasSource()
}

// validateRepo checks the git URL and ref before anything is booted.
func validateRepo(repoURL, ref string) error {
	if !validRepoURL(repoURL) {
		return errBadRepo
	}
	if ref != "" && !repoRefRe.MatchString(ref) {
		return errors.New("repo_ref must be a plain branch, tag or commit")
	}
	return nil
}

// orgEgressOverride reads the org's egress override. A storeless node has none,
// which is nil/false rather than an error.
func (s *Server) orgEgressOverride(ctx context.Context, org string) (*egress.Rules, bool, error) {
	if s.st == nil {
		return nil, false, nil
	}
	return s.st.OrgEgress(ctx, org)
}

// templateDefaults adapts a catalogue entry to what the policy needs.
func templateDefaults(t templates.Template) sandbox.TemplateDefaults {
	return sandbox.TemplateDefaults{
		ID: t.ID, CPU: t.CPU, MemoryMB: t.MemoryMB, StorageMB: t.StorageMB, Port: t.Port,
		Network: templateEgress(t),
	}
}

// templateEgress is a template's default egress policy. Nothing today: a
// service template exists to be reached rather than to reach out, so deny-all
// is the right default for one, but imposing it here would fail every service
// create on a node whose runtime cannot enforce a policy — which is every
// docker node. Flipping a fleet default is its own change, with a week of deny
// records behind it. Until then a
// policy arrives only because a caller or an org override asked for one.
func templateEgress(templates.Template) *egress.Rules { return nil }

// planLimits adapts a billing plan to the ceiling the policy applies.
func planLimits(p entitlements.Plan) sandbox.Limits {
	return sandbox.Limits{
		ID: p.ID, MaxVCPU: p.MaxVCPU, MaxMemMB: p.MaxMemMB, MaxStorageMB: p.MaxStorageMB,
		MaxConcurrent: p.MaxConcurrent, MaxWorkstations: p.MaxWorkstations,
		SessionTTLHrs: p.SessionTTLHrs, Persistent: p.Persistent,
	}
}

// writeSandboxErr maps a policy error to its HTTP status: InvalidError is 400,
// LimitError is 429 with its Retry-After.
func writeSandboxErr(w http.ResponseWriter, err error) {
	var inv *sandbox.InvalidError
	if errors.As(err, &inv) {
		writeErr(w, http.StatusBadRequest, inv.Reason)
		return
	}
	var lim *sandbox.LimitError
	if errors.As(err, &lim) {
		if lim.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(lim.RetryAfter))
		}
		writeErr(w, http.StatusTooManyRequests, lim.Reason)
		return
	}
	writeErr(w, http.StatusInternalServerError, err.Error())
}

// firstNonEmptyPorts returns req if set, else tmpl. Request ports replace the
// template's rather than merge, so a deliberately omitted port stays unpublished.
func firstNonEmptyPorts(req, tmpl []int) []int {
	if len(req) > 0 {
		return req
	}
	return tmpl
}

// nodeBusy marks a create refusal as this node's, not the request's: the
// control plane then places the create on another node rather than failing it.
// The orchestrator reads the same literal header.
func nodeBusy(w http.ResponseWriter) { w.Header().Set("X-Node-Busy", "1") }
