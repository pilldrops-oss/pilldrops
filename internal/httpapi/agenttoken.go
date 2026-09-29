//go:build !core

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/aat"
	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	"github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// Agent Access Tokens (AATs) over HTTP: minting, listing, revoking, rotating,
// and the middleware that accepts one on an agent route. An org key holds every
// scope; an AAT is bound to one sandbox and narrowed to a scope set.
//
// Enforcement order is security-relevant: validity, sandbox binding, org
// ownership, agent kind, scope, rate limit. Everything before scope returns 404
// so a caller cannot confirm a sandbox exists; scope failures return 403 and
// name the missing scope.

// agentTokenCookie carries an AAT for a browser that cannot set a header
// (EventSource, an img, a plain link). Must stay distinct from sessionCookie.
const agentTokenCookie = "sandboxd_agent"

// AgentTokenHeader carries an AAT across a proxy hop, where Authorization
// already holds the per-hop operator key. When present it decides the request
// and a bad value fails it: falling back to Authorization would upgrade a
// share-link holder to full org authority.
const AgentTokenHeader = "X-Agent-Token"

// AgentSourceHeader lets an org-key caller attribute a turn. Ignored for delegated tokens.
const AgentSourceHeader = "X-Agent-Source"

const (
	codeAgentScopeRequired = "agent_scope_required"
	codeAgentTokenExpired  = "agent_token_expired"
)

type aatCtxKeyType string

const aatCtxKey aatCtxKeyType = "aat"

// agentTokenOf returns the AAT that authorised this request; absent means an org key did.
func agentTokenOf(r *http.Request) (store.AgentToken, bool) {
	t, ok := r.Context().Value(aatCtxKey).(store.AgentToken)
	return t, ok
}

// actorOf names who authorised a request, for audit and for a turn's source field.
func actorOf(r *http.Request) string {
	if t, ok := agentTokenOf(r); ok {
		return "token:" + t.ID
	}
	if id := keyIDOf(r); id != "" {
		return "key:" + id
	}
	return "unknown"
}

// agentSource labels where a turn came from, for the turn record and the UI.
func agentSource(r *http.Request) string {
	if t, ok := agentTokenOf(r); ok {
		// A share-link holder cannot disclaim what they submitted.
		return "token:" + t.ID
	}
	// Allowlisted, not free text: the value is journaled and shown in a UI.
	switch src := r.Header.Get(AgentSourceHeader); src {
	case "telegram", "discord", "cron", "dashboard", "chat":
		return src
	}
	return "api"
}

// authAgentScope authenticates an agent route and enforces one scope.
// It replaces s.auth rather than wrapping it, since s.auth would 401 an AAT.
func (s *Server) authAgentScope(scope aat.Scope, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// AgentTokenHeader, when set, is exclusive; see its doc comment.
		raw, delegated := r.Header.Get(AgentTokenHeader), false
		if raw != "" {
			delegated = true
		} else {
			raw = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if raw == r.Header.Get("Authorization") {
				raw = "" // no Bearer prefix present
			}
			if raw == "" {
				if c, err := r.Cookie(agentTokenCookie); err == nil {
					raw = c.Value
				}
			}
		}

		if delegated && !strings.HasPrefix(raw, aat.Prefix) {
			// Refused here so it can never fall through to the org-key middleware.
			s.metrics.incAuthFailure()
			writeErr(w, http.StatusUnauthorized, "invalid agent access token")
			return
		}

		if !strings.HasPrefix(raw, aat.Prefix) {
			s.auth(func(w http.ResponseWriter, r *http.Request) {
				// An org key holds every agent scope; it can destroy the sandbox anyway.
				next(w, r)
			}).ServeHTTP(w, r)
			return
		}

		tok, err := s.st.ValidateAgentToken(r.Context(), raw)
		switch {
		case errors.Is(err, store.ErrAgentTokenExpired):
			// Safe to distinguish from invalid: reaching here required the correct secret.
			s.metrics.incAuthFailure()
			writeErrCode(w, http.StatusUnauthorized, codeAgentTokenExpired,
				"this agent access token has expired; ask for a new link")
			return
		case err != nil:
			// Unknown, revoked, malformed: one answer for all three.
			s.metrics.incAuthFailure()
			writeErr(w, http.StatusUnauthorized, "invalid agent access token")
			return
		}

		// Binding, ownership, kind: all 404, indistinguishable from "no such sandbox".
		if id := r.PathValue("id"); id == "" || id != tok.SandboxID {
			writeErrCode(w, http.StatusNotFound, codeAgentNotSupported, "sandbox not found")
			return
		}
		sb, found := s.d.Get(tok.SandboxID)
		if !found || sb.OrgID != tok.OrgID {
			// Token outlived its sandbox, or the sandbox changed hands.
			writeErrCode(w, http.StatusNotFound, codeAgentNotSupported, "sandbox not found")
			return
		}
		if t, ok := templates.Resolve(sb.Template); !ok || t.Kind != templates.KindAgent {
			writeErrCode(w, http.StatusNotFound, codeAgentNotSupported, "sandbox not found")
			return
		}

		if !tok.Scopes.Has(scope) {
			writeErrCode(w, http.StatusForbidden, codeAgentScopeRequired,
				"this token does not have the "+string(scope)+" scope, which this action needs")
			return
		}

		// Per token, not per org, so a forwarded share link cannot spend the owner's budget.
		if !s.limiter.allow("aat:" + tok.ID) {
			w.Header().Set("Retry-After", "1")
			writeErr(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}

		s.st.TouchAgentToken(r.Context(), tok, httpx.ClientIP(r))

		ctx := context.WithValue(r.Context(), orgIDKey, tok.OrgID)
		// No role: an AAT is never an operator, so X-Org-Override never applies.
		ctx = context.WithValue(ctx, keyIDKey, "aat_"+tok.ID)
		ctx = context.WithValue(ctx, aatCtxKey, tok)
		next(w, r.WithContext(ctx))
	})
}

