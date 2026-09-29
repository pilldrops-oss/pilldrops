package main

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/pilldrops-oss/pilldrops/internal/httpx"
)

// agentSubtree is one wildcard route for the whole agent API, so a new node
// endpoint never 404s at the portal; the node does all authorization. In fleet
// mode the sandboxProxy middleware answers before the body runs.
const agentSubtree = "/v1/sandboxes/{id}/agent/*"

// agentPathSegment is matched as a segment so a sandbox id containing "agent"
// is not mistaken for an agent path.
const agentPathSegment = "/agent/"

// isAgentPath reports whether p is an agent API request under /v1/sandboxes.
// The query is stripped so a client-controlled parameter cannot select this path.
func isAgentPath(p string) bool {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	return strings.HasPrefix(p, "/v1/sandboxes/") && strings.Contains(p, agentPathSegment)
}

// agentRelayHeaders is an allowlist, not a blanket copy: the inbound
// Authorization is the customer's portal credential and must not reach machines
// that run customer code. Last-Event-ID is how an interrupted stream resumes.
var agentRelayHeaders = []string{"Last-Event-ID", "Idempotency-Key", "Accept"}

// portalStreamClient has no overall timeout: http.Client.Timeout also bounds
// reading the body, which would sever an open event stream. Redirects are
// relayed, not followed, so a 303's Set-Cookie (the share-code claim) survives.
var portalStreamClient = newPortalStreamClient()

// portalBufferedClient serves proxyToOrchestrator's request/response exchanges;
// unlike the stream client it carries an overall timeout.
var portalBufferedClient = &http.Client{
	Timeout:   60 * time.Second,
	Transport: httpx.NewTransport(),
}

func newPortalStreamClient() *http.Client {
	return &http.Client{
		// A literal &http.Transport{} has no dial timeout and only 2 idle conns per host.
		Transport: httpx.NewTransport(),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// serveAgentProxy forwards one agent API request to the orchestrator. Shared by
// the middleware and its tests; the production caller is a closure in main().
func serveAgentProxy(w http.ResponseWriter, r *http.Request, orchURL, orchKey, owner string) {
	streamToOrchestrator(w, r, orchURL, orchKey, owner, agentRelayHeaders...)
}

// registerAgentRoutes mounts the agent API subtree on the portal.
func registerAgentRoutes(r chi.Router, proxyMode bool) {
	r.Handle(agentSubtree, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !proxyMode {
			// 501 rather than 404: this is a configuration gap, not a missing feature.
			http.Error(w, `{"error":"the agent API requires fleet mode: set PORTAL_ORCHESTRATOR_URL and PORTAL_ORCHESTRATOR_KEY. The portal's local runtime does not run agentd, so there is no agent to reach."}`,
				http.StatusNotImplemented)
			return
		}
		// Unreachable in fleet mode; a 500 makes a middleware-ordering regression visible.
		http.Error(w, `{"error":"agent proxy not wired"}`, http.StatusInternalServerError)
	}))
}
