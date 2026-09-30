// Package httpapi is sandboxd's PillDrops-shaped HTTP layer over the daemon.
package httpapi

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/authcache"
	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	"github.com/pilldrops-oss/pilldrops/internal/proxy"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// Server bundles the daemon and store behind an HTTP API.
type Server struct {
	authc *authcache.Cache
	// agentFeature is build-tagged so the core build carries an empty struct.
	agentFeature
	deploys        *deployTracker
	d              *daemon.Daemon
	st             *store.Store
	templates      []string
	metrics        *metrics
	limiter        *rateLimiter
	previewLimiter *rateLimiter // per-IP throttle for the public /preview proxy
	// authFailLimiter throttles failed auth attempts per source IP. Inert unless
	// AUTH_FAIL_RATE_RPS is set.
	authFailLimiter *rateLimiter
	maxActive       int
	dataDir         string
	orchURL         string // orchestrator base URL when this node is clustered
	orchKey         string // API key to read the orchestrator's fleet
	// allowDirectTenantAPI re-opens the customer API to non-operator keys on a
	// clustered node. Off by default.
	allowDirectTenantAPI bool
	// consoleKey signs the dashboard's account sessions (webui) and is itself the
	// API key the dashboard acts with; empty when the dashboard uses the key form.
	consoleKey string
	// orgOverrideSecret verifies the signed X-Org-Override assertion. Unset means
	// the override is refused unless devInsecure().
	orgOverrideSecret []byte
	// orgAssertPub verifies X-Org-Override asymmetrically and replaces
	// orgOverrideSecret when set.
	orgAssertPub ed25519.PublicKey
	// nodeID is folded into the org assertion signature so an assertion minted
	// for a sibling node does not verify here. Empty on a standalone node.
	nodeID string
	// assertNonces refuses a second presentation of the same org assertion.
	assertNonces *nonceCache
	// previewSecret is the HMAC secret for ?ptok= on /preview/. Unset fails
	// closed except under devInsecure().
	previewSecret []byte
	rateRPS       int
	rateBurst     int
	maxBodyBytes  int64 // request-body cap; MAX_REQUEST_BYTES, default 64 MiB
	// filesMaxBytes is the per-file cap on /files uploads, which are exempt
	// from maxBodyBytes so a large file is bounded by a limit that names itself.
	filesMaxBytes int64 // PD_FILES_MAX_BYTES, default 1 GiB
	// functionFeature is build-tagged like agentFeature.
	functionFeature
	// imageAllowlist restricts body.Image to these registry prefixes when
	// non-empty (PILLDROPS_IMAGE_ALLOWLIST). Empty allows any registry.
	imageAllowlist []string
	// imageRequireDigest rejects non-digest-pinned body.Image refs.
	imageRequireDigest bool
	sandboxDomain      string
	// isolationOK is false only after an enforce-mode preflight failure; then
	// create and resume refuse new work even when reached directly.
	isolationOK atomic.Bool
	// probePreview is a field so tests can substitute a canned upstream.
	probePreview func(ctx context.Context, target string) (int, error)
	// localTemplates reports the template ids held locally; nil means this node
	// does not enumerate.
	localTemplates func() []string
}

