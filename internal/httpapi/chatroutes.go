//go:build !core

package httpapi

import "net/http"

// registerChatRoutes mounts web chat. Chat is an agent front-end (agentd or the
// Telegram bridge), so a core build 404s it rather than accepting and failing.
func (s *Server) registerChatRoutes(route func(string, http.Handler)) {
	route("POST /v1/sandboxes/{id}/chat", s.auth(s.handleChatSend))
	route("GET /v1/sandboxes/{id}/chat", s.auth(s.handleChatHistory))
	// Chat-link codes are redeemed by the Telegram bridge, which the core lacks,
	// so they live here too. GET .../channels stays in the core: it only reports.
	route("POST /v1/sandboxes/{id}/chat-link", s.auth(s.handleChatLinkCode))
	route("DELETE /v1/sandboxes/{id}/chat-link", s.auth(s.handleChatLinkDelete))
}
