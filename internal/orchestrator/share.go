package orchestrator

// Public links to an app on a developer's own machine. This file is the API
// and its token; shareingress.go holds the tunnels and sharepublic.go serves
// visitors. The API runs on every replica. One replica, SHARE_INGRESS=1, holds
// the tunnels, following FORWARD_INGRESS.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"

	"github.com/pilldrops-oss/pilldrops/internal/auth/previewtoken"
	"github.com/pilldrops-oss/pilldrops/internal/config"
	"github.com/pilldrops-oss/pilldrops/internal/proxy"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

const (
	shareDefaultTTL = 8 * time.Hour
	shareMaxTTL     = 24 * time.Hour
	shareTokenTTL   = 5 * time.Minute
	// shareStaleAfter is how long an unseen share keeps its slot in the org limit.
	shareStaleAfter = 2 * time.Minute
	shareNameTries  = 5
)

// Plain words only: nothing that reads as a brand, a bank or a login page.
var (
	shareAdjectives = []string{"amber", "brisk", "calm", "dusty", "eager", "gentle", "hazel", "jolly",
		"keen", "lively", "mellow", "nimble", "olive", "quiet", "rustic", "sunny"}
	shareNouns = []string{"badger", "cedar", "dune", "ember", "finch", "grove", "heron", "iris",
		"juniper", "kestrel", "lark", "maple", "otter", "pine", "quail", "reef"}
)

// shareDomain is SHARE_DOMAIN as set. Only a local dev run gives it a port.
func shareDomain() string { return strings.TrimSpace(os.Getenv("SHARE_DOMAIN")) }

// shareHost is the domain as a request's host is compared against it.
func shareHost() string { return proxy.NormalizeHost(shareDomain()) }

// ShareIngressEnabled reports whether this replica holds the tunnels.
func ShareIngressEnabled() bool {
	v := os.Getenv("SHARE_INGRESS")
	return v == "1" || v == "true" || v == "yes"
}

// shareLinksOn reports whether this deployment can serve a link. As with
// forwards, "remote" is the operator's word that another replica is the ingress.
func shareLinksOn() bool {
	if shareDomain() == "" {
		return false
	}
	return ShareIngressEnabled() || strings.EqualFold(strings.TrimSpace(os.Getenv("SHARE_INGRESS")), "remote")
}

func shareScheme() string {
	if devInsecure() {
		return "http"
	}
	return "https"
}

func shareLinkURL(name string) string { return shareScheme() + "://" + name + "." + shareDomain() }

func shareTunnelURL() string {
	if devInsecure() {
		return "ws://" + shareDomain() + "/tunnel"
	}
	return "wss://" + shareDomain() + "/tunnel"
}

// ShareDomainGuardError refuses a share domain that overlaps the platform's. A
// reported phishing link must not take the dashboard's domain with it, and a
// customer's app must not be able to set cookies on it.
func ShareDomainGuardError(sandboxDomain, portalOrigin string) error {
	share := shareHost()
	if share == "" {
		return nil
	}
	others := []string{proxy.NormalizeHost(strings.TrimSpace(sandboxDomain))}
	if u, err := url.Parse(strings.TrimSpace(portalOrigin)); err == nil {
		others = append(others, strings.ToLower(u.Hostname()))
	}
	for _, other := range others {
		if other == "" {
			continue
		}
		if share == other || strings.HasSuffix(share, "."+other) || strings.HasSuffix(other, "."+share) ||
			sameRegistrableDomain(share, other) {
			return fmt.Errorf("SHARE_DOMAIN %q overlaps %q: share links must be on a domain of their own", share, other)
		}
	}
	return nil
}

// sameRegistrableDomain is false for a host with no registrable domain, such as localhost or an IP.
func sameRegistrableDomain(a, b string) bool {
	if net.ParseIP(a) != nil || net.ParseIP(b) != nil {
		return false
	}
	ra, errA := publicsuffix.EffectiveTLDPlusOne(a)
	rb, errB := publicsuffix.EffectiveTLDPlusOne(b)
	return errA == nil && errB == nil && ra == rb
}

func (s *Server) shareTokenKey() []byte { return previewtoken.BrowserKey("share:" + s.apiKey) }

// mintShareToken signs one share's right to open its control channel. The
// preview token's construction under its own key, so neither verifies as the other.
func (s *Server) mintShareToken(id string) string {
	return previewtoken.BrowserAt(s.shareTokenKey(), id, 1, "tunnel", s.now().Add(shareTokenTTL))
}

func (s *Server) validShareToken(tok, id string) bool {
	if s.apiKey == "" && !devInsecure() {
		return false
	}
	return previewtoken.BrowserValidAt(s.shareTokenKey(), tok, id, 1, "tunnel", s.now())
}

