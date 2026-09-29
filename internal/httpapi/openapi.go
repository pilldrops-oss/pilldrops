package httpapi

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.json
var openAPISpec []byte

// OpenAPISpec returns the embedded OpenAPI document so other servers can serve the same contract.
func OpenAPISpec() []byte { return openAPISpec }

// handleOpenAPI serves the spec unauthenticated; it carries no tenant data and SDK generators fetch it.
func (s *Server) handleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(openAPISpec)
}
