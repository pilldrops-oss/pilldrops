package orchestrator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/httpx"
)

// sourceCacheTTL bounds how stale a UDP forward's copy of the source list may
// be. Nothing else caches it.
const sourceCacheTTL = 30 * time.Second

// Reachability: an org may limit which source networks reach its previews.
// A peer outside the list gets the answer a nonexistent sandbox gets, so the
// public path never confirms that a restricted org's sandbox exists.

// registerPreviewSourceRoutes mounts the operator-only source-CIDR settings.
// Operator-only for now: the gate gives no protection against a peer that can
// reach a node's preview port directly, so the dashboard does not offer it yet.
func (s *Server) registerPreviewSourceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/admin/orgs/{org}/preview-sources", s.auth(s.handlePreviewSourcesGet))
	mux.HandleFunc("PUT /v1/admin/orgs/{org}/preview-sources", s.auth(s.handlePreviewSourcesPut))
}

func (s *Server) handlePreviewSourcesGet(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil || !s.requireOperator(w, r) {
		return
	}
	cidrs, err := st.OrgPreviewSources(r.Context(), r.PathValue("org"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if cidrs == nil {
		cidrs = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"org": r.PathValue("org"), "cidrs": cidrs})
}

func (s *Server) handlePreviewSourcesPut(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil || !s.requireOperator(w, r) {
		return
	}
	var body struct {
		CIDRs []string `json:"cidrs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	for _, c := range body.CIDRs {
		if _, err := netip.ParsePrefix(strings.TrimSpace(c)); err != nil {
			// Named in the message: a typo in one entry would otherwise lock the
			// org's previews out with no clue which entry did it.
			writeErr(w, http.StatusBadRequest, "preview source "+c+" is not a CIDR (want 100.64.0.0/10)")
			return
		}
	}
	if err := st.SetOrgPreviewSources(r.Context(), r.PathValue("org"), body.CIDRs); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := map[string]any{"org": r.PathValue("org"), "cidrs": body.CIDRs}
	// Said here as well as at boot: this is the moment the operator can act on it.
	if warn := trustedProxyWarning(); warn != "" && len(body.CIDRs) > 0 {
		s.log().WarnContext(r.Context(), warn, "component", "orchestrator", "org", r.PathValue("org"))
		out["warning"] = warn
	}
	writeJSON(w, http.StatusOK, out)
}

// requireOperator answers 403 and returns false for a customer credential.
func (s *Server) requireOperator(w http.ResponseWriter, r *http.Request) bool {
	if isOperator(r) {
		return true
	}
	writeErr(w, http.StatusForbidden, "preview source networks are an operator setting")
	return false
}

// peerAllowed reports whether an HTTP request's peer may reach the org.
func (s *Server) peerAllowed(r *http.Request, org string) bool {
	return s.peerIPAllowed(r.Context(), org, httpx.ClientIP(r))
}

// peerIPAllowed is the gate itself, taking an address rather than a request so
// every way into a sandbox can ask it. The HTTP preview was its only caller for
// a while, which left a restricted org reachable from any network on a raw
// forwarded port — the restriction covered the one door with a lock on it and
// none of the others.
//
// Read per call, not cached: a restriction an operator sets must apply to the
// next connection, not to the one after a TTL. The UDP forward is the single
// exception, and it caches on its own listener rather than here — see
// udpPeerAllowed.
//
// No CIDRs (and any store error) means no restriction: this gate exists to
// narrow access for orgs that asked for it, never to fail closed because the
// database hiccuped.
func (s *Server) peerIPAllowed(ctx context.Context, org, ip string) bool {
	if org == "" {
		return true
	}
	return peerInCIDRs(ip, s.orgSourceCIDRs(ctx, org))
}

// peerInCIDRs is the match. An empty list is no restriction; an address that
// will not parse is refused, since it cannot be shown to be inside one.
func peerInCIDRs(ip string, cidrs []netip.Prefix) bool {
	if len(cidrs) == 0 {
		return true
	}
	peer, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	// Unmapped, so a v4-mapped v6 peer (what a dual-stack listener reports) is
	// matched by the v4 prefix an operator actually wrote.
	peer = peer.Unmap()
	return slices.ContainsFunc(cidrs, func(p netip.Prefix) bool { return p.Contains(peer) })
}

// orgSourceCIDRs reads and parses one org's source prefixes. Nil means no
// restriction, which is also what a store error returns — see peerIPAllowed.
func (s *Server) orgSourceCIDRs(ctx context.Context, org string) []netip.Prefix {
	st := s.st.Load()
	if st == nil || org == "" {
		return nil
	}
	// Fresh every call, as before; the cache only answers when the store fails,
	// so an outage keeps the org's last restriction instead of dropping it.
	v, err := s.pcache.get("src:"+org, 0, s.now(), func() (any, error) { return st.OrgPreviewSources(ctx, org) })
	raw, _ := v.([]string)
	if err != nil {
		s.log().ErrorContext(ctx, "preview source lookup failed, allowing the request",
			"component", "orchestrator", "org", org, "err", err)
		return nil
	}
	out := make([]netip.Prefix, 0, len(raw))
	for _, c := range raw {
		if p, err := netip.ParsePrefix(strings.TrimSpace(c)); err == nil {
			out = append(out, p.Masked())
		}
	}
	return out
}

// trustedProxyWarning is non-empty when a source restriction cannot do its job:
// with no TRUSTED_PROXY_CIDRS, clientIP ignores X-Forwarded-For and every
// request behind a TLS terminator presents the TERMINATOR's address. The gate
// then admits everyone or nobody, uniformly, and never discriminates by real
// client — which looks exactly like a working restriction until someone tests
// it from outside.
func trustedProxyWarning() string {
	if len(httpx.TrustedProxyCIDRs()) > 0 {
		return ""
	}
	return "preview source restrictions are configured but TRUSTED_PROXY_CIDRS is not: " +
		"X-Forwarded-For is ignored, so every request behind a TLS terminator presents the " +
		"terminator's own address and the restriction admits everyone or nobody. Set " +
		"TRUSTED_PROXY_CIDRS to the terminator's networks"
}

// WarnIfSourceGateCannotWork logs trustedProxyWarning at boot when any org has
// a restriction. Called once the store is attached.
func (s *Server) WarnIfSourceGateCannotWork(ctx context.Context) {
	st := s.st.Load()
	if st == nil {
		return
	}
	warn := trustedProxyWarning()
	if warn == "" {
		return
	}
	orgs, err := st.OrgsWithPreviewSources(ctx)
	if err != nil || len(orgs) == 0 {
		return
	}
	s.log().WarnContext(ctx, warn, "component", "orchestrator", "orgs", strings.Join(orgs, ","))
}
