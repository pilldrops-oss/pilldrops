//go:build !core

package httpapi

import "net/http"

// registerIntegrationRoutes mounts the MCP integration routes through Register's
// own route wrapper so they get the same metrics and auth as every other route.
func (s *Server) registerIntegrationRoutes(route func(string, http.Handler)) {
	route("GET /v1/integrations", s.auth(s.handleIntegrationsCatalog))
	route("POST /v1/sandboxes/{id}/integrations", s.auth(s.handleIntegrationsSet))
	route("GET /v1/sandboxes/{id}/integrations", s.auth(s.handleIntegrationsGet))
}
