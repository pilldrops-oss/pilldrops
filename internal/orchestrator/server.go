// Package orchestrator is the control plane's HTTP surface: it authenticates
// callers, asks internal/cluster where a sandbox should live, and proxies to the
// node that owns it so clients see a single endpoint.
package orchestrator

import (
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/auth/adminauth"
	"github.com/pilldrops-oss/pilldrops/internal/auth/authcache"
	"github.com/pilldrops-oss/pilldrops/internal/auth/orgassert"
	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/config"
	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	"github.com/pilldrops-oss/pilldrops/internal/mcp"
	"github.com/pilldrops-oss/pilldrops/internal/observability"
	"github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/webui"
)

// devInsecure is the local-dev escape hatch: SANDBOXD_DEV=1 or DEV_INSECURE=1
// lets an unset credential fail open instead of closed.
func devInsecure() bool {
	return os.Getenv("SANDBOXD_DEV") == "1" || os.Getenv("DEV_INSECURE") == "1"
}

// Server is the multi-node control plane: nodes heartbeat to it, clients hit
// it as a single endpoint, and it schedules and proxies to the owning node.
type Server struct {
	reg       *cluster.Registry
	apiKey    string // bearer for client /v1 + /preview requests
	nodeToken string // shared secret nodes present when they heartbeat
	// orgAssertKey signs X-Org-Override so a node can verify but not mint one. nil means HMAC on nodeToken.
	orgAssertKey ed25519.PrivateKey
	// nodeKeySecret derives per-node API keys; empty falls back to nodeToken.
	nodeKeySecret string
	// multiTenant makes admission fail closed when it cannot establish state.
	multiTenant bool
	client      *http.Client
	// stream is the client for long-lived proxied streams; see newStreamClient.
	stream *http.Client
	// hopClients caches one client per node TLS pin; see pinnedClient.
	hopClients hopClientCache
	now        func() time.Time
	// authc memoizes customer key validation so the front door does not do a store round trip per request.
	authc *authcache.Cache
	// revoke tells the other replicas' auth caches a key was revoked; nil
	// without a shared Redis.
	revoke *cluster.Revocations
	// metrics is this replica's Prometheus surface, served at GET /metrics.
	metrics   *orchMetrics
	hbLimiter *ipLimiter // per-IP rate limit on the on-demand TLS check
	// hbIPLimit bounds heartbeats per source address before the token is
	// checked; hbNodeLimit bounds them per node after. See ratelimit.go.
	hbIPLimit, hbNodeLimit *ipLimiter
	orgRate                *cluster.OrgRate // per-org request cap across replicas; nil = off
	// previewLimit is the per-visitor preview throttle. It lives at the front
	// door because only here is the client address the visitor's: behind this
	// tier a node sees one address for everyone.
	previewLimit *ipLimiter
	admit        *cluster.Admission          // Redis concurrency check; nil = Postgres advisory lock
	locks        *cluster.Locks              // per-sandbox operation locks, shared across replicas via Redis
	pcache       staleCache                  // preview-gate store reads; see previewcache.go
	leader       *cluster.LeaderLease        // nil = always leader (single node); else Redis lease (HA)
	st           atomic.Pointer[store.Store] // shared Postgres; nil = proxy to a node. Set async at boot.
	ui           *webui.Server               // the dashboard; nil when ORCHESTRATOR_UI=off
	fb           *feedbackStore              // in-memory feedback inbox for the admin console
	publicSB     sync.Map                    // sandbox id -> true when created public; no-store fallback only

	// logger is the structured logger. Use the ...Context variants so
	// request_id and trace_id are stamped from the context.
	logger *slog.Logger

	join            cluster.JoinStore // join-token mint/consume + name allocation
	bootstrapDSN    string            // Postgres DSN handed to nodes on join
	bootstrapSecret string            // SANDBOXD_SECRET_KEY handed to nodes on join

	// sandboxDomain is PORTAL_SANDBOX_DOMAIN; empty means previews share the
	// dashboard's origin. See frameAncestorsPolicy.
	sandboxDomain string

	// domains caches custom-domain routing decisions for customDomainTTL,
	// negative answers included. Zero value ready.
	domains routeCache

	// maxBodyBytes (MAX_REQUEST_BYTES) bounds only the buffered-JSON endpoints,
	// not the streamed proxies, which the node caps itself. See capBody.
	maxBodyBytes         int64
	maxBuildContextBytes int64 // MAX_BUILD_CONTEXT_BYTES; applied by handleImageBuild itself
	maxArtifact          int64 // MAX_ARTIFACT_BYTES; applied by handleTemplateArtifactPublish itself

	// ingress holds this replica's bound public forward ports, nil unless
	// FORWARD_INGRESS=1. See portingress.go.
	ingress atomic.Pointer[forwardIngress]

	// artifacts is the shared artifact store. An atomic pointer so
	// SetArtifactStore may run while Routes() is serving; nil means
	// unconfigured and the publish routes refuse.
	artifacts atomic.Pointer[artifact.StorageProvider]
	// artifactDirect is how long a presigned URL a node is redirected to stays
	// valid; zero serves artifact bytes through this process instead.
	artifactDirect time.Duration
}

