package orchestrator

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// frameAncestorsPolicy returns the CSP directive for previews. With a preview
// domain the dashboard must be allowed to embed them; without one previews
// share the dashboard origin and nothing may frame them.
func frameAncestorsPolicy(sandboxDomain, portalOrigin string) string {
	if sandboxDomain == "" {
		return "frame-ancestors 'none'"
	}
	policy := "frame-ancestors 'self' https://" + sandboxDomain + " https://*." + sandboxDomain
	if portalOrigin != "" {
		policy += " " + portalOrigin
	}
	return policy
}

// PortalOriginGuardError refuses a PORTAL_ORIGIN that is not one bare origin: it
// goes into a response header, so anything else would rewrite the framing policy.
// Plain HTTP is for local development only.
func PortalOriginGuardError(origin string) error {
	if origin == "" {
		return nil
	}
	u, err := url.Parse(origin)
	valid := err == nil && u.Host != "" && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && u.User == nil &&
		!strings.ContainsAny(origin, " ;,'\"") &&
		(u.Scheme == "https" || (u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1")))
	if !valid {
		return fmt.Errorf("PORTAL_ORIGIN %q must be one origin such as https://app.example.com", origin)
	}
	return nil
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
func setPreviewGuards(h http.Header, sandboxDomain, portalOrigin string) {
	h.Set("Content-Security-Policy", frameAncestorsPolicy(sandboxDomain, portalOrigin))
	// X-Frame-Options cannot express an allow-list, so DENY only in the deny case.
	if sandboxDomain == "" {
		h.Set("X-Frame-Options", "DENY")
	} else {
		h.Del("X-Frame-Options")
	}
	// The preview URL carries ?token= and ?ptok=; a Referer would leak them.
	h.Set("Referrer-Policy", "no-referrer")

}
