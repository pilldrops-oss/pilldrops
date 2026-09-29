package httpapi

import "net/http"

// registerOpsRoutes mounts the account and operator-console routes through
// Register's own route wrapper so they keep its metrics and auth. GET
// /v1/admin/sandboxes stays in Register because the placement reconciler polls it.
func (s *Server) registerOpsRoutes(route func(string, http.Handler)) {
	route("GET /v1/keys", s.auth(s.handleListKeys))
	route("POST /v1/keys", s.auth(s.handleCreateKey))
	route("DELETE /v1/keys/{id}", s.auth(s.handleDeleteKey))
	route("GET /v1/usage", s.auth(s.handleUsage))
	route("GET /v1/audit", s.auth(s.handleAudit))

	route("POST /v1/admin/login-event", s.authAdmin(s.handleLoginEvent))
	route("GET /v1/admin/users", s.authAdmin(s.handleAdminUsers))
	route("GET /v1/admin/users/{ownerID}", s.authAdmin(s.handleAdminUserDetail))
	route("GET /v1/admin/overview", s.authAdmin(s.handleAdminOverview))
	route("GET /v1/admin/funnel", s.authAdmin(s.handleAdminFunnel))
	// No payment routes: the core meters and enforces limits but takes no money.
	route("GET /v1/admin/sessions", s.authAdmin(s.handleAdminSessions))
	route("GET /v1/admin/errors", s.authAdmin(s.handleAdminErrors))
	route("GET /v1/admin/history/{id}", s.authAdmin(s.handleAdminHistory))
	route("GET /v1/admin/sandboxes/{id}", s.authAdmin(s.handleAdminSandbox))
}