func shareRandomPick(words []string) (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(words))))
	if err != nil {
		return "", err
	}
	return words[n.Int64()], nil
}

func newShareName() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	adjective, err := shareRandomPick(shareAdjectives)
	if err != nil {
		return "", err
	}
	noun, err := shareRandomPick(shareNouns)
	if err != nil {
		return "", err
	}
	suffix := make([]byte, 8)
	for i := range suffix {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		suffix[i] = alphabet[n.Int64()]
	}
	return adjective + "-" + noun + "-" + string(suffix), nil
}

func newShareID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sh_" + hex.EncodeToString(b), nil
}

// registerShareRoutes mounts the API. Sandbox identities are already refused
// by sandboxPrincipalAllowed, which admits no path under /v1/shares.
func (s *Server) registerShareRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/shares", s.auth(s.handleShareCreate))
	mux.HandleFunc("POST /v1/shares/{id}/token", s.auth(s.handleShareToken))
	mux.HandleFunc("DELETE /v1/shares/{id}", s.auth(s.handleShareDelete))
	mux.HandleFunc("POST /v1/admin/shares/{name}/block", s.authOperator(s.handleShareBlock))
}

func (s *Server) shareJSON(sh store.Share) map[string]any {
	return map[string]any{
		"id": sh.ID, "name": sh.Name, "url": shareLinkURL(sh.Name),
		"tunnel_url": shareTunnelURL(), "tunnel_token": s.mintShareToken(sh.ID),
		"expires_at": sh.ExpiresAt,
	}
}

func (s *Server) handleShareCreate(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	if !shareLinksOn() {
		writeJSONErr(w, http.StatusNotImplemented, "no_share_ingress",
			"share links need SHARE_DOMAIN set and one orchestrator replica started with SHARE_INGRESS=1 "+
				"(and SHARE_INGRESS=remote on the API-only replicas)")
		return
	}
	var body struct {
		TTLSeconds int64 `json:"ttl_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	ttl := shareDefaultTTL
	if body.TTLSeconds != 0 {
		ttl = time.Duration(body.TTLSeconds) * time.Second
	}
	if ttl <= 0 || ttl > shareMaxTTL {
		writeErr(w, http.StatusBadRequest, `"ttl_seconds" must be between 1 and 86400`)
		return
	}
	now, org := s.now(), fastOrg(r)
	if limit := config.Int("SHARE_MAX_PER_ORG", 5); limit > 0 {
		n, err := st.CountActiveShares(r.Context(), org, now.Unix(), now.Add(-shareStaleAfter).Unix())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if n >= limit {
			writeJSONErr(w, http.StatusTooManyRequests, "share_limit",
				fmt.Sprintf("this org already has %d active share links, the limit; stop one first", n))
			return
		}
	}
	keyID, _ := callerKey(r.Context())
	sh := store.Share{OrgID: org, CreatedBy: keyID, CreatedAt: now.Unix(), ExpiresAt: now.Add(ttl).Unix()}
	var err error
	for range shareNameTries {
		if sh.ID, err = newShareID(); err != nil {
			break
		}
		if sh.Name, err = newShareName(); err != nil {
			break
		}
		if err = st.InsertShare(r.Context(), sh); err == nil {
			writeJSON(w, http.StatusCreated, s.shareJSON(sh))
			return
		}
	}
	writeErr(w, http.StatusInternalServerError, err.Error())
}

// handleShareToken lets a reconnecting CLI back in. 410 tells it to stop trying.
func (s *Server) handleShareToken(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	sh, ok, err := st.ShareByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok || sh.OrgID != fastOrg(r) {
		writeErr(w, http.StatusNotFound, "share not found")
		return
	}
	if state := sh.State(s.now().Unix()); state != "open" {
		writeJSON(w, http.StatusGone, map[string]string{
			"error": "this share link has ended: " + state, "code": "share_ended", "reason": state,
		})
		return
	}
	writeJSON(w, http.StatusOK, s.shareJSON(sh))
}

func (s *Server) handleShareDelete(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	id := r.PathValue("id")
	ok, err := st.CloseShare(r.Context(), fastOrg(r), id, s.now().Unix())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "share not found")
		return
	}
	if sh, found, err := st.ShareByID(r.Context(), id); err == nil && found {
		s.shares.end(sh.Name, "closed")
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "closed"})
}

func (s *Server) handleShareBlock(w http.ResponseWriter, r *http.Request) {
	st := s.projectStore(w)
	if st == nil {
		return
	}
	name := r.PathValue("name")
	sh, ok, err := st.BlockShare(r.Context(), name, s.now().Unix())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "share not found")
		return
	}
	s.shares.end(sh.Name, "blocked")
	writeJSON(w, http.StatusOK, map[string]string{"name": sh.Name, "status": "blocked"})
}
