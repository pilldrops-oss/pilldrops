//go:build !core

package httpapi

import (
	"net/http"
	"os"
	"strings"
)

// handleChatLinkCode mints a one-time code that connects a messaging account to this sandbox.
// The code is a credential: single-use, short TTL, owner-only, audited.
func (s *Server) handleChatLinkCode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	var body struct {
		// ReadOnly links may report but never write or execute.
		ReadOnly bool `json:"read_only"`
		// Autonomous grants the agent its --dangerously-* flags on this link. Default off.
		Autonomous bool `json:"autonomous"`
	}
	// An absent body means a read-write link.
	_ = decodeBody(r, &body)

	code, err := s.st.CreateLinkCode(r.Context(), id, orgID(r), body.ReadOnly, body.Autonomous)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not create a link code")
		return
	}
	s.auditDetail(r, "sandbox.chat_link_code", id, map[string]any{
		"read_only": body.ReadOnly, "autonomous": body.Autonomous,
	})

	resp := map[string]any{
		"code":         code,
		"read_only":    body.ReadOnly,
		"autonomous":   body.Autonomous,
		"telegram_url": telegramDeepLink(code),
		"expires_in":   "10m",
	}
	writeJSON(w, http.StatusOK, resp)
}

// telegramDeepLink builds t.me/<bot>?start=<code>. Empty when no bot username
// is configured; the code still works with /link.
func telegramDeepLink(code string) string {
	bot := strings.TrimPrefix(strings.TrimSpace(os.Getenv("TELEGRAM_BOT_USERNAME")), "@")
	if bot == "" {
		return ""
	}
	return "https://t.me/" + bot + "?start=" + code
}

// handleChatLinkDelete revokes every messaging link to this sandbox. Idempotent.
func (s *Server) handleChatLinkDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	if err := s.st.DeleteChatLinksForSandbox(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not revoke links")
		return
	}
	s.auditDetail(r, "sandbox.chat_link_revoke", id, map[string]any{})
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}
