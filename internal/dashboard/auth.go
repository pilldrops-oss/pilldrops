package dashboard

import (
	"net/http"
	"strings"
)

// sessionCookie is the same-origin cookie webui sets; the browser attaches it
// to EventSource calls, which cannot carry an Authorization header. The name
// must match webui's.
const sessionCookie = "sandboxd_session"

// requireAuth rejects anonymous callers. It accepts a key from the
// Authorization: Bearer header or the session cookie.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			if c, err := r.Cookie(sessionCookie); err == nil {
				token = c.Value
			}
		}
		if token == "" {
			writeAuthErr(w, "missing session cookie or bearer token")
			return
		}
		if _, err := s.st.ValidateKey(r.Context(), token); err != nil {
			writeAuthErr(w, "invalid session")
			return
		}
		next(w, r)
	}
}

func bearerToken(r *http.Request) string {
	raw := r.Header.Get("Authorization")
	if tok := strings.TrimPrefix(raw, "Bearer "); tok != raw {
		return tok
	}
	return ""
}

func writeAuthErr(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"` + msg + `"}`))
}
