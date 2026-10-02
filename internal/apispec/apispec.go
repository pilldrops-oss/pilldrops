// Package apispec embeds the HTTP API's OpenAPI document, which a node and the
// control plane both serve.
package apispec

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.json
var spec []byte

// JSON returns the embedded OpenAPI document.
func JSON() []byte { return spec }

// Serve serves the spec unauthenticated; it carries no tenant data and SDK
// generators fetch it.
func Serve(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(spec)
}