// New builds a Server. tmpl overrides the default template list; empty means
// templates.Catalog(). Limits come from env, see limits.go.
func New(d *daemon.Daemon, st *store.Store, tmpl ...string) *Server {
	// tmpl only orders the default list; validation consults the dynamic registry.
	ids := tmpl
	if len(ids) == 0 {
		for _, t := range templates.Catalog() {
			ids = append(ids, t.ID)
		}
	}
	rps := envInt("RATE_LIMIT_RPS", defaultRateRPS)
	burst := envInt("RATE_LIMIT_BURST", defaultRateBurst)
	s := &Server{
		deploys: newDeployTracker(),
		d:       d, st: st, templates: ids,
		authc:                authcache.New(),
		metrics:              newMetrics(),
		agentFeature:         newAgentFeature(),
		limiter:              newRateLimiter(float64(rps), burst),
		previewLimiter:       newRateLimiter(float64(envInt("PREVIEW_RATE_RPS", 50)), envInt("PREVIEW_RATE_BURST", 100)),
		authFailLimiter:      newRateLimiter(float64(envInt("AUTH_FAIL_RATE_RPS", 0)), envInt("AUTH_FAIL_BURST", 10)),
		maxActive:            envInt("MAX_ACTIVE_SANDBOXES", defaultMaxActive),
		dataDir:              "/",
		rateRPS:              rps,
		rateBurst:            burst,
		maxBodyBytes:         int64(envInt("MAX_REQUEST_BYTES", 64<<20)),
		filesMaxBytes:        int64(envInt("PD_FILES_MAX_BYTES", 1<<30)),
		allowDirectTenantAPI: envBool("SANDBOXD_ALLOW_DIRECT_TENANT_API", false),
		imageAllowlist:       splitCSV(envStr("PILLDROPS_IMAGE_ALLOWLIST", "")),
		imageRequireDigest:   envBool("PILLDROPS_IMAGE_REQUIRE_DIGEST", false),
		probePreview:         probePreviewHTTP,
		assertNonces:         newNonceCache(),
	}
	s.isolationOK.Store(true)
	s.logImagePolicy()
	// The preview throttle is always on; the auth-failure one only when set.
	// Both key on the client address, so both are wrong behind a terminator
	// whose X-Forwarded-For this node is not told to trust.
	httpx.WarnIfUntrustedProxy("sandboxd", true)
	return s
}

// logImagePolicy logs the image policy at startup, as loudly when the controls
// are off as when they are on, so a misspelled env var is visible.
func (s *Server) logImagePolicy() {
	if len(s.imageAllowlist) == 0 {
		slog.Warn("tenant image policy: NO registry allowlist — a create may name an image from any registry",
			"component", "httpapi",
			"set", "PILLDROPS_IMAGE_ALLOWLIST",
			"require_digest", s.imageRequireDigest)
		return
	}
	slog.Info("tenant image policy: registry allowlist enforced",
		"component", "httpapi",
		"allow", strings.Join(s.imageAllowlist, ","),
		"require_digest", s.imageRequireDigest)
}

// Close releases the rate limiters' evictors. Optional in short-lived processes.
func (s *Server) Close() { s.limiter.close(); s.previewLimiter.close(); s.authFailLimiter.close() }

// Limits returns the resolved protective limits.
func (s *Server) Limits() (maxActive, rateRPS, rateBurst int) {
	return s.maxActive, s.rateRPS, s.rateBurst
}

// SetSandboxDomain enables subdomain-form app URLs (https://<port>-<id>.<domain>)
// when domain is non-empty; otherwise URLs use the /preview/{id}/{port}/ path.
func (s *Server) SetSandboxDomain(domain string) { s.sandboxDomain = domain }

// SetIsolationOK records whether the boot-time isolation preflight passed.
func (s *Server) SetIsolationOK(ok bool) { s.isolationOK.Store(ok) }

// SetConsoleKey accepts the dashboard's signed account session in place of the
// key it was signed with. Without it the dashboard would sign someone in and
// then be refused on every call it makes.
func (s *Server) SetConsoleKey(key string) { s.consoleKey = key }

// SetDataDir sets the filesystem the host endpoint reports disk usage for. Empty means "/".
func (s *Server) SetDataDir(dir string) {
	if dir == "" {
		dir = "/"
	}
	s.dataDir = dir
}

