package orchestrator

import "net/http"

// frameAncestorsPolicy returns the CSP directive for previews. With a preview
// domain the dashboard must be allowed to embed them; without one previews
// share the dashboard origin and nothing may frame them.
func frameAncestorsPolicy(sandboxDomain string) string {
	if sandboxDomain == "" {
		return "frame-ancestors 'none'"
	}
	return "frame-ancestors 'self' https://" + sandboxDomain + " https://*." + sandboxDomain
}

// previewGuardDomain is the domain the framing policy is written for. On a
// customer hostname it is empty, which denies framing outright: the dashboard
// never embeds a customer domain, so nothing needs the allow-list there.
func (s *Server) previewGuardDomain(r *http.Request) string {
	if onCustomHost(r) {
		return ""
	}
	return s.sandboxDomain
}

// setPreviewGuards overwrites the framing and referrer headers on a preview
// response so the sandboxed app cannot opt out. Both guards live in one
// function so no call site can remember one and forget the other.
func setPreviewGuards(h http.Header, sandboxDomain string) {
	h.Set("Content-Security-Policy", frameAncestorsPolicy(sandboxDomain))
	// X-Frame-Options cannot express an allow-list, so DENY only in the deny case.
	if sandboxDomain == "" {
		h.Set("X-Frame-Options", "DENY")
	} else {
		h.Del("X-Frame-Options")
	}
	// The preview URL carries ?token= and ?ptok=; a Referer would leak them.
	h.Set("Referrer-Policy", "no-referrer")

}
