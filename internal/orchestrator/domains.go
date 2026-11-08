package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/publicsuffix"

	"github.com/pilldrops-oss/pilldrops/internal/cron"
	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// Custom domains: a customer hostname in front of one public sandbox, or a
// wildcard in front of all of the org's. The row is written here, verified by
// the background verifier, and read by the ingress stage and by the TLS
// terminator's ask endpoint. Nothing routes, and no certificate is issued,
// until the verifier has seen the DNS record.

// defaultDomainPort is the app port a host serves when the caller names none.
const defaultDomainPort = 8000

// registerDomainRoutes mounts /v1/domains. The check endpoint is deliberately
// unauthenticated: the TLS terminator asks it during a handshake, before any
// customer credential exists.
func (s *Server) registerDomainRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/domains", s.auth(s.handleDomainCreate))
	mux.HandleFunc("GET /v1/domains", s.auth(s.handleDomainList))
	mux.HandleFunc("DELETE /v1/domains/{host}", s.auth(s.handleDomainDelete))
	mux.HandleFunc("GET /v1/domains/check", s.handleDomainCheck)
}

// domainTarget is the CNAME record the customer must create: the sandbox's own
// platform host for an exact row, the platform wildcard itself for a wildcard
// row. Empty when the deployment has no PORTAL_SANDBOX_DOMAIN, which is what
// makes the whole feature unavailable there.
func (s *Server) domainTarget(d store.Domain) string {
	if s.sandboxDomain == "" {
		return ""
	}
	if d.Wildcard {
		return s.sandboxDomain
	}
	return strconv.Itoa(d.Port) + "-" + d.SandboxID + "." + s.sandboxDomain
}

// domainLegacyTargets are the records a row was told to create under each
// legacy sandbox domain, which the verifier still accepts for a while.
func (s *Server) domainLegacyTargets(d store.Domain) []string {
	if s.legacy == nil {
		return nil
	}
	out := make([]string, 0, len(s.legacy.Domains))
	for _, old := range s.legacy.Domains {
		if d.Wildcard {
			out = append(out, old)
		} else {
			out = append(out, strconv.Itoa(d.Port)+"-"+d.SandboxID+"."+old)
		}
	}
	return out
}

// StartDomainVerifier runs the DNS verifier until ctx is cancelled. Every
// replica runs it: the checks are idempotent, and a leader-only job would stop
// verifying whenever the lease moved. Without a sandbox domain there is no
// record to verify, so it does not start.
func (s *Server) StartDomainVerifier(ctx context.Context, st *store.Store) {
	if s.sandboxDomain == "" || st == nil {
		return
	}
	v := cron.NewDomainVerifier(st, s.domainTarget)
	v.Now, v.Log, v.LegacyTargets = s.now, s.log(), s.domainLegacyTargets
	go v.Run(ctx)
}

// cnameRecord is the copy-paste line the CLI and dashboard show.
func cnameRecord(host, target string) string {
	return strings.TrimPrefix(host, "*.") + " CNAME " + target
}

// validDomainHost normalizes and checks a customer hostname: lower-cased, no
// trailing dot, RFC 1123 labels, at least two of them, optionally a leading
// "*." for a wildcard. Refused rather than stored, because an unparseable host
// would be a row nothing can ever verify or route.
func validDomainHost(raw string) (host string, wildcard bool, err error) {
	host = strings.ToLower(strings.TrimSpace(raw))
	host = strings.TrimSuffix(host, ".")
	if host, wildcard = strings.CutPrefix(host, "*."); wildcard {
		host = "*." + host
	}
	bare := strings.TrimPrefix(host, "*.")
	if bare == "" || len(bare) > 253 {
		return "", false, errors.New(`"host" must be a hostname like app.example.com`)
	}
	labels := strings.Split(bare, ".")
	if len(labels) < 2 {
		return "", false, errors.New(`"host" must have at least two labels, like app.example.com`)
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return "", false, errors.New("host label " + strconv.Quote(l) + " is not a DNS label")
		}
		for i := 0; i < len(l); i++ {
			c := l[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return "", false, errors.New("host label " + strconv.Quote(l) + " is not a DNS label")
			}
		}
	}
	return host, wildcard, nil
}

// platformParent returns the platform domain host is, or sits under: the
// sandbox domain and, once previews moved, a legacy one or the dashboard's
// registrable domain. Those hosts are ours: a row for one would let an org
// claim another org's sandbox host, or a name like login.<dashboard domain>
// while old wildcard DNS still points here. "" when host is none of them.
func (s *Server) platformParent(host string) string {
	host = strings.TrimPrefix(host, "*.")
	parents := []string{strings.ToLower(strings.TrimSpace(s.sandboxDomain))}
	if s.legacy != nil {
		parents = append(parents, s.legacy.Domains...)
		if u, err := url.Parse(s.portalOrigin); err == nil && u.Hostname() != "" {
			if reg, err := publicsuffix.EffectiveTLDPlusOne(strings.ToLower(u.Hostname())); err == nil {
				parents = append(parents, reg)
			}
		}
	}
	for _, d := range parents {
		if d != "" && (host == d || strings.HasSuffix(host, "."+d)) {
			return d
		}
	}
	return ""
}

