package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pilldrops-oss/pilldrops/internal/auth/authcache"
	"github.com/pilldrops-oss/pilldrops/internal/auth/orgassert"
	"github.com/pilldrops-oss/pilldrops/internal/httpx"
)

// devInsecure reports whether SANDBOXD_DEV=1 or DEV_INSECURE=1 is set, which
// lets an operator key pass a bare unsigned X-Org-Override. Local development only.
// Duplicated from cmd/sandboxd because importing nodeboot would cycle.
func devInsecure() bool {
	return envBool("SANDBOXD_DEV", false) || envBool("DEV_INSECURE", false)
}

type ctxKey string

const orgIDKey ctxKey = "orgID"
const roleKey ctxKey = "role"
const keyIDKey ctxKey = "keyID"

// principalRoleKey is the role of the key the *customer* holds, which on the
// fleet path is not roleKey: there the credential on the wire is the
// orchestrator's operator key, and only the signed assertion says who is
// really calling. Visibility is the one decision that must use this and not
// roleKey, or every request through the control plane would be an operator's.
const principalRoleKey ctxKey = "principalRole"

// sessionCookie mirrors internal/webui.SessionCookie; importing webui would embed the SPA.
const sessionCookie = "sandboxd_session"

// auth wraps a handler with Bearer-token authentication. The session cookie is
// accepted too because EventSource cannot set an Authorization header.
func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the two pre-identity failures below draw down the budget, so
		// authenticated clients are never throttled here.
		ip := httpx.ClientIP(r)
		if s.authFailLimiter.blocked(ip) {
			w.Header().Set("Retry-After", "1")
			writeErr(w, http.StatusTooManyRequests, "too many failed authentication attempts")
			return
		}
		raw := r.Header.Get("Authorization")
		token := strings.TrimPrefix(raw, "Bearer ")
		if token == "" || token == raw {
			token = ""
			if c, err := r.Cookie(sessionCookie); err == nil {
				token = c.Value
			}
		}
		if token == "" {
			s.metrics.incAuthFailure()
			s.authFailLimiter.allow(ip)
			writeErr(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		hash := authcache.Key(token)
		key, ok := s.authc.Get(hash)
		if !ok {
			var err error
			key, err = s.st.ValidateKey(r.Context(), token)
			if err != nil {
				s.metrics.incAuthFailure()
				s.authFailLimiter.allow(ip)
				writeErr(w, http.StatusUnauthorized, "invalid api key")
				return
			}
			s.authc.Put(hash, key)
		}
		// In a fleet, tenant traffic must come through the control plane: capacity
		// checks here are node-local, so a tenant hitting nodes directly would get
		// their concurrency cap once per node. The orchestrator forwards with an
		// operator key, so role is the discriminator. Operators may still hit a
		// node directly; SANDBOXD_ALLOW_DIRECT_TENANT_API=1 reopens it for tenants.
		if s.Clustered() && key.Role != "operator" && !s.allowDirectTenantAPI {
			s.metrics.incAuthFailure()
			writeErr(w, http.StatusForbidden,
				"this node is part of a fleet: send API requests to the control plane, not to a node directly")
			return
		}
		// An operator key may act as another org via a signed, time-bound
		// X-Org-Override. A present-but-invalid override is a 403, never a
		// fallback to the key's own org.
		org := key.OrgID
		// assertedKey is the customer key the control plane says is calling,
		// honoured only once the assertion that binds it verifies. A bare
		// X-Key-Id on a request that carries no assertion is nobody.
		assertedKey, assertedRole := "", ""
		// overrode records that this request acts for an org other than the
		// key's own, so attribution comes from the assertion, not the wire key.
		overrode := false
		if key.Role == "operator" {
			if ov := r.Header.Get("X-Org-Override"); ov != "" {
				// Ed25519 when a public key is configured, HMAC otherwise, never
				// both: accepting both would let a holder of the shared secret
				// forge past the asymmetric check. nodeID binds the assertion
				// to this node.
				verify := func(principal ...string) (string, string, bool) {
					if len(s.orgAssertPub) > 0 {
						return orgassert.VerifyAsym(s.orgAssertPub, ov, s.nodeID, r.Method, r.URL.Path, time.Now(), principal...)
					}
					return orgassert.Verify(s.orgOverrideSecret, ov, s.nodeID, r.Method, r.URL.Path, time.Now(), principal...)
				}
				verifyEither := func() (string, string, bool) {
					sentKey, sentRole := r.Header.Get("X-Key-Id"), r.Header.Get("X-Key-Role")
					if sentKey == "" {
						return verify()
					}
					if vOrg, nonce, ok := verify(sentKey, sentRole); ok {
						assertedKey, assertedRole = sentKey, sentRole
						return vOrg, nonce, ok
					}
					// The assertion does not cover the key id, so the header is
					// somebody's addition: verify the request on its own merits
					// and attribute it to no one.
					return verify()
				}
				switch verifiedOrg, nonce, ok := verifyEither(); {
				case ok && nonce == "" && requireSecureHop():
					// Pre-nonce assertions are replayable for their whole life.
					// Tolerated by default so nodes can upgrade before the control plane.
					s.metrics.incAuthFailure()
					writeErr(w, http.StatusForbidden,
						"org override carries no nonce and this node requires replay-protected assertions "+
							"(upgrade the control plane, or unset PILLDROPS_REQUIRE_SECURE_HOP)")
					return
				case ok && !s.assertNonces.use(nonce):
					// Valid but already used: a replay.
					s.metrics.incAuthFailure()
					writeErr(w, http.StatusForbidden, "org override replayed: this assertion has already been used")
					return
				case ok:
					org, overrode = verifiedOrg, true
				case len(s.orgOverrideSecret) == 0 && devInsecure():
					// Nothing is signed on this path, so the caller names itself:
					// dev only, and no weaker than the bare org it sits beside.
					assertedKey, assertedRole = r.Header.Get("X-Key-Id"), r.Header.Get("X-Key-Role")
					org, overrode = ov, true
				default:
					s.metrics.incAuthFailure()
					writeErr(w, http.StatusForbidden, "invalid, expired, or unsigned org override")
					return
				}
			}
		}
		// Throttle per effective org so one tenant can't exhaust the node.
		if !s.limiter.allow(org) {
			w.Header().Set("Retry-After", "1")
			writeErr(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		keyID := key.ID
		// Acting for another org: the key on the wire is the control plane's
		// and belongs to a different org, so the caller is whoever the
		// assertion named — and nobody at all when it named no one. The
		// header alone never gets here; it is honoured only once verified.
		if overrode {
			keyID = assertedKey
		}
		// Without an override the caller is who the node authenticated: a
		// direct tenant, or an operator acting for its own org. With one, the
		// role is the asserted one — and an override that names no role is a
		// plain member, never an operator, so a control plane that does not
		// send one yet hides private sandboxes rather than exposing them.
		principalRole := key.Role
		if overrode {
			principalRole = assertedRole
		}
		ctx := context.WithValue(r.Context(), orgIDKey, org)
		ctx = context.WithValue(ctx, roleKey, key.Role)
		ctx = context.WithValue(ctx, keyIDKey, keyID)
		ctx = context.WithValue(ctx, principalRoleKey, principalRole)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// authAdmin is auth plus an operator-role gate.
func (s *Server) authAdmin(next http.HandlerFunc) http.Handler {
	return s.auth(func(w http.ResponseWriter, r *http.Request) {
		if roleOf(r) != "operator" {
			writeErr(w, http.StatusForbidden, "operator role required")
			return
		}
		next(w, r)
	})
}

// orgID pulls the authenticated org from the request context ("default" if absent).
func orgID(r *http.Request) string {
	if v, ok := r.Context().Value(orgIDKey).(string); ok && v != "" {
		return v
	}
	return "default"
}

// roleOf pulls the authenticated key's role from the request context.
func roleOf(r *http.Request) string {
	if v, ok := r.Context().Value(roleKey).(string); ok {
		return v
	}
	return ""
}

// operatorPrincipal reports whether the key the caller actually holds is an
// operator: its own role when it authenticated at this node, the asserted one
// when the control plane forwarded on its behalf.
func operatorPrincipal(r *http.Request) bool {
	if v, ok := r.Context().Value(principalRoleKey).(string); ok {
		return v == "operator"
	}
	return roleOf(r) == "operator"
}

// keyIDOf pulls the authenticated key's ID from the request context.
func keyIDOf(r *http.Request) string {
	if v, ok := r.Context().Value(keyIDKey).(string); ok {
		return v
	}
	return ""
}

// audit records an action best-effort; an audit-write failure never fails the request.
func (s *Server) audit(r *http.Request, action, target string) {
	_ = s.st.RecordAudit(r.Context(), orgID(r), keyIDOf(r), action, target)
}

// Commands are truncated to maxAuditCmdBytes; if the blob still exceeds
// maxAuditDetailBytes, optional fields are dropped rather than cutting JSON mid-way.
const (
	maxAuditDetailBytes = 4096
	maxAuditCmdBytes    = 2048
)

// auditDetail records an action with a JSON detail payload, best-effort.
// It mutates detail, so callers must not reuse the map.
func (s *Server) auditDetail(r *http.Request, action, target string, detail map[string]any) {
	for _, k := range []string{"cmd", "start_cmd"} {
		if v, ok := detail[k].(string); ok && len(v) > maxAuditCmdBytes {
			// Cut on a rune boundary so the value stays valid UTF-8.
			cut := maxAuditCmdBytes
			for cut > 0 && !utf8.RuneStart(v[cut]) {
				cut--
			}
			detail[k] = v[:cut]
		}
	}
	b, err := json.Marshal(detail)
	if err != nil {
		_ = s.st.RecordAuditDetail(r.Context(), orgID(r), keyIDOf(r), action, target, "{}")
		return
	}
	for _, drop := range []string{"files", "setup", "env_keys"} {
		if len(b) <= maxAuditDetailBytes {
			break
		}
		delete(detail, drop)
		if nb, err := json.Marshal(detail); err == nil {
			b = nb
		}
	}
	if len(b) > maxAuditDetailBytes {
		b = []byte(`{}`)
	}
	_ = s.st.RecordAuditDetail(r.Context(), orgID(r), keyIDOf(r), action, target, string(b))
}

// envKeys strips values from "NAME=VALUE" entries so audit detail never stores secrets.
func envKeys(env []string) []string {
	keys := make([]string, 0, len(env))
	for _, e := range env {
		if name, _, found := strings.Cut(e, "="); found {
			if name != "" {
				keys = append(keys, name)
			}
		} else if e != "" {
			keys = append(keys, e)
		}
	}
	return keys
}