// appURL builds the browser URL for a sandbox port, subdomain form when a sandbox
// domain is configured, else the /preview path. The ?ptok= is minted here because
// the subdomain form never passes the orchestrator.
func (s *Server) appURL(r *http.Request, id string, port int, org string) string {
	tok := s.previewToken(id, port, org)
	q := ""
	if tok != "" {
		q = "?ptok=" + tok
	}
	if s.sandboxDomain != "" {
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			return fmt.Sprintf("https://%d-%s.%s/%s", port, id, s.sandboxDomain, q)
		}
		hostPort := ""
		if _, p, err := net.SplitHostPort(r.Host); err == nil && p != "80" {
			hostPort = ":" + p
		}
		return fmt.Sprintf("http://%d-%s.%s%s/%s", port, id, s.sandboxDomain, hostPort, q)
	}
	return fmt.Sprintf("http://%s/preview/%s/%d/%s", r.Host, id, port, q)
}

// previewRoot is the path-form preview root as this request reached it, for
// the guest's PD_PREVIEW_BASE. Always the path form: the mux serves /preview/
// on the bare host whether or not a sandbox domain is configured.
// ponytail: host-form deployments get a URL that works but is not app_url's
// prefix; add a scheme://{port}-<id>.<domain> form when a guest needs it.
func (s *Server) previewRoot(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/preview"
}

// Routes builds a mux with the API mounted.
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	s.Register(mux)
	return mux
}