func NewServer(apiKey, nodeToken string) *Server {
	// REDIS_URL makes routes and the node table shared so N replicas can run.
	// A set-but-unreachable Redis is fatal: degrading to in-memory would
	// split-brain placement silently.
	reg := cluster.NewRegistry()
	var leader *cluster.LeaderLease
	if url := os.Getenv("REDIS_URL"); url != "" {
		rs, rsErr := cluster.NewRedisRouteStore(url)
		switch {
		case rsErr == nil:
			// The node table must be shared too: a load balancer delivers each heartbeat to one replica.
			ns, nsErr := cluster.NewRedisNodeStore(url)
			if nsErr != nil {
				slog.Error("REDIS_URL is set and the route catalog opened, but the node table did not. "+
					"Refusing to start with a per-replica node table, which is the split-brain HA exists to remove.",
					"component", "orchestrator", "err", nsErr)
				os.Exit(1)
			}
			reg = cluster.NewRegistryShared(ns, rs)
			slog.Info("route catalog and node table backed by Redis", "component", "orchestrator", "ha_ready", true)
		case allowMemoryRoutes():
			slog.Warn("REDIS_URL set but unreachable; continuing with in-memory routes because "+
				"ORCHESTRATOR_ALLOW_MEMORY_ROUTES=1. NEVER do this with more than one replica: "+
				"routes and leadership are per-process, so placement will diverge.",
				"component", "orchestrator", "err", rsErr)
		default:
			slog.Error("REDIS_URL is set but Redis is unreachable. Refusing to start with in-memory "+
				"routes, which would give this replica a private route table and its own reconciler "+
				"lease. Fix Redis, unset REDIS_URL for a genuinely single-node deployment, or set "+
				"ORCHESTRATOR_ALLOW_MEMORY_ROUTES=1 to override.",
				"component", "orchestrator", "err", rsErr)
			os.Exit(1)
		}
		// One leader for the reconciler so N replicas do not all run it.
		if ll, err := cluster.NewLeaderLease(url, "sandforge:leader:reconciler", cluster.NewReplicaID(), 15*time.Second); err == nil {
			leader = ll
		} else if rsErr == nil {
			slog.Warn("leader lease unavailable; this replica will run the reconciler unconditionally",
				"component", "orchestrator", "err", err)
		}
	}
	// JoinStore follows the route store: Redis when reachable, else in-memory.
	var join cluster.JoinStore = cluster.NewMemJoinStore()
	if url := os.Getenv("REDIS_URL"); url != "" {
		if rj, err := cluster.NewRedisJoinStore(url); err == nil {
			join = rj
		} else {
			slog.Warn("join store falling back to in-memory; join tokens will not work across replicas",
				"component", "orchestrator", "err", err)
		}
	}
	s := &Server{
		reg:           reg,
		apiKey:        apiKey,
		nodeToken:     nodeToken,
		multiTenant:   multiTenantFromEnv(),
		nodeKeySecret: os.Getenv("NODE_KEY_SECRET"),
		orgAssertKey: func() ed25519.PrivateKey {
			k, err := orgassert.ParsePrivateKey(os.Getenv("ORG_ASSERT_KEY"))
			if err != nil {
				// Falling back to HMAC on a key the operator set but cannot parse would silently leave forgery open.
				panic("ORG_ASSERT_KEY: " + err.Error())
			}
			return k
		}(),
		// http.DefaultTransport caps idle connections per host at 2; every buffered node forward uses this client.
		client:      &http.Client{Timeout: 150 * time.Second, Transport: httpx.NewTransport()},
		stream:      newStreamClient(),
		now:         time.Now,
		authc:       authcache.New(),
		metrics:     newOrchMetrics(),
		hbLimiter:   newIPLimiter(heartbeatRPS, heartbeatBurst),
		hbIPLimit:   newIPLimiter(heartbeatIPRPS, heartbeatIPBurst),
		hbNodeLimit: newIPLimiter(heartbeatNodeRPS, heartbeatNodeBurst),
		previewLimit: newIPLimiter(float64(config.Int("PREVIEW_RATE_RPS", 50)),
			float64(config.Int("PREVIEW_RATE_BURST", 100))),
		// Same env vars and format as the node's logger.
		logger:          observability.NewLogger(os.Stderr, os.Getenv("LOG_FORMAT"), os.Getenv("LOG_LEVEL")),
		leader:          leader,
		fb:              newFeedbackStore(1000),
		join:            join,
		bootstrapDSN:    os.Getenv("NODE_BOOTSTRAP_DSN"),
		bootstrapSecret: os.Getenv("NODE_BOOTSTRAP_SECRET"),
		sandboxDomain:   os.Getenv("PORTAL_SANDBOX_DOMAIN"),

		// Buffered JSON bodies only; the node's own MAX_REQUEST_BYTES governs the streamed routes.
		maxBodyBytes:         int64(config.Int("MAX_REQUEST_BYTES", 10<<20)),
		maxBuildContextBytes: int64(config.Int("MAX_BUILD_CONTEXT_BYTES", 512<<20)),
		// 0 = defaultMaxArtifactBytes, which does not fit in an int on a 32-bit build.
		maxArtifact: int64(config.Int("MAX_ARTIFACT_BYTES", 0)),
		// On by default: a bucket serves downloads better than this process
		// does, and it only applies to a store that can presign. =0 when the
		// nodes cannot reach the bucket's endpoint.
		artifactDirect: time.Duration(config.Int("ORCHESTRATOR_ARTIFACT_DIRECT_SECONDS", 900)) * time.Second,
	}
	// Per-sandbox locks (a move and a destroy must not overlap) go wherever the
	// replicas share state; one replica keeps them in process.
	if l, err := cluster.NewLocks(os.Getenv("REDIS_URL")); err == nil {
		s.locks = l
	} else {
		s.locks, _ = cluster.NewLocks("")
		slog.Warn("sandbox locks are per-replica: Redis unreachable", "component", "orchestrator", "err", err)
	}
	// Revocations clear every replica's and edge's auth cache, not only the
	// one that took the request.
	if r, err := cluster.NewRevocations(os.Getenv("REDIS_URL")); err == nil && r != nil {
		s.revoke = r
		go r.Subscribe(context.Background(), s.authc.Clear)
	} else if err != nil {
		slog.Warn("key revocations reach other replicas only through the auth-cache TTL: Redis unreachable",
			"component", "orchestrator", "err", err)
	}
	// With a shared Redis the concurrency check claims there, not under a
	// Postgres advisory lock per org; Postgres stays the fallback.
	if url := os.Getenv("REDIS_URL"); url != "" {
		if a, err := cluster.NewAdmission(url); err == nil {
			s.admit = a
		}
	}
	// ORG_RATE_PER_SEC bounds one org fleet-wide; each node's own limit only
	// sees the requests that reach it. 40 is a node's burst; 0 turns it off.
	if o, err := cluster.NewOrgRate(config.Int("ORG_RATE_PER_SEC", 40), os.Getenv("REDIS_URL")); err == nil {
		s.orgRate = o
	} else {
		slog.Warn("per-org rate limit unavailable; the nodes' own limits still apply",
			"component", "orchestrator", "err", err)
	}
	// The dashboard is served here because this is the tier that
	// authenticates customers. ORCHESTRATOR_UI=off leaves it unmounted.
	if os.Getenv("ORCHESTRATOR_UI") != "off" {
		if ui, err := webui.New(apiKey, "orchestrator", "user", true, os.Getenv("PORTAL_SANDBOX_DOMAIN")); err == nil {
			// This tier's /install.sh is the node joiner; webui's CLI installer would collide.
			ui.SkipInstallScript()
			s.ui = ui
		}
	}
	return s
}