func (s *Server) handleDomainCreate(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	if s.sandboxDomain == "" {
		writeErr(w, http.StatusNotImplemented,
			"custom domains need PORTAL_SANDBOX_DOMAIN set on the orchestrator: without it there is no "+
				"CNAME target to point at")
		return
	}
	var body struct {
		Host      string `json:"host"`
		SandboxID string `json:"sandbox_id"`
		Port      int    `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	host, wildcard, err := validDomainHost(body.Host)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if parent := s.platformParent(host); parent != "" {
		writeErr(w, http.StatusBadRequest, "hosts under "+parent+" belong to the platform and cannot be attached")
		return
	}

	d := store.Domain{Host: host, OrgID: fastOrg(r), Wildcard: wildcard, Port: body.Port, CreatedAt: s.now().Unix()}
	if wildcard {
		if body.SandboxID != "" {
			writeErr(w, http.StatusBadRequest,
				`a wildcard host addresses every sandbox by label, so it takes no "sandbox_id"`)
			return
		}
		d.Port = 0
	} else if !s.prepareSandboxDomain(w, r, &d, body.SandboxID) {
		return
	}

	switch err := st.PutDomain(r.Context(), d); {
	case errors.Is(err, store.ErrDomainTaken):
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": host + " is already attached to another organisation's sandbox; remove it there first. " +
				"A claim that never verifies stops holding the host after " +
				strconv.Itoa(store.DomainClaimGrace/(24*60*60)) + " days.",
			"code": "host_taken",
		})
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	row, _, _ := st.GetDomain(r.Context(), host)
	writeJSON(w, http.StatusCreated, s.domainJSON(row))
}

// prepareSandboxDomain fills in and checks the per-sandbox half of a row:
// ownership, the public-preview requirement and the port the node can serve.
// It writes the refusal and returns false when the row must not be stored.
func (s *Server) prepareSandboxDomain(w http.ResponseWriter, r *http.Request, d *store.Domain, sandboxID string) bool {
	if sandboxID == "" {
		writeErr(w, http.StatusBadRequest, `"sandbox_id" is required (or a "*." host for an org-wide wildcard)`)
		return false
	}
	// 404, never 403: a 403 would confirm the id exists for someone else.
	if !s.callerOwnsSandbox(r, sandboxID) {
		writeErr(w, http.StatusNotFound, "not found")
		return false
	}
	d.SandboxID = sandboxID
	if d.Port == 0 {
		d.Port = defaultDomainPort
	}
	if d.Port < 1 || d.Port > 65535 {
		writeErr(w, http.StatusBadRequest, `"port" must be 1-65535`)
		return false
	}
	// The preview gate never opens these ports without the owner's token, so a
	// host on one would refuse every visitor.
	if rt.IsPlatformPort(d.Port) {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "port " + strconv.Itoa(d.Port) + " is the sandbox's terminal or desktop, which gives shell access " +
				"and always needs an access token from the owner, so a custom host on it would refuse every visitor. " +
				"Attach the host to the port your app serves.",
			"code": "platform_port",
		})
		return false
	}
	// A private sandbox behind a customer host would answer 403 without a
	// ?token=, which makes the host useless and the sharing model incoherent.
	if !s.isPublicPreview(r.Context(), sandboxID) {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "sandbox " + sandboxID + " is private, so a custom host would refuse every visitor without " +
				"a ?token=. Recreate it with \"public\": true (or make its preview public) and attach the host again.",
			"code": "sandbox_not_public",
		})
		return false
	}
	// A single-port runtime publishes one host port; any other port would 502.
	if node, ok := s.reg.NodeFor(sandboxID); ok && node.Runtime == "docker" {
		published := defaultDomainPort
		if st := s.st.Load(); st != nil {
			if row, found, err := st.GetActiveSandboxRow(r.Context(), sandboxID); err == nil && found && row.PublicPort != 0 {
				published = row.PublicPort
			}
		}
		if d.Port != published {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "sandbox " + sandboxID + " runs on a single-port runtime and publishes port " +
					strconv.Itoa(published) + ", so port " + strconv.Itoa(d.Port) + " has nothing behind it",
				"code": "port_not_servable",
			})
			return false
		}
	}
	return true
}

// domainJSON is the wire shape: the row plus the record the customer must create.
func (s *Server) domainJSON(d store.Domain) map[string]any {
	target := s.domainTarget(d)
	return map[string]any{
		"host": d.Host, "sandbox_id": d.SandboxID, "port": d.Port, "wildcard": d.Wildcard,
		"status": d.Status, "error": d.Error, "created_at": d.CreatedAt,
		"cname_target": target,
		"record":       cnameRecord(d.Host, target),
		// The row's DNS still names a legacy sandbox domain: record is the one to create.
		"legacy_target": d.LegacyTargetSeenAt != 0,
	}
}

func (s *Server) handleDomainList(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	rows, err := st.ListDomainsByOrg(r.Context(), fastOrg(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, d := range rows {
		out = append(out, s.domainJSON(d))
	}
	writeJSON(w, http.StatusOK, map[string]any{"domains": out})
}

func (s *Server) handleDomainDelete(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	host, _, err := validDomainHost(r.PathValue("host"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	// Org-scoped in the statement: another org's host answers 404, not 403.
	ok, err := st.DeleteDomain(r.Context(), fastOrg(r), host)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	s.domains.forget(host)
	writeJSON(w, http.StatusOK, map[string]string{"host": host, "status": "removed"})
}

// handleDomainCheck is the TLS terminator's on-demand ask. It answers 200 only
// for a host the verifier has seen resolve to us; every other answer is 404, so
// a stranger pointing arbitrary names at the deployment cannot make us ask a
// certificate authority for a certificate and burn the rate limit for everyone.
func (s *Server) handleDomainCheck(w http.ResponseWriter, r *http.Request) {
	// Unauthenticated, so bounded per peer: a handshake flood is otherwise a
	// store read per connection.
	if !s.hbLimiter.allow(httpx.ClientIP(r)) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	host := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("host")))
	host = strings.TrimSuffix(host, ".")
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" || s.resolveCustomHost(r, host) == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"host": host, "status": "active"})
}
