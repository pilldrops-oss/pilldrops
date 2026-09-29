package httpapi

import (
	"net/http"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/previewtoken"
	"github.com/pilldrops-oss/pilldrops/internal/proxy"
)

// previewVerifiesAsym reports which secret signs the way in here: with an
// org-assert public key the node verifies Ed25519 and can only verify, because
// the control plane holds the private half. It is one fact read by both halves
// below — the minter and the verifier — so they cannot drift into signing with
// one secret and checking with another.
func (s *Server) previewVerifiesAsym() bool { return len(s.orgAssertPub) > 0 }

// previewToken mints the org-bound ?ptok= for a sandbox port, so a standalone
// node can embed one without an orchestrator. Returns "" when previewSecret is
// unset, and when this node verifies asymmetrically: the HMAC form is the only
// one it can mint and checkPreviewAuth would refuse it, so embedding one in a
// URL hands the caller a link that cannot open. On a fleet node the control
// plane in front mints the credential the caller actually uses.
func (s *Server) previewToken(id string, port int, org string) string {
	if s.previewVerifiesAsym() {
		return ""
	}
	return previewtoken.Token(s.previewSecret, id, port, org)
}

// previewAuthResult is checkPreviewAuth's verdict; the path form maps each
// refusal to its own status.
type previewAuthResult int

const (
	previewAuthOK previewAuthResult = iota
	previewAuthUnconfigured
	previewAuthNotFound
	previewAuthForbidden
)

// checkPreviewAuth is shared by the path and subdomain preview forms. Fails
// closed without previewSecret except under devInsecure(). The token is checked
// against the sandbox's real owner org, which is returned; an empty org is a refusal.
func (s *Server) checkPreviewAuth(id string, port int, token string) (previewAuthResult, string) {
	if len(s.previewSecret) == 0 {
		if devInsecure() {
			if sb, exists := s.d.Get(id); exists {
				return previewAuthOK, sb.OrgID
			}
			return previewAuthNotFound, ""
		}
		return previewAuthUnconfigured, ""
	}
	sb, exists := s.d.Get(id)
	if !exists {
		return previewAuthNotFound, ""
	}
	// No dual-accept: falling back to HMAC when the signature fails would
	// restore the forgery the asymmetric form removes.
	if s.previewVerifiesAsym() {
		if previewtoken.AuthorizedAsym(s.orgAssertPub, token, id, port, sb.OrgID, time.Now()) {
			return previewAuthOK, sb.OrgID
		}
		return previewAuthForbidden, ""
	}
	if previewtoken.Authorized(s.previewSecret, token, id, port, sb.OrgID, false) {
		return previewAuthOK, sb.OrgID
	}
	return previewAuthForbidden, ""
}

// requirePreviewToken gates the public /preview/ proxy so knowing a sandbox id
// and port is not enough to reach another tenant's app. throttleByIP stays
// outermost so refused requests are still rate limited.
func (s *Server) requirePreviewToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, port, _, ok := proxy.ParsePreviewPath(r.URL.Path)
		if !ok {
			// Let the proxy produce its own 400 for a malformed path.
			next.ServeHTTP(w, r)
			return
		}
		verdict, org := s.checkPreviewAuth(id, port, r.URL.Query().Get("ptok"))
		switch verdict {
		case previewAuthOK:
			if org == "" {
				writeErr(w, http.StatusForbidden, "invalid or missing preview token")
				return
			}
			// The verified org is what makes daemon.PreviewTarget fail closed.
			next.ServeHTTP(w, r.WithContext(proxy.WithPreviewOrg(r.Context(), org)))
		case previewAuthUnconfigured:
			writeErr(w, http.StatusServiceUnavailable, "preview gating unavailable: no preview secret configured")
		case previewAuthNotFound:
			writeErr(w, http.StatusNotFound, "sandbox not found")
		default:
			writeErr(w, http.StatusForbidden, "invalid or missing preview token")
		}
	})
}

// VerifyPreview is checkPreviewAuth for proxy.HostRouter's subdomain form,
// passed as a func value because internal/proxy cannot import this package.
// ok is false whenever org would be empty.
func (s *Server) VerifyPreview(id string, port int, token string) (string, bool) {
	verdict, org := s.checkPreviewAuth(id, port, token)
	if verdict != previewAuthOK || org == "" {
		return "", false
	}
	return org, true
}

// throttlePreview is throttleByIP for the preview proxy, except on a clustered
// node for a request carrying a valid control-plane hop token. That request
// came through an edge or API replica, whose one address stands for every
// visitor of every sandbox on this node, so a per-IP bucket here capped the
// whole node's preview traffic at PREVIEW_RATE_RPS per front-door replica
// (measured: 50 rps). The front door limits per visitor instead.
func (s *Server) throttlePreview(next http.Handler) http.Handler {
	limited := s.throttleByIP(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Clustered() {
			if id, port, _, ok := proxy.ParsePreviewPath(r.URL.Path); ok {
				if v, _ := s.checkPreviewAuth(id, port, r.URL.Query().Get("ptok")); v == previewAuthOK {
					next.ServeHTTP(w, r)
					return
				}
			}
		}
		limited.ServeHTTP(w, r)
	})
}