// SetStore wires the shared Postgres; nil falls back to proxying a node. It
// also switches the console from the API-key prompt to its own accounts.
func (s *Server) SetStore(st *store.Store) {
	s.st.Store(st)
	if st != nil {
		s.reg.SetRouteFallback(func(id string) (string, bool) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			rec, ok, err := st.SandboxByID(ctx, id)
			if err != nil || !ok || rec.NodeID == "" {
				return "", false
			}
			return rec.NodeID, true
		})
	}
	if s.ui != nil && st != nil {
		s.ui.SetAdminStore(store.AccountsOf(st))
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	// The liveness body stays the literal "ok"; runbooks and probes match on it.
	mux.HandleFunc("GET /health", s.handleLive)
	mux.HandleFunc("GET /livez", s.handleLive)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("POST /internal/heartbeat", s.handleHeartbeat)
	// Behind auth: the scrape names every node.
	mux.HandleFunc("GET /metrics", s.auth(s.handleMetrics))
	mux.HandleFunc("GET /v1/nodes", s.auth(s.handleNodes))
	// Mounted explicitly, or the /v1/ catch-all would return one node's answer as the fleet's.
	mux.HandleFunc("GET /v1/capabilities", s.auth(s.handleCapabilities))
	mux.HandleFunc("GET /v1/whoami", s.auth(s.handleWhoami))
	mux.HandleFunc("POST /v1/sandboxes", s.auth(s.handleCreate))
	mux.HandleFunc("GET /v1/sandboxes", s.auth(s.handleList))
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", s.auth(s.bySlug(s.handleDestroy)))
	// {id}/{rest...} below does not match an empty tail.
	mux.HandleFunc("GET /v1/sandboxes/{id}", s.auth(s.bySlug(s.handleSandboxGet)))
	mux.HandleFunc("PATCH /v1/sandboxes/{id}", s.auth(s.bySlug(s.handleSandboxRename)))
	// Explicit: a fork registers a new sandbox id, which the proxy cannot place.
	mux.HandleFunc("POST /v1/sandboxes/{id}/fork", s.auth(s.bySlug(s.handleFork)))
	mux.HandleFunc("GET /v1/sandboxes/{id}/preview-status", s.auth(s.bySlug(s.handlePreviewStatus)))
	mux.HandleFunc("GET /v1/sandboxes/{id}/metrics", s.auth(s.bySlug(s.handleSandboxMetrics)))
	mux.HandleFunc("GET /v1/sandboxes/{id}/ports", s.auth(s.bySlug(s.handleSandboxPorts)))
	mux.HandleFunc("GET /v1/cluster/stats", s.auth(s.handleClusterStats))
	mux.HandleFunc("POST /v1/nodes/{id}/cordon", s.auth(s.handleCordon))
	mux.HandleFunc("POST /v1/nodes/{id}/uncordon", s.auth(s.handleUncordon))
	mux.HandleFunc("POST /v1/nodes/{id}/drain", s.auth(s.handleDrain))
	mux.HandleFunc("DELETE /v1/nodes/{id}", s.auth(s.handleRemoveNode))
	mux.HandleFunc("GET /install.sh", s.handleInstallScript) // public: runs before any credential exists
	mux.HandleFunc("GET /bin/{name}", s.handleNodeBinary)    // public: install.sh fetches these before any credential exists
	mux.HandleFunc("POST /v1/join-tokens", s.auth(s.handleMintJoinToken))
	mux.HandleFunc("POST /v1/join", s.handleJoin) // not behind s.auth: the token is the auth
	mux.HandleFunc("GET /v1/images", s.auth(s.handleImagesList))
	mux.HandleFunc("GET /v1/history", s.auth(s.handleHistoryList))
	mux.HandleFunc("GET /v1/limits", s.auth(s.handleLimitsProxy))
	mux.HandleFunc("GET /v1/usage", s.auth(s.handleUsageProxy))
	mux.HandleFunc("GET /v1/events", s.auth(s.handleEventsProxy))
	mux.HandleFunc("GET /v1/concurrency", s.auth(s.handleConcurrencyProxy))
	// Fanned out to every node so the tag launches anywhere.
	mux.HandleFunc("POST /v1/images/build", s.auth(s.handleImageBuild))
	mux.HandleFunc("GET /v1/history/{id}", s.auth(s.handleHistoryGet))
	mux.HandleFunc("PATCH /v1/images/{id}", s.auth(s.handleImagePatch))
	// Admin analytics go to one healthy node; a fan-out would double-count logins.
	mux.HandleFunc("POST /v1/admin/login-event", s.auth(s.handleLoginEventProxy))
	mux.HandleFunc("GET /v1/admin/users", s.auth(s.handleAdminUsersProxy))
	mux.HandleFunc("GET /v1/admin/users/{ownerID}", s.auth(s.handleAdminUserDetailProxy))
	mux.HandleFunc("GET /v1/admin/overview", s.auth(s.handleAdminOverviewProxy))
	mux.HandleFunc("GET /v1/admin/sessions", s.auth(s.handleAdminSessionsProxy))
	mux.HandleFunc("GET /v1/admin/errors", s.auth(s.handleAdminErrorsProxy))
	mux.HandleFunc("GET /v1/admin/history/{id}", s.auth(s.handleAdminHistoryProxy))
	mux.HandleFunc("GET /v1/admin/sandboxes/{id}", s.auth(s.handleAdminSandboxProxy))
	// Authorization probe for the admin SPA: 200 = allowed, 403 = signed in but not allow-listed.
	mux.HandleFunc("GET /v1/admin/whoami", s.auth(s.handleAdminWhoami))

	mux.HandleFunc("POST /v1/feedback", s.auth(s.handleFeedbackCreate))
	mux.HandleFunc("GET /v1/admin/feedback", s.auth(s.handleFeedbackList))

	mux.HandleFunc("POST /v1/waitlist", s.auth(s.handleWaitlistCreateProxy))
	mux.HandleFunc("GET /v1/admin/waitlist", s.auth(s.handleAdminWaitlistProxy))
	mux.HandleFunc("/v1/sandboxes/{id}/{rest...}", s.auth(s.bySlug(s.handleSandboxProxy)))

	// Routes this tier answers from the shared store go before the /v1/
	// catch-all, which forwards everything else to any healthy node.
	s.registerProjectRoutes(mux)
	s.registerVaultRoutes(mux)
	s.registerPreviewMint(mux)
	s.registerDomainRoutes(mux)
	s.registerPreviewSourceRoutes(mux)
	s.registerOrgEgressRoutes(mux)
	s.registerForwardRoutes(mux)
	s.registerPublicTemplateRoutes(mux)
	s.registerLaunchStatsRoutes(mux)
	// Artifact publish lives here because a node's database role has only SELECT on template_builds and artifacts.
	s.registerArtifactRoutes(mux)
	// Same reason: a named snapshot is a template build, and only this tier may publish one.
	s.registerSnapshotRoutes(mux)
	// Moving a sandbox rides the snapshot plane: capture, create elsewhere, evict.
	s.registerMoveRoutes(mux)

	// MCP dispatches every tool call in-process to mux, so the /v1 handlers
	// keep enforcing auth and org scoping. The mux reference is only followed
	// at request time, after Routes() has returned.
	mux.Handle("/mcp", &mcp.HTTPServer{API: mux})

	mux.HandleFunc("/v1/", s.auth(s.handleNodeResourceProxy))

	// The SPA's path to the live-stats stream is a constant, so this tier must serve it.
	mux.HandleFunc("GET /dashboard/api/events", s.auth(s.handleDashboardEvents))
	mux.HandleFunc("GET /dashboard/api/stats", s.auth(s.handleDashboardStats))

	// The dashboard and its login, last so no /v1 pattern is shadowed.
	if s.ui != nil {
		s.ui.Register(mux)
	}
	// Public, like the node's preview; websocket-capable.
	mux.HandleFunc("/preview/{id}/{rest...}", s.handlePreview)
	// hostIngress is outermost so a wildcard-host preview is rewritten into
	// the path form before the mux sees it. observability.Middleware resolves
	// the correlation id everything below reads. The span is named by mux
	// pattern so sandbox ids do not become distinct operations.
	return s.hostIngress(observability.Middleware(observability.TraceMiddleware(func(r *http.Request) string {
		_, pattern := mux.Handler(r)
		return pattern
	})(s.capBody(mux))))
}

