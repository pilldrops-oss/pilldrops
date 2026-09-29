package orchestrator

// A sandbox acting as itself. A pd_sb_ bearer resolves to one sandbox row
// (never cached: destroy deletes the row and the next request anywhere is a
// 401) and is admitted only to the routes below. Everything else answers 403
// naming the principal kind, including any route added later.

import (
	"net/http"
	"strings"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
)

// principalSandboxRole is the ctx role a sandbox principal carries.
const principalSandboxRole = "sandbox"

// authSandbox resolves a pd_sb_ bearer. handled=false means the token is not
// a sandbox credential at all. Deliberately not memoised in authc.
func (s *Server) authSandbox(w http.ResponseWriter, r *http.Request, next http.HandlerFunc, tok string) (handled bool) {
	if !strings.HasPrefix(tok, sbtoken.IdentityPrefix) {
		return false
	}
	st := s.st.Load()
	if st == nil {
		writeErr(w, 401, "missing or invalid bearer token")
		return true
	}
	rec, ok, err := st.SandboxByIdentityHash(r.Context(), sbtoken.HashHex(tok))
	if err != nil || !ok {
		writeErr(w, 401, "missing or invalid bearer token")
		return true
	}
	if !sandboxPrincipalAllowed(r.Method, r.URL.Path, rec.ID) {
		writeErr(w, 403, "principal is a sandbox ("+rec.ID+"): this route is not available to a sandbox identity")
		return true
	}
	// The sandbox is also the key below the org, and the signed assertion
	// carries that name to the node. Leaving it unnamed evaluated every
	// ownership question against an empty key, which is the one caller a
	// private sandbox refuses — so a sandbox could not read, exec or list
	// itself, and a private fork child was refused by the identity it had
	// just been issued.
	ctx := withKey(r.Context(), sandbox.SelfKeyID(rec.ID), principalSandboxRole)
	next(w, r.WithContext(withOrg(ctx, rec.OrgID)))
	return true
}

// sandboxPrincipalAllowed is the allowlist, by method and path shape so it
// runs before routing and covers routes that do not exist yet: everything on
// the sandbox itself; and within the org, sandbox create, list, get, exec,
// file copy, fork and port listing. ponytail: org-wide exec/cp/fork is the
// boxd model; narrow with a lineage column if a customer needs self-only.
func sandboxPrincipalAllowed(method, path, self string) bool {
	path = strings.TrimSuffix(path, "/")
	switch path {
	case "/v1/sandboxes":
		return method == http.MethodGet || method == http.MethodPost
	case "/v1/whoami":
		return method == http.MethodGet
	}
	rest, ok := strings.CutPrefix(path, "/v1/sandboxes/")
	if !ok || rest == "" {
		return false
	}
	id, sub, _ := strings.Cut(rest, "/")
	if id == self {
		return true
	}
	switch sub {
	case "":
		return method == http.MethodGet
	case "exec", "exec/stream":
		return method == http.MethodPost
	case "files":
		return method == http.MethodGet || method == http.MethodPost
	case "fork":
		return method == http.MethodPost
	case "ports":
		return method == http.MethodGet
	}
	return false
}

// handleWhoami names the caller: what principal it is and which org it acts for.
func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	out := map[string]string{"org": fastOrg(r), "principal": "key"}
	keyID, keyRole := callerKey(r.Context())
	switch {
	case isOperator(r):
		out["principal"] = "operator"
	case keyRole == principalSandboxRole:
		out["principal"] = "sandbox"
		out["sandbox_id"] = strings.TrimPrefix(keyID, sandbox.SelfKeyPrefix)
	}
	writeJSON(w, 200, out)
}