// Register mounts the API routes onto an existing mux. Health probes and
// /preview/* are open; everything else requires a key.
func (s *Server) Register(mux *http.ServeMux) {
	route := func(pattern string, h http.Handler) { mux.Handle(pattern, s.instrument(h)) }
	// raw skips instrument (body cap, span) for handlers that proxy a user's own
	// traffic. Named so the exemption is greppable.
	raw := func(pattern string, h http.Handler) { mux.Handle(pattern, h) }

	// /health stays as the liveness alias because deploy scripts curl it.
	route("GET /health", http.HandlerFunc(s.handleHealth))
	route("GET /livez", http.HandlerFunc(s.handleHealth))
	route("GET /readyz", http.HandlerFunc(s.handleReady))
	route("GET /metrics", s.auth(s.handleMetrics))
	route("GET /openapi.json", http.HandlerFunc(s.handleOpenAPI))
	route("GET /v1/host", s.auth(s.handleHost))
	route("GET /v1/nodes", s.auth(s.handleNodes))
	route("GET /v1/capabilities", s.auth(s.handleCapabilities))

	route("POST /v1/sandboxes", s.auth(s.handleCreate))
	route("GET /v1/sandboxes", s.auth(s.handleList))
	route("GET /v1/sandboxes/{id}", s.auth(s.handleGet))
	route("PATCH /v1/sandboxes/{id}", s.auth(s.handleRename))
	route("GET /v1/sandboxes/{id}/preview-status", s.auth(s.handlePreviewStatus))
	route("DELETE /v1/sandboxes/{id}", s.auth(s.handleDestroy))

	route("GET /v1/history", s.auth(s.handleHistoryList))
	route("GET /v1/history/{id}", s.auth(s.handleHistoryGet))
	route("GET /v1/events", s.auth(s.handleEventsList))
	route("GET /v1/concurrency", s.auth(s.handleConcurrency))
	route("POST /v1/sandboxes/{id}/exec", s.auth(s.handleExec))
	route("POST /v1/sandboxes/{id}/exec/stream", s.auth(s.handleExecStream))
	route("POST /v1/sandboxes/{id}/start", s.auth(s.handleStart))
	route("POST /v1/sandboxes/{id}/pause", s.auth(s.handlePause))
	route("POST /v1/sandboxes/{id}/standby", s.auth(s.handleStandby))
	route("POST /v1/sandboxes/{id}/resume", s.auth(s.handleResume))
	route("POST /v1/sandboxes/{id}/rehome", s.auth(s.handleRehome))
	route("POST /v1/sandboxes/{id}/fork", s.auth(s.handleFork))
	route("POST /v1/sandboxes/{id}/visibility", s.auth(s.handleVisibility))
	route("POST /v1/sandboxes/{id}/timeout", s.auth(s.handleTimeout))
	route("POST /v1/sandboxes/{id}/refresh", s.auth(s.handleRefresh))

	// Config is applied per-exec, so a change lands on the next command.
	route("POST /v1/sandboxes/{id}/config", s.auth(s.handleConfigSet))
	route("GET /v1/sandboxes/{id}/config", s.auth(s.handleConfigGet))

	route("GET /v1/sandboxes/{id}/metrics", s.auth(s.handleSandboxMetrics))
	route("GET /v1/sandboxes/{id}/ports", s.auth(s.handleSandboxPorts))
	route("POST /v1/sandboxes/{id}/files", s.auth(s.handleWriteFile))
	route("GET /v1/sandboxes/{id}/files", s.auth(s.handleReadFile))
	route("GET /v1/sandboxes/{id}/filesystem/list", s.auth(s.handleListDir))
	route("POST /v1/sandboxes/{id}/filesystem/mkdir", s.auth(s.handleMkdir))
	route("POST /v1/sandboxes/{id}/filesystem/move", s.auth(s.handleMove))
	route("POST /v1/sandboxes/{id}/filesystem/remove", s.auth(s.handleRemove))
	route("GET /v1/sandboxes/{id}/filesystem/stat", s.auth(s.handleStat))
	route("GET /v1/sandboxes/{id}/filesystem/watch", s.auth(s.handleWatch))

	route("GET /v1/sandboxes/{id}/history", s.auth(s.handleUnbuilt))
	route("GET /v1/sandboxes/{id}/logs", s.auth(s.handleSandboxLogs))
	route("GET /v1/sandboxes/{id}/deploy", s.auth(s.handleDeployStatus))
	route("GET /v1/sandboxes/{id}/process/list", s.auth(s.handleProcList))
	route("POST /v1/sandboxes/{id}/process/signal", s.auth(s.handleProcSignal))
	route("GET /v1/members", s.auth(s.handleUnbuilt))
	route("GET /v1/webhooks", s.auth(s.handleUnbuilt))
	route("GET /v1/builds", s.auth(s.handleUnbuilt))
	route("GET /v1/limits", s.auth(s.handleEntitlements))
	// Org-blind inventory for the orchestrator's placement reconciler.
	route("GET /v1/admin/sandboxes", s.authAdmin(s.handleAdminSandboxList))

	s.registerAgentRoutes(route)
	s.registerImageRoutes(route)
	s.registerCheckpointRoutes(route)
	s.registerBackupRoutes(route)
	s.registerSnapshotExportRoutes(route)
	s.registerOpsRoutes(route)
	s.registerFunctionRoutes(route, raw)
	s.registerCronRoutes(route)
	s.registerIntegrationRoutes(route)
	s.registerChatRoutes(route)
	s.registerForwardRoutes(raw)

	// No API key on the preview proxy, so throttle per IP and require an
	// org-bound preview token.
	raw("/preview/", s.throttlePreview(s.requirePreviewToken(proxy.Handler(s.d))))
}

// handleHealth answers liveness. It must stay dependency-free: a failure here
// means "restart me", and a database blip is not fixed by a restart.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReady answers readiness: the store is reachable. Load balancers drain on this.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.st.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unready", "check": "database"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// writeJSON encodes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr sends a JSON error body.
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": codeForStatus(status)})
}

// writeErrCode writes an error with a stable machine-readable code alongside the message.
func writeErrCode(w http.ResponseWriter, status int, code, msg string) {
	body := map[string]any{"error": msg}
	if code != "" {
		body["code"] = code
	}
	writeJSON(w, status, body)
}

// codeForStatus maps an HTTP status to a stable error code SDKs can branch on.
func codeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusPaymentRequired:
		return "payment_required"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusRequestEntityTooLarge:
		return "payload_too_large"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusServiceUnavailable:
		return "unavailable"
	default:
		return "error"
	}
}