// EdgeRoutes is the sandbox data path alone: previews in path and host form,
// plus customer domains and health. Run it as its own
// replicas (ORCHESTRATOR_ROLE=edge) so preview traffic scales, and survives,
// apart from the API. It reads the same Redis routes and node table; it
// schedules nothing and serves no API or dashboard.
func (s *Server) EdgeRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleLive)
	mux.HandleFunc("GET /livez", s.handleLive)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("/preview/{id}/{rest...}", s.handlePreview)
	return s.hostIngress(observability.Middleware(observability.TraceMiddleware(func(r *http.Request) string {
		_, pattern := mux.Handler(r)
		return pattern
	})(mux)))
}

// handleLive checks no dependency: the only response to a liveness failure is
// a restart, which fixes nothing a dependency outage caused.
func (s *Server) handleLive(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("ok")) //nolint:errcheck // best-effort: nothing left to do if the client already went away
}

// handleReady checks only what this process owns. A nil store is a supported
// mode, and fleet health is not a readiness signal: pulling this tier out of
// rotation would remove the component still able to report the outage.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	st := s.st.Load()
	if st == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "store": "not-configured"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := st.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unready", "check": "database"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// allowMemoryRoutes lets a single-replica deployment ride out a Redis outage.
// Never set it with more than one replica. Deliberately not covered by devInsecure.
func allowMemoryRoutes() bool { return config.Bool("ORCHESTRATOR_ALLOW_MEMORY_ROUTES", false) }