// Token management is org-key only: a credential that can issue its own
// successors is not revocable in practice.

type agentTokenJSON struct {
	ID        string   `json:"id"`
	SandboxID string   `json:"sandbox_id"`
	Scopes    []string `json:"scopes"`
	Label     string   `json:"label,omitempty"`
	CreatedBy string   `json:"created_by,omitempty"`
	CreatedAt string   `json:"created_at"`
	ExpiresAt string   `json:"expires_at"`
	RevokedAt string   `json:"revoked_at,omitempty"`
	// LastUsedAt is empty when never used.
	LastUsedAt string `json:"last_used_at,omitempty"`
	LastUsedIP string `json:"last_used_ip,omitempty"`
	Supersedes string `json:"supersedes,omitempty"`
	Active     bool   `json:"active"`
	// Secret appears only in a mint or rotate response; storage holds a hash.
	Secret string `json:"secret,omitempty"`
}

func toAgentTokenJSON(t store.AgentToken) agentTokenJSON {
	scopes := make([]string, 0, len(t.Scopes))
	for _, sc := range t.Scopes.Sorted() {
		scopes = append(scopes, string(sc))
	}
	j := agentTokenJSON{
		ID: t.ID, SandboxID: t.SandboxID, Scopes: scopes, Label: t.Label,
		CreatedBy:  t.CreatedBy,
		CreatedAt:  t.CreatedAt.UTC().Format(time.RFC3339),
		ExpiresAt:  t.ExpiresAt.UTC().Format(time.RFC3339),
		Supersedes: t.Supersedes,
		Active:     t.Active(time.Now()),
	}
	if !t.RevokedAt.IsZero() {
		j.RevokedAt = t.RevokedAt.UTC().Format(time.RFC3339)
	}
	if !t.LastUsedAt.IsZero() {
		j.LastUsedAt = t.LastUsedAt.UTC().Format(time.RFC3339)
		j.LastUsedIP = t.LastUsedIP
	}
	return j
}

