//go:build !core

package httpapi

import (
	"net/http"

	"github.com/pilldrops-oss/pilldrops/internal/auth/aat"
)

// registerAgentRoutes mounts the agent API through Register's own route wrapper
// so these keep the same metrics and auth wrapping as every other route.
func (s *Server) registerAgentRoutes(route func(string, http.Handler)) {
	// Every handler re-runs the KindAgent gate and 404s for any other template.
	route("GET /v1/sandboxes/{id}/agent/status", s.authAgentScope(aat.ScopeRead, s.handleAgentStatus))
	route("POST /v1/sandboxes/{id}/agent/turns", s.authAgentScope(aat.ScopeWrite, s.handleAgentSubmit))
	route("GET /v1/sandboxes/{id}/agent/turns", s.authAgentScope(aat.ScopeRead, s.handleAgentTurns))
	route("GET /v1/sandboxes/{id}/agent/turns/{turn}", s.authAgentScope(aat.ScopeRead, s.handleAgentTurn))
	route("DELETE /v1/sandboxes/{id}/agent/turns/{turn}", s.authAgentScope(aat.ScopeWrite, s.handleAgentTurnDelete))
	route("POST /v1/sandboxes/{id}/agent/turns/{turn}/interrupt", s.authAgentScope(aat.ScopeWrite, s.handleAgentInterrupt))
	route("POST /v1/sandboxes/{id}/agent/queue/reorder", s.authAgentScope(aat.ScopeWrite, s.handleAgentReorder))
	route("GET /v1/sandboxes/{id}/agent/events", s.authAgentScope(aat.ScopeRead, s.handleAgentEvents))
	route("GET /v1/sandboxes/{id}/agent/models", s.authAgentScope(aat.ScopeRead, s.handleAgentModels))
	route("GET /v1/sandboxes/{id}/agent/budget", s.authAgentScope(aat.ScopeRead, s.handleAgentBudget))
	route("PATCH /v1/sandboxes/{id}/agent/budget", s.authAgentScope(aat.ScopeAdmin, s.handleAgentBudget))
	route("GET /v1/sandboxes/{id}/agent/threads", s.authAgentScope(aat.ScopeRead, s.handleAgentThreads))
	route("GET /v1/sandboxes/{id}/agent/threads/{thread}", s.authAgentScope(aat.ScopeRead, s.handleAgentThread))
	route("POST /v1/sandboxes/{id}/agent/tokens", s.auth(s.handleAgentTokenCreate))
	route("GET /v1/sandboxes/{id}/agent/tokens", s.auth(s.handleAgentTokenList))
	route("DELETE /v1/sandboxes/{id}/agent/tokens/{tokenID}", s.auth(s.handleAgentTokenRevoke))
	route("POST /v1/sandboxes/{id}/agent/tokens/{tokenID}/rotate", s.auth(s.handleAgentTokenRotate))
	route("POST /v1/sandboxes/{id}/agent/share", s.auth(s.handleAgentShare))
	// Org-key only: the connect bundle mints a credential, and a credential that
	// can issue its own successors is not revocable in practice.
	route("GET /v1/sandboxes/{id}/agent/connect", s.auth(s.handleAgentConnect))
	// Read scope: mints nothing and reveals env names only, never values.
	route("GET /v1/sandboxes/{id}/agent/isolation", s.authAgentScope(aat.ScopeRead, s.handleAgentIsolation))
	route("GET /v1/sandboxes/{id}/agent/share/qr", s.auth(s.handleAgentShareQR))
	// Org-key only: a delegated token must not be able to aim this server's requests.
	route("GET /v1/sandboxes/{id}/agent/webhooks", s.auth(s.handleAgentWebhooks))
	route("POST /v1/sandboxes/{id}/agent/webhooks", s.auth(s.handleAgentWebhooks))
	route("GET /v1/sandboxes/{id}/agent/webhooks/failures", s.auth(s.handleAgentWebhookFailures))
	route("DELETE /v1/sandboxes/{id}/agent/webhooks/{hookID}", s.auth(s.handleAgentWebhookDelete))
	route("POST /v1/sandboxes/{id}/agent/webhooks/{hookID}/test", s.auth(s.handleAgentWebhookTest))
	// Revoking a remembered grant is admin scope: it changes what future turns may do without review.
	route("GET /v1/sandboxes/{id}/agent/approvals", s.authAgentScope(aat.ScopeRead, s.handleAgentApprovals))
	route("POST /v1/sandboxes/{id}/agent/approvals/{approvalID}", s.authAgentScope(aat.ScopeApprove, s.handleAgentApprovalDecide))
	route("GET /v1/sandboxes/{id}/agent/policy", s.authAgentScope(aat.ScopeRead, s.handleAgentPolicy))
	route("DELETE /v1/sandboxes/{id}/agent/policy/remembered", s.authAgentScope(aat.ScopeAdmin, s.handleAgentPolicy))
	// Unauthenticated: the single-use, short-lived code is the credential.
	// The control plane routes by the {id} in the path, so only the sandbox-scoped
	// form is proxyable; the bare form serves direct node hits and the agent origin.
	route("GET /v1/sandboxes/{id}/agent/claim", http.HandlerFunc(s.handleAgentClaim))
	route("GET /agent/claim", http.HandlerFunc(s.handleAgentClaim))
	route("POST /v1/sandboxes/{id}/agent/responses", s.authAgentScope(aat.ScopeWrite, s.handleAgentResponseCreate))
	route("GET /v1/sandboxes/{id}/agent/responses/{response}", s.authAgentScope(aat.ScopeRead, s.handleAgentResponseGet))
	route("GET /v1/sandboxes/{id}/agent/responses/{response}/stream", s.authAgentScope(aat.ScopeRead, s.handleAgentResponseStream))
	route("POST /v1/sandboxes/{id}/agent/responses/{response}/cancel", s.authAgentScope(aat.ScopeWrite, s.handleAgentResponseCancel))
}