// capBody wraps r.Body only, never the ResponseWriter, so streaming routes
// keep http.Hijacker and http.Flusher. Exempt routes enforce their own cap;
// double-capping would turn a clean 413 into a mid-stream 502.
func (s *Server) capBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && s.maxBodyBytes > 0 && !bodyCapExempt(r.URL.Path) {
			r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// bodyCapExempt reports whether p enforces its own body cap or streams the body to a node.
func bodyCapExempt(p string) bool {
	return p == "/v1/images/build" || isStreamedProxyPath(p) || isTemplateArtifactPath(p)
}

// isStreamedProxyPath matches by shape because capBody runs before routing:
// /preview/... and /v1/sandboxes/<id>/<more> stream the body through; the
// bare id and the collection path stay capped.
func isStreamedProxyPath(p string) bool {
	if strings.HasPrefix(p, "/preview/") {
		return true
	}
	const sbxPrefix = "/v1/sandboxes/"
	if !strings.HasPrefix(p, sbxPrefix) {
		return false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(p, sbxPrefix), "/")
	return strings.Contains(rest, "/")
}

// bearer extracts the Authorization bearer token, or "". Go trims trailing
// header whitespace, so "Bearer " arrives as "Bearer" and a naive TrimPrefix
// would return the literal instead of "".
func bearer(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(h) < len("Bearer") || !strings.EqualFold(h[:len("Bearer")], "Bearer") {
		return ""
	}
	return strings.TrimSpace(h[len("Bearer"):])
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiKey == "" {
			// Fail closed: an unset key must not serve anonymously.
			if devInsecure() {
				next(w, r)
				return
			}
			writeErr(w, 503, "orchestrator not configured: no API key set (refusing to serve unauthenticated; set ORCHESTRATOR_API_KEY, or DEV_INSECURE=1 for local dev)")
			return
		}
		// The operator key is the only credential allowed to name another org via X-Org-Override.
		tok := bearer(r)
		if c, err := r.Cookie(sessionCookie); err == nil && tok == "" {
			// A console session is an operator, verified by signature. Kept
			// apart from the key comparison: it expires and names a person.
			if _, err := adminauth.VerifySession([]byte(s.apiKey), c.Value, s.now()); err == nil {
				next(w, r.WithContext(withOperator(r.Context())))
				return
			}
			tok = c.Value
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.apiKey)) == 1 {
			// The portal holds this key and names the customer key it
			// authenticated; the node is told both, under one signature.
			id, role := keyPrincipalFrom(r)
			r = r.WithContext(withKey(withOperator(r.Context()), id, role))
			// The portal acting for a customer is limited like that customer
			// calling with their own key; its own traffic names no org.
			if s.overOrgRate(w, actingForOrg(r)) {
				return
			}
			next(w, r)
			return
		}
		// A sandbox acting as itself: one row read, never cached, allowlisted.
		if s.authSandbox(w, r, next, tok) {
			return
		}
		// A customer key: the org comes from the key, never from a header the caller controls.
		if st := s.st.Load(); st != nil && tok != "" {
			// Memoized; a key revoked elsewhere stays usable for up to authcache.TTL.
			hash := authcache.Key(tok)
			key, ok := s.authc.Get(hash)
			if ok {
				s.metrics.incAuthCacheHit()
			} else {
				s.metrics.incAuthCacheMiss()
				var err error
				if key, err = st.ValidateKey(r.Context(), tok); err == nil {
					s.authc.Put(hash, key)
					ok = true
				}
			}
			if ok {
				if s.overOrgRate(w, key.OrgID) {
					return
				}
				next(w, r.WithContext(withKey(withOrg(r.Context(), key.OrgID), key.ID, key.Role)))
				return
			}
		}
		writeErr(w, 401, "missing or invalid bearer token")
	}
}