// handleAgentTokenCreate mints a token: POST /v1/sandboxes/{id}/agent/tokens.
func (s *Server) handleAgentTokenCreate(w http.ResponseWriter, r *http.Request) {
	id, tmpl, ok := s.agentGate(r)
	if !ok {
		writeErrCode(w, http.StatusNotFound, codeAgentNotSupported, "sandbox not found")
		return
	}
	var req struct {
		Scopes     []string `json:"scopes"`
		Label      string   `json:"label"`
		TTLSeconds int64    `json:"ttl_seconds"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return
	}
	scopes := make([]aat.Scope, 0, len(req.Scopes))
	for _, s := range req.Scopes {
		scopes = append(scopes, aat.Scope(strings.TrimSpace(s)))
	}
	set, err := aat.NewSet(scopes)
	if err != nil {
		// Unknown scopes are rejected, not dropped, so nothing shows as granted but behaves as denied.
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	tok, secret, err := s.st.CreateAgentToken(r.Context(), store.AgentTokenRequest{
		SandboxID: id, OrgID: orgID(r), Scopes: set, Label: req.Label,
		CreatedBy: keyIDOf(r), TTL: time.Duration(req.TTLSeconds) * time.Second,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.auditDetail(r, "agent.token.mint", id, map[string]any{
		"token_id": tok.ID, "scopes": set.Encode(), "agent": tmpl.ID,
		"expires_at": tok.ExpiresAt.UTC().Format(time.RFC3339),
	})

	out := toAgentTokenJSON(tok)
	out.Secret = secret
	writeJSON(w, http.StatusCreated, map[string]any{
		"token": out,
		"note":  "This secret is shown once and cannot be retrieved again. Store it now.",
	})
}

// handleAgentTokenList lists tokens: GET /v1/sandboxes/{id}/agent/tokens.
func (s *Server) handleAgentTokenList(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.agentGate(r)
	if !ok {
		writeErrCode(w, http.StatusNotFound, codeAgentNotSupported, "sandbox not found")
		return
	}
	rows, err := s.st.ListAgentTokens(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]agentTokenJSON, 0, len(rows))
	for _, t := range rows {
		out = append(out, toAgentTokenJSON(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out, "scopes": scopeCatalog()})
}

// scopeCatalog describes every scope so a UI can render a permission chooser.
func scopeCatalog() []map[string]string {
	out := make([]map[string]string, 0, len(aat.All))
	for _, sc := range aat.All {
		out = append(out, map[string]string{
			"scope": string(sc), "description": aat.Describe(sc),
		})
	}
	return out
}

// handleAgentTokenRevoke revokes one: DELETE /v1/sandboxes/{id}/agent/tokens/{tokenID}.
func (s *Server) handleAgentTokenRevoke(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.agentGate(r)
	if !ok {
		writeErrCode(w, http.StatusNotFound, codeAgentNotSupported, "sandbox not found")
		return
	}
	tokenID := r.PathValue("tokenID")
	if err := s.st.RevokeAgentToken(r.Context(), id, tokenID); err != nil {
		writeErr(w, http.StatusNotFound, "no such token for this sandbox")
		return
	}
	s.auditDetail(r, "agent.token.revoke", id, map[string]any{"token_id": tokenID})
	writeJSON(w, http.StatusOK, map[string]any{"revoked": tokenID})
}

// handleAgentTokenRotate replaces a secret: POST .../agent/tokens/{tokenID}/rotate.
func (s *Server) handleAgentTokenRotate(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.agentGate(r)
	if !ok {
		writeErrCode(w, http.StatusNotFound, codeAgentNotSupported, "sandbox not found")
		return
	}
	tokenID := r.PathValue("tokenID")
	fresh, secret, err := s.st.RotateAgentToken(r.Context(), id, tokenID, keyIDOf(r))
	switch {
	case errors.Is(err, store.ErrAgentTokenRevoked):
		writeErr(w, http.StatusConflict,
			"this token was revoked; mint a new one instead of rotating it")
		return
	case errors.Is(err, store.ErrInvalidAgentToken):
		writeErr(w, http.StatusNotFound, "no such token for this sandbox")
		return
	case err != nil && fresh.ID == "":
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.auditDetail(r, "agent.token.rotate", id, map[string]any{
		"token_id": fresh.ID, "supersedes": tokenID,
	})
	out := toAgentTokenJSON(fresh)
	out.Secret = secret
	body := map[string]any{
		"token": out,
		"note": "The previous secret keeps working for a short overlap so in-flight " +
			"callers are not broken, then stops.",
	}
	if err != nil {
		// Partial success: the new token is usable, but the old one's expiry was not shortened.
		body["warning"] = err.Error()
	}
	writeJSON(w, http.StatusOK, body)
}

// A share link must not carry a durable credential in its URL (history,
// referrers, logs), so it carries a single-use code exchanged for an HttpOnly cookie.

// shareCodeTTL is short: the flow is "click the link you were just sent".
const shareCodeTTL = 10 * time.Minute

type shareCode struct {
	secret string // AAT plaintext this code hands over
	sbID   string
	exp    time.Time
}

// shareCodes holds pending codes in memory only: a live code is credential-equivalent.
// A node restart invalidates them, which for a ten-minute link is a re-share.
type shareCodes struct {
	mu sync.Mutex
	m  map[string]shareCode
}

func newShareCodes() *shareCodes { return &shareCodes{m: map[string]shareCode{}} }

func (c *shareCodes) put(code string, v shareCode) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Opportunistic sweep; codes are few and short-lived.
	now := time.Now()
	for k, e := range c.m {
		if now.After(e.exp) {
			delete(c.m, k)
		}
	}
	c.m[code] = v
}

// take consumes a code. It is deleted even when expired, so a code is single-use either way.
func (c *shareCodes) take(code string) (shareCode, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[code]
	if !ok {
		return shareCode{}, false
	}
	delete(c.m, code)
	if time.Now().After(v.exp) {
		return shareCode{}, false
	}
	return v, true
}

// handleAgentShare mints a read-scoped token and returns a one-time link:
// POST /v1/sandboxes/{id}/agent/share.
func (s *Server) handleAgentShare(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.agentGate(r)
	if !ok {
		writeErrCode(w, http.StatusNotFound, codeAgentNotSupported, "sandbox not found")
		return
	}
	var req struct {
		Scopes     []string `json:"scopes"`
		Label      string   `json:"label"`
		TTLSeconds int64    `json:"ttl_seconds"`
	}
	// An empty body is valid and means read-only.
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req)

	scopes := []aat.Scope{aat.ScopeRead}
	if len(req.Scopes) > 0 {
		scopes = scopes[:0]
		for _, sc := range req.Scopes {
			scopes = append(scopes, aat.Scope(strings.TrimSpace(sc)))
		}
	}
	set, err := aat.NewSet(scopes)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if set.Has(aat.ScopeConsole) {
		// A shell must not be reachable from a forwardable link.
		writeErr(w, http.StatusBadRequest,
			"a share link cannot carry console access; mint a console token explicitly instead")
		return
	}

	tok, secret, err := s.st.CreateAgentToken(r.Context(), store.AgentTokenRequest{
		SandboxID: id, OrgID: orgID(r), Scopes: set,
		Label: shareLabel(req.Label), CreatedBy: keyIDOf(r),
		TTL: time.Duration(req.TTLSeconds) * time.Second,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	code := aat.RandomCode()
	s.shares.put(code, shareCode{secret: secret, sbID: id, exp: time.Now().Add(shareCodeTTL)})

	s.auditDetail(r, "agent.share", id, map[string]any{
		"token_id": tok.ID, "scopes": set.Encode(),
	})
	writeJSON(w, http.StatusCreated, map[string]any{
		"token": toAgentTokenJSON(tok),
		// Sandbox-scoped form: the proxy finds the owning node from the {id} in the path.
		"claim_url":  "/v1/sandboxes/" + id + "/agent/claim?code=" + code,
		"expires_in": int(shareCodeTTL.Seconds()),
		"note": "The link works once. Opening it sets a cookie and the address bar is " +
			"left with no credential in it.",
	})
}

func shareLabel(l string) string {
	if strings.TrimSpace(l) == "" {
		return "share link"
	}
	return l
}

// handleAgentClaim exchanges a one-time code for a cookie: GET /agent/claim?code=...
// Unauthenticated: the single-use code is the credential.
func (s *Server) handleAgentClaim(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	v, ok := s.shares.take(code)
	if !ok {
		// One message for expired, unknown and used, so a stale link reveals nothing.
		writeErr(w, http.StatusUnauthorized, "this link has already been used or has expired")
		return
	}
	// A path/code mismatch cannot grant the wrong sandbox (the cookie is for v.sbID)
	// but would burn the code and land the recipient where it does not apply.
	if id := r.PathValue("id"); id != "" && id != v.sbID {
		writeErr(w, http.StatusUnauthorized, "this link has already been used or has expired")
		return
	}
	// On the per-sandbox agent origin the whole origin is this sandbox, so "/" is
	// fine. On the shared API host the cookie must stay pinned to this sandbox's subtree.
	cookiePath, dest := "/v1/sandboxes/"+v.sbID+"/agent", "/sandboxes/"+v.sbID+"?view=agent"
	if onAgentOrigin(r, v.sbID) {
		cookiePath, dest = "/", "/"
	}
	http.SetCookie(w, &http.Cookie{
		Name:     agentTokenCookie,
		Value:    v.secret,
		Path:     cookiePath,
		HttpOnly: true,
		// Not unconditionally Secure, so local http development still works.
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(24 * time.Hour),
	})
	// The destination carries no credential.
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// onAgentOrigin reports whether the request arrived on the sandbox's own agent
// origin. Host is attacker-controlled, so it is compared for equality against
// the id the code names; a prefix check would let a forged Host loosen the cookie.
func onAgentOrigin(r *http.Request, sandboxID string) bool {
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	label, _, found := strings.Cut(strings.ToLower(host), ".")
	return found && label == "agent-"+strings.ToLower(sandboxID)
}