// overOrgRate answers 429 and reports true when org is over its fleet-wide rate.
func (s *Server) overOrgRate(w http.ResponseWriter, org string) bool {
	if s.orgRate.Allow(org, s.now()) {
		return false
	}
	w.Header().Set("Retry-After", "1")
	writeJSON(w, http.StatusTooManyRequests, map[string]string{
		"error": "this organisation is over its request rate across the fleet; retry after a second",
		"code":  "rate_limited",
	})
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v) //nolint:errcheck // best-effort: status/headers already committed, nothing left to do on an encode/write failure
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// log returns the logger, never nil; a zero-value Server gets the default.
func (s *Server) log() *slog.Logger {
	if s.logger == nil {
		return slog.Default()
	}
	return s.logger
}

// sessionCookie is the dashboard's login cookie. Duplicated from
// internal/webui because importing it would embed the SPA into this binary.
const sessionCookie = "sandboxd_session"

// Caller identity, resolved by auth(). An operator's X-Org-Override is
// honoured; a customer's org comes from their key and no header changes it.
type ctxKey int

const (
	ctxOperator ctxKey = iota
	ctxOrg
)

func withOperator(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxOperator, true)
}

func withOrg(ctx context.Context, org string) context.Context {
	return context.WithValue(ctx, ctxOrg, org)
}

// isOperator reports whether this request holds the fleet-wide key.
func isOperator(r *http.Request) bool {
	v, _ := r.Context().Value(ctxOperator).(bool)
	return v
}

// authedOrg returns the org resolved from a customer key, or "" for an operator.
func authedOrg(r *http.Request) string {
	v, _ := r.Context().Value(ctxOrg).(string)
	return v
}

// handleNodeResourceProxy forwards an org-scoped resource request to any
// healthy node, scoped to the authenticated org.
func (s *Server) handleNodeResourceProxy(w http.ResponseWriter, r *http.Request) {
	// A key revocation is the one write every replica's auth cache has to
	// hear about: without this a revoked key kept working on the others for
	// the cache TTL (60 s on the rig).
	if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/keys/") {
		rec := &revokeStatus{ResponseWriter: w, code: http.StatusOK}
		s.proxyToNode(rec, r, r.Method, r.URL.Path)
		if rec.code/100 == 2 {
			s.authc.Clear()
			s.revoke.Publish()
		}
		return
	}
	s.proxyToNode(w, r, r.Method, r.URL.Path)
}

// revokeStatus records the status a key revocation was answered with.
type revokeStatus struct {
	http.ResponseWriter
	code int
}

func (w *revokeStatus) WriteHeader(code int) { w.code = code; w.ResponseWriter.WriteHeader(code) }

// The caller's Idempotency-Key travels in the context because forwardOnce
// takes no request; dropping it at this hop made retried creates run twice.
type idemKeyCtx struct{}

func withIdempotencyKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, idemKeyCtx{}, key)
}

func idempotencyKey(ctx context.Context) string {
	v, _ := ctx.Value(idemKeyCtx{}).(string)
	return v
}
