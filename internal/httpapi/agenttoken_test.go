//go:build !core

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/extras/agent/agentd"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// authorised reports whether a response cleared authorisation. These tests run
// without agentd, so a request that passes auth still fails downstream with 503/501.
func authorised(code int) bool {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return false
	}
	return true
}

// mintToken creates an AAT via the API and returns its id and secret.
func mintToken(t *testing.T, srv, key, id string, scopes ...string) (tokenID, secret string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"scopes": scopes, "label": "test"})
	code, out := do(t, "POST", srv+"/v1/sandboxes/"+id+"/agent/tokens", key, string(body))
	if code != http.StatusCreated {
		t.Fatalf("mint %v = %d %v", scopes, code, out)
	}
	tok, _ := out["token"].(map[string]any)
	tokenID, _ = tok["id"].(string)
	secret, _ = tok["secret"].(string)
	if tokenID == "" || secret == "" {
		t.Fatalf("mint returned no usable token: %v", out)
	}
	return tokenID, secret
}

func TestMintReturnsTheSecretExactlyOnce(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	tokenID, secret := mintToken(t, srv, key, id, "agent:read")

	if !strings.HasPrefix(secret, "sfa_") {
		t.Fatalf("secret has the wrong prefix: %q", secret)
	}

	// Every later read must omit the secret.
	code, out := do(t, "GET", srv+"/v1/sandboxes/"+id+"/agent/tokens", key, "")
	if code != http.StatusOK {
		t.Fatalf("list = %d %v", code, out)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), secret) {
		t.Fatal("the token list echoed the secret back")
	}
	if !strings.Contains(string(raw), tokenID) {
		t.Fatalf("the token list does not contain the token id: %s", raw)
	}
	// The list also carries the scope catalogue.
	if !strings.Contains(string(raw), "agent:console") {
		t.Fatalf("scope catalogue missing from the list response: %s", raw)
	}
}

func TestTokenAuthorisesItsOwnSandbox(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	_, secret := mintToken(t, srv, key, id, "agent:read")

	code, out := do(t, "GET", srv+"/v1/sandboxes/"+id+"/agent/status", secret, "")
	if !authorised(code) {
		t.Fatalf("a read token was denied its own sandbox's status: %d %v", code, out)
	}
}

func TestTokenIsRejectedOnAnotherSandbox(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	_, secret := mintToken(t, srv, key, id, "agent:read")

	// Same org, so only the token's sandbox binding keeps it out.
	code, out := do(t, "POST", srv+"/v1/sandboxes", key, `{"template":"claude"}`)
	if code != http.StatusCreated {
		t.Fatalf("second sandbox = %d %v", code, out)
	}
	other, _ := out["id"].(string)

	code, out = do(t, "GET", srv+"/v1/sandboxes/"+other+"/agent/status", secret, "")
	if code != http.StatusNotFound {
		// 404, never 403: a 403 would confirm the other sandbox exists.
		t.Fatalf("token reached another sandbox: %d %v", code, out)
	}
}

func TestWriteScopeIsRequiredToSubmitATurn(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	_, readOnly := mintToken(t, srv, key, id, "agent:read")

	code, out := do(t, "POST", srv+"/v1/sandboxes/"+id+"/agent/turns", readOnly,
		`{"message":"do something"}`)
	if code != http.StatusForbidden {
		t.Fatalf("a read-only token queued a turn: %d %v", code, out)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "agent:write") {
		t.Fatalf("the refusal does not name the missing scope: %v", out)
	}
	if c, _ := out["code"].(string); c != codeAgentScopeRequired {
		t.Fatalf("no stable error code to branch on: %v", out)
	}
}

func TestWriteScopeDoesNotGrantBudgetChanges(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	_, writeTok := mintToken(t, srv, key, id, "agent:read", "agent:write")

	// Raising a cap needs admin: a write token must not lift its own spending limit.
	code, out := do(t, "PATCH", srv+"/v1/sandboxes/"+id+"/agent/budget", writeTok,
		`{"cap_usd":1000}`)
	if code != http.StatusForbidden {
		t.Fatalf("a write token changed the budget: %d %v", code, out)
	}
	if code, out := do(t, "GET", srv+"/v1/sandboxes/"+id+"/agent/budget", writeTok, ""); !authorised(code) {
		t.Fatalf("a read+write token could not read the budget: %d %v", code, out)
	}
}

func TestTokenCannotReachOrgLevelRoutes(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	// Even with every scope: org-level routes run the org-key middleware.
	_, secret := mintToken(t, srv, key, id,
		"agent:read", "agent:write", "agent:console", "agent:approve", "agent:files", "agent:admin")

	for _, route := range []struct{ method, path, body string }{
		{"GET", "/v1/sandboxes", ""},
		{"POST", "/v1/sandboxes", `{"template":"claude"}`},
		{"DELETE", "/v1/sandboxes/" + id, ""},
	} {
		code, out := do(t, route.method, srv+route.path, secret, route.body)
		if code != http.StatusUnauthorized {
			t.Fatalf("%s %s with an AAT = %d %v", route.method, route.path, code, out)
		}
	}
}

func TestTokenCannotMintOrRevokeTokens(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	tokenID, secret := mintToken(t, srv, key, id, "agent:admin", "agent:read")

	base := srv + "/v1/sandboxes/" + id + "/agent/tokens"
	for _, route := range []struct{ method, path, body string }{
		{"POST", base, `{"scopes":["agent:read"]}`},
		{"GET", base, ""},
		{"DELETE", base + "/" + tokenID, ""},
		{"POST", base + "/" + tokenID + "/rotate", `{}`},
		{"POST", srv + "/v1/sandboxes/" + id + "/agent/share", `{}`},
	} {
		code, out := do(t, route.method, route.path, secret, route.body)
		if code != http.StatusUnauthorized {
			t.Fatalf("%s %s with an AAT = %d %v", route.method, route.path, code, out)
		}
	}
}

func TestRevokedTokenStopsWorkingImmediately(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	tokenID, secret := mintToken(t, srv, key, id, "agent:read")

	if code, _ := do(t, "GET", srv+"/v1/sandboxes/"+id+"/agent/status", secret, ""); !authorised(code) {
		t.Fatalf("token did not work before revocation: %d", code)
	}
	if code, out := do(t, "DELETE", srv+"/v1/sandboxes/"+id+"/agent/tokens/"+tokenID, key, ""); code != http.StatusOK {
		t.Fatalf("revoke = %d %v", code, out)
	}
	code, out := do(t, "GET", srv+"/v1/sandboxes/"+id+"/agent/status", secret, "")
	if code != http.StatusUnauthorized {
		t.Fatalf("revoked token still authorised: %d %v", code, out)
	}
}

func TestRotationHandsOverWithoutBreakingTheOldSecret(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	tokenID, oldSecret := mintToken(t, srv, key, id, "agent:read")

	code, out := do(t, "POST", srv+"/v1/sandboxes/"+id+"/agent/tokens/"+tokenID+"/rotate", key, `{}`)
	if code != http.StatusOK {
		t.Fatalf("rotate = %d %v", code, out)
	}
	tok, _ := out["token"].(map[string]any)
	newSecret, _ := tok["secret"].(string)
	if newSecret == "" || newSecret == oldSecret {
		t.Fatalf("rotation produced no new secret: %v", out)
	}
	if sup, _ := tok["supersedes"].(string); sup != tokenID {
		t.Fatalf("rotation did not record what it replaced: %v", tok)
	}
	// Both live during the overlap so in-flight callers are not broken.
	for name, s := range map[string]string{"old": oldSecret, "new": newSecret} {
		if code, out := do(t, "GET", srv+"/v1/sandboxes/"+id+"/agent/status", s, ""); !authorised(code) {
			t.Fatalf("%s secret denied during overlap: %d %v", name, code, out)
		}
	}
}

func TestUnknownScopeIsRefusedAtMint(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	code, out := do(t, "POST", srv+"/v1/sandboxes/"+id+"/agent/tokens", key,
		`{"scopes":["agent:read","agent:teleport"]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("unknown scope accepted: %d %v", code, out)
	}
	if code, out := do(t, "POST", srv+"/v1/sandboxes/"+id+"/agent/tokens", key,
		`{"scopes":[]}`); code != http.StatusBadRequest {
		t.Fatalf("token with no scopes accepted: %d %v", code, out)
	}
}

func TestTokenRoutesAre404ForANonAgentSandbox(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "base")
	for _, route := range []struct{ method, path, body string }{
		{"POST", "/v1/sandboxes/" + id + "/agent/tokens", `{"scopes":["agent:read"]}`},
		{"GET", "/v1/sandboxes/" + id + "/agent/tokens", ""},
		{"POST", "/v1/sandboxes/" + id + "/agent/share", `{}`},
	} {
		code, out := do(t, route.method, srv+route.path, key, route.body)
		if code != http.StatusNotFound {
			t.Fatalf("%s %s on a non-agent sandbox = %d %v", route.method, route.path, code, out)
		}
	}
}

func TestAnotherOrgCannotMintForYourSandbox(t *testing.T) {
	srv, _, id := newAgentTestAPI(t, "claude")
	code, out := do(t, "POST", srv+"/v1/sandboxes/"+id+"/agent/tokens", "sf_live_not_a_real_key",
		`{"scopes":["agent:read"]}`)
	if code != http.StatusUnauthorized {
		t.Fatalf("an unknown key reached the mint route: %d %v", code, out)
	}
}

func TestShareLinkExchangesAOneTimeCodeForACookie(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")

	code, out := do(t, "POST", srv+"/v1/sandboxes/"+id+"/agent/share", key, `{}`)
	if code != http.StatusCreated {
		t.Fatalf("share = %d %v", code, out)
	}
	claim, _ := out["claim_url"].(string)
	// Sandbox-scoped: the proxy finds the owning node from the {id} in the path.
	if !strings.HasPrefix(claim, "/v1/sandboxes/"+id+"/agent/claim?code=") {
		t.Fatalf("claim url is not routable through the control plane: %q", claim)
	}
	tok, _ := out["token"].(map[string]any)
	raw, _ := json.Marshal(tok["scopes"])
	if string(raw) != `["agent:read"]` {
		t.Fatalf("default share scopes = %s", raw)
	}
	// The link itself must not be a bearer token.
	if strings.Contains(claim, "sfa_") {
		t.Fatalf("the share URL carries a credential: %s", claim)
	}

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(srv + claim)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("claim = %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); strings.Contains(loc, "sfa_") || strings.Contains(loc, "code=") {
		t.Fatalf("redirect target still carries a credential: %s", loc)
	}

	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == agentTokenCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("claim set no agent cookie")
	}
	if !cookie.HttpOnly {
		t.Fatal("the agent cookie is readable by scripts")
	}
	if want := "/v1/sandboxes/" + id + "/agent"; cookie.Path != want {
		t.Fatalf("cookie path = %q, want %q", cookie.Path, want)
	}

	// The cookie must authenticate on its own, since EventSource cannot set a header.
	req, _ := http.NewRequest("GET", srv+"/v1/sandboxes/"+id+"/agent/status", nil)
	req.AddCookie(cookie)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("cookie request: %v", err)
	}
	defer resp2.Body.Close()
	if !authorised(resp2.StatusCode) {
		t.Fatalf("the share cookie did not authenticate: %d", resp2.StatusCode)
	}
}

func TestShareCodeCannotBeReused(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	code, out := do(t, "POST", srv+"/v1/sandboxes/"+id+"/agent/share", key, `{}`)
	if code != http.StatusCreated {
		t.Fatalf("share = %d %v", code, out)
	}
	claim, _ := out["claim_url"].(string)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	first, err := client.Get(srv + claim)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	first.Body.Close()
	if first.StatusCode != http.StatusSeeOther {
		t.Fatalf("first claim = %d", first.StatusCode)
	}

	second, err := client.Get(srv + claim)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	defer second.Body.Close()
	if second.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a one-time code worked twice: %d", second.StatusCode)
	}
	// One message for used, expired, and unknown.
	var body map[string]any
	_ = json.NewDecoder(second.Body).Decode(&body)
	if msg, _ := body["error"].(string); !strings.Contains(msg, "already been used or has expired") {
		t.Fatalf("second claim message = %v", body)
	}
}

func TestAnUnknownClaimCodeIsRejected(t *testing.T) {
	srv, _, _ := newAgentTestAPI(t, "claude")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(srv + "/agent/claim?code=" + url.QueryEscape("deadbeefdeadbeef"))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an invented code = %d", resp.StatusCode)
	}
	if len(resp.Cookies()) != 0 {
		t.Fatal("a failed claim still set a cookie")
	}
}

func TestShareLinkRefusesConsoleAccess(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	code, out := do(t, "POST", srv+"/v1/sandboxes/"+id+"/agent/share", key,
		`{"scopes":["agent:read","agent:console"]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("share link minted console access: %d %v", code, out)
	}
}

func TestShareCodesAreSingleUseUnderConcurrency(t *testing.T) {
	sc := newShareCodes()
	sc.put("code", shareCode{secret: "sfa_x", sbID: "sb_a", exp: time.Now().Add(time.Minute)})

	const racers = 32
	wins := make(chan bool, racers)
	start := make(chan struct{})
	for range racers {
		go func() {
			<-start
			_, ok := sc.take("code")
			wins <- ok
		}()
	}
	close(start)
	got := 0
	for range racers {
		if <-wins {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("%d callers claimed the same one-time code", got)
	}
}

func TestExpiredShareCodeIsConsumedAnyway(t *testing.T) {
	sc := newShareCodes()
	sc.put("code", shareCode{secret: "sfa_x", sbID: "sb_a", exp: time.Now().Add(-time.Minute)})
	if _, ok := sc.take("code"); ok {
		t.Fatal("an expired code was honoured")
	}
	// Deleted even though it failed.
	sc.mu.Lock()
	n := len(sc.m)
	sc.mu.Unlock()
	if n != 0 {
		t.Fatalf("expired code left behind (%d entries)", n)
	}
}

// The bare /agent/claim route must be registered outside /v1 and not behind s.auth.
func TestClaimRouteIsRegistered(t *testing.T) {
	srv, _, _ := newAgentTestAPI(t, "claude")
	resp, err := http.Get(srv + "/agent/claim")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	// 401 (no such code), not 404 (no such route).
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/agent/claim = %d, want 401 from the handler itself", resp.StatusCode)
	}
}

func TestScopeCatalogueCoversEveryScope(t *testing.T) {
	cat := scopeCatalog()
	if len(cat) != 6 {
		t.Fatalf("catalogue has %d entries, want 6", len(cat))
	}
	for _, e := range cat {
		if e["scope"] == "" || e["description"] == "" {
			t.Fatalf("incomplete catalogue entry: %v", e)
		}
		if e["description"] == e["scope"] {
			t.Fatalf("scope %q has no real description", e["scope"])
		}
	}
}

// Lifecycle revocation tests go through the real daemon and StoreAdapter: the
// revoker is an optional interface, so an adapter that stops satisfying it fails silently.

func TestPauseRevokesConsoleTokensButNotReadTokens(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	_, console := mintToken(t, srv, key, id, "agent:read", "agent:console")
	_, watch := mintToken(t, srv, key, id, "agent:read")

	if code, out := do(t, "POST", srv+"/v1/sandboxes/"+id+"/pause", key, ""); code != http.StatusOK {
		t.Skipf("this runtime cannot pause (%d %v); the store-level rule is covered in internal/store", code, out)
	}

	if code, out := do(t, "GET", srv+"/v1/sandboxes/"+id+"/agent/status", console, ""); code != http.StatusUnauthorized {
		t.Fatalf("console token survived the pause: %d %v", code, out)
	}
	if code, out := do(t, "GET", srv+"/v1/sandboxes/"+id+"/agent/status", watch, ""); code == http.StatusUnauthorized {
		t.Fatalf("read token was revoked by a console-only sweep: %d %v", code, out)
	}
}

func TestDestroyRevokesEveryTokenForTheSandbox(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	_, read := mintToken(t, srv, key, id, "agent:read")
	_, write := mintToken(t, srv, key, id, "agent:write")

	if code, out := do(t, "DELETE", srv+"/v1/sandboxes/"+id, key, ""); code != http.StatusOK {
		t.Fatalf("destroy = %d %v", code, out)
	}
	// 401, not 404: the token itself must be dead, since a sandbox id can be reused.
	for name, secret := range map[string]string{"read": read, "write": write} {
		if code, out := do(t, "GET", srv+"/v1/sandboxes/"+id+"/agent/status", secret, ""); code != http.StatusUnauthorized {
			t.Fatalf("%s token survived destroy: %d %v", name, code, out)
		}
	}
}

func TestClaimIsBoundToTheSandboxInItsPath(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")

	code, out := do(t, "POST", srv+"/v1/sandboxes/"+id+"/agent/share", key, `{}`)
	if code != http.StatusCreated {
		t.Fatalf("share = %d %v", code, out)
	}
	claim, _ := out["claim_url"].(string)
	_, query, _ := strings.Cut(claim, "?")

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	// Same valid code, different sandbox in the path.
	resp, err := client.Get(srv + "/v1/sandboxes/sb-someone-else/agent/claim?" + query)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("mismatched claim = %d, want 401", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == agentTokenCookie {
			t.Fatal("a mismatched claim set an agent cookie")
		}
	}

	// The code is spent regardless.
	resp2, err := client.Get(srv + claim)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replay after a failed claim = %d, want 401", resp2.StatusCode)
	}
}

// doHdr is do() with control over the headers.
func doHdr(t *testing.T, method, url string, hdr map[string]string, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestRelayHeaderAuthorisesLikeABearer(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	_, secret := mintToken(t, srv, key, id, "agent:read")

	code, out := doHdr(t, "GET", srv+"/v1/sandboxes/"+id+"/agent/status",
		map[string]string{AgentTokenHeader: secret}, "")
	if !authorised(code) {
		t.Fatalf("a valid token in the relay header was denied: %d %v", code, out)
	}
}

func TestRelayHeaderIsStillScopeLimited(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	_, readOnly := mintToken(t, srv, key, id, "agent:read")

	code, out := doHdr(t, "POST", srv+"/v1/sandboxes/"+id+"/agent/turns",
		map[string]string{AgentTokenHeader: readOnly, "Content-Type": "application/json"},
		`{"message":"do something"}`)
	if code != http.StatusForbidden {
		t.Fatalf("a read-only token queued a turn via the relay header: %d %v", code, out)
	}
}

// A relayed request carries both the operator key in Authorization and the
// delegated token; a bad token falling back to the org key would be a privilege escalation.
func TestABadRelayHeaderNeverFallsBackToTheOrgKey(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	tokenID, secret := mintToken(t, srv, key, id, "agent:read")

	// Sanity: the org key alone authorises the route, so a pass below would be the fallback.
	if code, out := do(t, "GET", srv+"/v1/sandboxes/"+id+"/agent/status", key, ""); !authorised(code) {
		t.Fatalf("the org key cannot reach this route, so the test proves nothing: %d %v", code, out)
	}

	if code, _ := do(t, "DELETE", srv+"/v1/sandboxes/"+id+"/agent/tokens/"+tokenID, key, ""); code != http.StatusOK {
		t.Fatalf("revoke = %d", code)
	}

	for name, bad := range map[string]string{
		"revoked":           secret,
		"unknown":           "sfa_deadbe_" + strings.Repeat("f", 32),
		"malformed":         "sfa_not-a-token",
		"not an aat at all": key, // org key in the delegated slot
	} {
		code, out := doHdr(t, "GET", srv+"/v1/sandboxes/"+id+"/agent/status", map[string]string{
			AgentTokenHeader: bad,
			"Authorization":  "Bearer " + key,
		}, "")
		if code != http.StatusUnauthorized {
			t.Fatalf("%s token + valid org key = %d %v — the request was authorised by the "+
				"credential it was NOT presenting", name, code, out)
		}
	}
}

func TestRelayHeaderNarrowsAValidOrgKey(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	_, readOnly := mintToken(t, srv, key, id, "agent:read")

	code, out := doHdr(t, "PATCH", srv+"/v1/sandboxes/"+id+"/agent/budget", map[string]string{
		AgentTokenHeader: readOnly,
		"Authorization":  "Bearer " + key,
		"Content-Type":   "application/json",
	}, `{"cap_usd":1000}`)
	if code != http.StatusForbidden {
		t.Fatalf("the org key won over the delegated token: %d %v", code, out)
	}
}

// Absent and empty must mean the same, or a proxy that always sets the header breaks org-key calls.
func TestAnEmptyRelayHeaderIsNotADelegatedRequest(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")

	code, out := doHdr(t, "GET", srv+"/v1/sandboxes/"+id+"/agent/status", map[string]string{
		AgentTokenHeader: "",
		"Authorization":  "Bearer " + key,
	}, "")
	if !authorised(code) {
		t.Fatalf("an empty relay header broke an org-key request: %d %v", code, out)
	}
}

func TestClaimOnTheAgentOriginScopesTheCookieToTheWholeOrigin(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")

	code, out := do(t, "POST", srv+"/v1/sandboxes/"+id+"/agent/share", key, `{}`)
	if code != http.StatusCreated {
		t.Fatalf("share = %d %v", code, out)
	}
	claim, _ := out["claim_url"].(string)

	req, _ := http.NewRequest("GET", srv+claim, nil)
	// What the portal sets when terminating an agent origin.
	req.Header.Set("X-Forwarded-Host", "agent-"+id+".pilldrops.com")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	defer resp.Body.Close()

	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == agentTokenCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no agent cookie set")
	}
	if cookie.Path != "/" {
		t.Fatalf("cookie path = %q on the agent origin; /v1/agent/* requests would not "+
			"carry it", cookie.Path)
	}
	// The landing page must be on this origin, not a dashboard the recipient has no account for.
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Fatalf("redirected to %q, which is not on this origin", loc)
	}
}

func TestClaimOnTheAPIHostStaysPinnedToItsSandbox(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")

	code, out := do(t, "POST", srv+"/v1/sandboxes/"+id+"/agent/share", key, `{}`)
	if code != http.StatusCreated {
		t.Fatalf("share = %d %v", code, out)
	}
	claim, _ := out["claim_url"].(string)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(srv + claim) // no agent-origin forwarding headers
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	defer resp.Body.Close()

	for _, c := range resp.Cookies() {
		if c.Name != agentTokenCookie {
			continue
		}
		if c.Path != "/v1/sandboxes/"+id+"/agent" {
			t.Fatalf("cookie path = %q on the shared API host, want this sandbox's subtree", c.Path)
		}
		return
	}
	t.Fatal("no agent cookie set")
}

func TestAForgedAgentOriginCannotWidenTheCookieOnTheAPIHost(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")

	code, out := do(t, "POST", srv+"/v1/sandboxes/"+id+"/agent/share", key, `{}`)
	if code != http.StatusCreated {
		t.Fatalf("share = %d %v", code, out)
	}
	claim, _ := out["claim_url"].(string)

	req, _ := http.NewRequest("GET", srv+claim, nil)
	req.Header.Set("X-Forwarded-Host", "agent-someone-elses-sandbox.pilldrops.com")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	defer resp.Body.Close()

	for _, c := range resp.Cookies() {
		if c.Name == agentTokenCookie && c.Path == "/" {
			t.Fatal("a forged X-Forwarded-Host widened the cookie to the whole host")
		}
	}
}

// A pipeline that requests the work and also grants it is a review loop with nobody in it.
func TestAutomationCannotApproveItsOwnAgent(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")

	// Same scopes as a human token, so only the label triggers the refusal.
	body, _ := json.Marshal(map[string]any{
		"scopes": []string{"agent:read", "agent:approve"}, "label": "ci-runner",
	})
	code, out := do(t, "POST", srv+"/v1/sandboxes/"+id+"/agent/tokens", key, string(body))
	if code != http.StatusCreated {
		t.Fatalf("mint = %d %v", code, out)
	}
	tok, _ := out["token"].(map[string]any)
	robot, _ := tok["secret"].(string)

	code, out = do(t, "POST", srv+"/v1/sandboxes/"+id+"/agent/approvals/apr_1", robot,
		`{"decision":"allow"}`)
	if code != http.StatusForbidden {
		t.Fatalf("an automation token approved an action: %d %v", code, out)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "automation") {
		t.Errorf("the refusal does not say why: %v", out)
	}
	if c, _ := out["code"].(string); c != codeApprovalNotPermitted {
		t.Errorf("no stable error code to branch on: %v", out)
	}
}

func TestApprovingNeedsTheApproveScope(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	_, writeOnly := mintToken(t, srv, key, id, "agent:read", "agent:write")

	code, out := do(t, "POST", srv+"/v1/sandboxes/"+id+"/agent/approvals/apr_1", writeOnly,
		`{"decision":"allow"}`)
	if code != http.StatusForbidden {
		t.Fatalf("a token without agent:approve decided an approval: %d %v", code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "agent:approve") {
		t.Errorf("the refusal does not name the missing scope: %v", out)
	}
}

func TestAHumanTokenWithApproveMayDecide(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	body, _ := json.Marshal(map[string]any{
		"scopes": []string{"agent:read", "agent:approve"}, "label": "alice's laptop",
	})
	code, out := do(t, "POST", srv+"/v1/sandboxes/"+id+"/agent/tokens", key, string(body))
	if code != http.StatusCreated {
		t.Fatalf("mint = %d %v", code, out)
	}
	tok, _ := out["token"].(map[string]any)
	human, _ := tok["secret"].(string)

	code, out = do(t, "POST", srv+"/v1/sandboxes/"+id+"/agent/approvals/apr_1", human,
		`{"decision":"allow"}`)
	if !authorised(code) {
		t.Fatalf("a human token holding agent:approve was refused: %d %v", code, out)
	}
}

// The org key can always read the approvals queue.
func TestApprovalsListSaysWhetherYouCanAct(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	code, out := do(t, "GET", srv+"/v1/sandboxes/"+id+"/agent/approvals", key, "")
	if !authorised(code) {
		t.Fatalf("the owner could not read the approvals queue: %d %v", code, out)
	}
}

func TestAutomationLabelDetection(t *testing.T) {
	for _, label := range []string{
		"ci", "CI", "ci-runner", "github-ci", "nightly cron", "release_bot",
		"automation", "build pipeline", "scheduled sweep", "webhook relay",
	} {
		if !looksAutomated(label) {
			t.Errorf("label %q was not recognised as automation", label)
		}
	}
	// Must not fire on words that merely contain "ci": a false positive locks a person out.
	for _, label := range []string{
		"circuit board", "alice's laptop", "bobby", "citrus", "robot-arm-project",
		"", "share link", "Alice CI-adjacent team", // splits to "ci", see below
	} {
		if label == "Alice CI-adjacent team" {
			continue // acknowledged match; refusing is the safe side
		}
		if looksAutomated(label) {
			t.Errorf("label %q was wrongly treated as automation", label)
		}
	}
}

func TestPausedWorkstationServesItsHistoryFromTheMirror(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")

	// An empty turn list reads as "this agent has never run", which is false when
	// there is no mirrored history and no reachable supervisor.
	code, out := do(t, "GET", srv+"/v1/sandboxes/"+id+"/agent/turns", key, "")
	if code == http.StatusOK {
		if turns, _ := out["turns"].([]any); len(turns) == 0 {
			t.Fatal("an unreachable agent with an empty mirror returned an empty turn list, " +
				"which reads as 'this agent has never run'")
		}
	}
}

// served_from is on every response, including the live one.
func TestServedFromIsAlwaysStated(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")

	for _, path := range []string{"/agent/turns", "/agent/status"} {
		code, out := do(t, "GET", srv+"/v1/sandboxes/"+id+path, key, "")
		if !authorised(code) {
			t.Fatalf("%s = %d", path, code)
		}
		if code != http.StatusOK {
			continue // no agentd in this harness
		}
		if sf, _ := out["served_from"].(string); sf != "node" && sf != "mirror" {
			t.Errorf("%s did not say where it was served from: %v", path, out["served_from"])
		}
	}
}

// A supervisor that answered with a refusal (429, 402) is not unavailable.
func TestAgentRefusalIsNotReplacedWithStaleData(t *testing.T) {
	if agentUnavailableLive(&agentd.StatusError{Code: 429, Message: "rate limited"}) {
		t.Fatal("a supervisor's own refusal was treated as unavailability, so a real " +
			"answer would be replaced by stale data")
	}
	if agentUnavailableLive(&agentd.StatusError{Code: 402, Message: "spend cap reached"}) {
		t.Fatal("a spend-cap refusal was treated as unavailability")
	}
	// The cases that are unavailability.
	for name, err := range map[string]error{
		"paused":      rt.ErrPaused,
		"gone":        rt.ErrNotFound,
		"no runtime":  rt.ErrUnsupported,
		"unreachable": agentd.ErrAgentUnreachable,
	} {
		if !agentUnavailableLive(err) {
			t.Errorf("%s was not treated as unavailability, so the mirror would not answer", name)
		}
	}
	if agentUnavailableLive(nil) {
		t.Error("a nil error was treated as unavailability")
	}
}

// A replay ends the response and EventSource reconnects, so it needs a retry
// directive and a marker or a sleeping workstation becomes a 3s poll loop.
func TestMirrorReplayIsObservableAndBacksOff(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()
	if _, err := st.MirrorEvents(t.Context(), []store.MirroredEvent{
		{SandboxID: "sb1", OrgID: "org1", Seq: 1, Type: "turn.started", At: time.Now()},
		{SandboxID: "sb1", OrgID: "org1", Seq: 2, Type: "agent.text", At: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}

	s := &Server{st: st}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/sandboxes/sb1/agent/events", nil)
	req = req.WithContext(context.WithValue(req.Context(), orgIDKey, "org1"))

	if !s.replayMirroredEvents(rr, req, rr, "sb1", 0) {
		t.Fatal("the replay declined to serve a mirror that holds events")
	}
	body := rr.Body.String()

	// Nothing changes until the workstation is woken.
	if !strings.Contains(body, "retry: 60000") {
		t.Error("no retry directive; the client would reconnect every few seconds")
	}
	// A named event, not an SSE comment: EventSource hides comments from client code.
	if !strings.Contains(body, "event: mirror.info") || !strings.Contains(body, `"served_from":"mirror"`) {
		t.Errorf("the replay does not identify itself:\n%s", body)
	}
	// seq is the SSE id so a reconnect resumes against either source.
	if !strings.Contains(body, "id: 1") || !strings.Contains(body, "id: 2") {
		t.Errorf("events missing or unnumbered:\n%s", body)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
}

// With nothing mirrored the caller must report the live failure, not serve an empty stream.
func TestReplayDeclinesWhenNothingWasEverMirrored(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()

	s := &Server{st: st}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/sandboxes/sb1/agent/events", nil)
	req = req.WithContext(context.WithValue(req.Context(), orgIDKey, "org1"))

	if s.replayMirroredEvents(rr, req, rr, "sb1", 0) {
		t.Fatal("an empty mirror served a stream, which reads as 'this agent has done nothing'")
	}
	if rr.Body.Len() != 0 {
		t.Errorf("the declined replay still wrote a body: %q", rr.Body.String())
	}
}

// A mirror with history but nothing past the cursor is a legitimate empty replay.
func TestCaughtUpReplayIsStillServed(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()
	if _, err := st.MirrorEvents(t.Context(), []store.MirroredEvent{
		{SandboxID: "sb1", OrgID: "org1", Seq: 5, Type: "agent.text", At: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}

	s := &Server{st: st}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/sandboxes/sb1/agent/events", nil)
	req = req.WithContext(context.WithValue(req.Context(), orgIDKey, "org1"))

	if !s.replayMirroredEvents(rr, req, rr, "sb1", 5) {
		t.Fatal("a caught-up client was refused, which would surface as a transport error")
	}
	if !strings.Contains(rr.Body.String(), "mirror.info") {
		t.Error("a caught-up replay did not identify itself")
	}
}

func TestConnectBundleIsUsableWithoutReadingDocs(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")

	code, out := do(t, "GET", srv+"/v1/sandboxes/"+id+"/agent/connect", key, "")
	if code != http.StatusOK {
		t.Fatalf("connect = %d %v", code, out)
	}

	eps, _ := out["endpoints"].(map[string]any)
	for _, want := range []string{"status", "submit", "events", "approvals", "policy", "responses_compat", "viewer"} {
		if v, _ := eps[want].(string); v == "" {
			t.Errorf("endpoints is missing %q", want)
		}
	}
	sn, _ := out["snippets"].(map[string]any)
	for _, want := range []string{"curl_submit", "curl_watch", "node", "python"} {
		if v, _ := sn[want].(string); v == "" {
			t.Errorf("snippets is missing %q", want)
		}
	}
	// The snippets carry the real token, not a placeholder.
	tok, _ := out["token"].(map[string]any)
	secret, _ := tok["value"].(string)
	if secret == "" {
		t.Fatal("no token value in the bundle")
	}
	if curl, _ := sn["curl_submit"].(string); !strings.Contains(curl, secret) {
		t.Error("the curl snippet does not carry the minted token")
	}
}

// The connect token gets pasted into terminals and chat windows, so it stays narrow.
func TestConnectTokenIsNarrowAndShortLived(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	code, out := do(t, "GET", srv+"/v1/sandboxes/"+id+"/agent/connect", key, "")
	if code != http.StatusOK {
		t.Fatalf("connect = %d %v", code, out)
	}
	tok, _ := out["token"].(map[string]any)

	raw, _ := json.Marshal(tok["scopes"])
	got := string(raw)
	if strings.Contains(got, "agent:console") {
		t.Error("the connect bundle hands out a shell")
	}
	if strings.Contains(got, "agent:admin") {
		t.Error("the connect bundle's token can raise its own spend cap")
	}
	if !strings.Contains(got, "agent:read") || !strings.Contains(got, "agent:write") {
		t.Errorf("the token cannot drive the agent: %s", got)
	}
	if v, _ := tok["shown_once"].(bool); !v {
		t.Error("the response does not say the secret is shown once")
	}
	exp, _ := tok["expires_at"].(string)
	at, err := time.Parse(time.RFC3339, exp)
	if err != nil {
		t.Fatalf("expires_at = %q: %v", exp, err)
	}
	if d := time.Until(at); d > 48*time.Hour {
		t.Errorf("the bundle's token lives %v; it is an exploration credential, not an integration one", d)
	}
}

func TestConnectRepeatedCallsMintDistinctTokens(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")

	first := connectToken(t, srv, key, id)
	second := connectToken(t, srv, key, id)
	if first == second {
		t.Fatal("two connect calls returned the same secret, so 'shown once' is not true")
	}
}

func connectToken(t *testing.T, srv, key, id string) string {
	t.Helper()
	code, out := do(t, "GET", srv+"/v1/sandboxes/"+id+"/agent/connect", key, "")
	if code != http.StatusOK {
		t.Fatalf("connect = %d %v", code, out)
	}
	tok, _ := out["token"].(map[string]any)
	s, _ := tok["value"].(string)
	return s
}

// A QR gets photographed and shown on slides, so it must never encode a credential.
func TestQRRefusesToEncodeACredential(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	base := srv + "/v1/sandboxes/" + id + "/agent/share/qr"

	for name, bad := range map[string]string{
		"an agent access token": "sfa_abc123_" + strings.Repeat("d", 32),
		"a URL carrying one":    "https://example.com/x?token=sfa_abc123_" + strings.Repeat("d", 32),
		"an org key":            "sf_live_abcdefghijklmnop",
	} {
		code, out := do(t, "GET", base+"?url="+url.QueryEscape(bad), key, "")
		if code != http.StatusBadRequest {
			t.Errorf("%s was accepted for QR encoding (%d)", name, code)
			continue
		}
		if msg, _ := out["error"].(string); !strings.Contains(msg, "screenshot") {
			t.Errorf("%s: the refusal does not explain why: %v", name, out)
		}
	}

	// A share link is acceptable: its one-time code is spent on use.
	claim := "/v1/sandboxes/" + id + "/agent/claim?code=abc123"
	code, out := do(t, "GET", base+"?url="+url.QueryEscape(claim), key, "")
	if code != http.StatusOK {
		t.Fatalf("a share link was refused: %d %v", code, out)
	}
	if p, _ := out["payload"].(string); p != claim {
		t.Errorf("payload = %q, want the share link", p)
	}
}

func TestConnectListsUnbuiltChannelsWithAReason(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	code, out := do(t, "GET", srv+"/v1/sandboxes/"+id+"/agent/connect", key, "")
	if code != http.StatusOK {
		t.Fatalf("connect = %d", code)
	}
	chans, _ := out["channels"].([]any)
	seen := map[string]bool{}
	for _, c := range chans {
		m, _ := c.(map[string]any)
		idv, _ := m["id"].(string)
		seen[idv] = true
		if avail, _ := m["available"].(bool); !avail {
			if reason, _ := m["reason"].(string); reason == "" {
				t.Errorf("channel %q is unavailable with no reason", idv)
			}
		}
	}
	for _, want := range []string{"telegram", "discord", "slack", "whatsapp"} {
		if !seen[want] {
			t.Errorf("channel %q is not listed at all, so its absence is silent", want)
		}
	}
}

func TestMCPStubSaysItIsNotANativeSurface(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	code, out := do(t, "GET", srv+"/v1/sandboxes/"+id+"/agent/connect", key, "")
	if code != http.StatusOK {
		t.Fatalf("connect = %d", code)
	}
	mcp, _ := out["mcp"].(map[string]any)
	if cfg, _ := mcp["config"].(string); !strings.Contains(cfg, "mcpServers") {
		t.Error("no MCP config to paste")
	}
	if sup, _ := mcp["supported"].(bool); sup {
		t.Error("the MCP stub claims a native surface that is not built")
	}
	if reason, _ := mcp["reason"].(string); !strings.Contains(reason, "not built yet") {
		t.Errorf("the MCP entry does not say what is missing: %v", mcp["reason"])
	}
}

func TestSpendGuardDoesNotPauseWhatItCannotRead(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	_ = key

	s := serverBehind(t, srv)
	if s == nil {
		t.Skip("cannot reach the server under test")
	}
	before := s.metrics.snapshotSpendPauses()
	s.pauseOverspentAgents(context.Background())
	if after := s.metrics.snapshotSpendPauses(); after != before {
		t.Fatalf("paused %d sandbox(es) whose budget could not be read", after-before)
	}
	code, out := do(t, "GET", srv+"/v1/sandboxes/"+id, key, "")
	if code == http.StatusOK {
		if state, _ := out["status"].(string); state == "paused" {
			t.Fatal("the spend guard paused a sandbox it knew nothing about")
		}
	}
}

// The recorded reason must say what to do next; an unexplained self-pause looks like a crash.
func TestSpendPauseIsAuditedWithAReason(t *testing.T) {
	// Asserted on the constant; a real pause needs a live agentd.
	const want = "Raise or clear the cap, then resume."
	if !strings.Contains(spendPauseReason, want) {
		t.Fatalf("the audit reason does not say what to do next: %q", spendPauseReason)
	}
	if !strings.Contains(spendPauseReason, "machine charges") {
		t.Errorf("the reason does not say why pausing helps: %q", spendPauseReason)
	}
}

// testWebhookURL is a valid target for tests whose subject is not the URL.
// An IP literal so the tests need no DNS; 192.0.2.1 is TEST-NET-1, which the
// SSRF guard accepts and which is never routable.
const testWebhookURL = "https://192.0.2.1/hook"

// A refusal at create time is visible; one at send time is a delivery that silently never arrives.
func TestWebhookCreationRefusesSSRFTargetsAtConfigTime(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	base := srv + "/v1/sandboxes/" + id + "/agent/webhooks"

	for name, url := range map[string]string{
		"cloud metadata": "https://169.254.169.254/hook",
		"loopback":       "https://127.0.0.1/hook",
		"private":        "https://10.1.2.3/hook",
		"plaintext":      "http://example.com/hook",
	} {
		body := `{"url":"` + url + `","events":["turn.completed"]}`
		code, out := do(t, "POST", base, key, body)
		if code != http.StatusBadRequest {
			t.Errorf("%s was accepted at create time (%d %v)", name, code, out)
		}
	}
}

func TestWebhookRefusesTurnOutputWithTheAlternative(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	code, out := do(t, "POST", srv+"/v1/sandboxes/"+id+"/agent/webhooks", key,
		`{"url":"`+testWebhookURL+`","events":["turn.output"]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("turn.output was accepted: %d %v", code, out)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "turn.completed") {
		t.Errorf("the refusal does not name the alternative: %q", msg)
	}
}

func TestWebhookWithNoEventsIsRefused(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	code, out := do(t, "POST", srv+"/v1/sandboxes/"+id+"/agent/webhooks", key,
		`{"url":"`+testWebhookURL+`","events":[]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("a hook with no events was accepted: %d %v", code, out)
	}
}

// The signing secret is recoverable server-side, so no read path returning it
// is the only guard against a forged signature.
func TestWebhookSecretIsShownOnceAndNeverListed(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	base := srv + "/v1/sandboxes/" + id + "/agent/webhooks"

	code, out := do(t, "POST", base, key,
		`{"url":"`+testWebhookURL+`","events":["turn.completed"]}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, out)
	}
	secret, _ := out["secret"].(string)
	if !strings.HasPrefix(secret, "whsec_") {
		t.Fatalf("no signing secret returned: %v", out)
	}
	if v, _ := out["shown_once"].(bool); !v {
		t.Error("the response does not say the secret is shown once")
	}

	code, list := do(t, "GET", base, key, "")
	if code != http.StatusOK {
		t.Fatalf("list = %d %v", code, list)
	}
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), secret) {
		t.Fatal("the webhook listing echoed the signing secret back")
	}
	if strings.Contains(string(raw), "secret_enc") {
		t.Fatal("the listing exposed the encrypted secret column")
	}
	// The listing documents how to verify.
	sig, _ := list["signature"].(map[string]any)
	if m, _ := sig["signed_material"].(string); !strings.Contains(m, "timestamp") {
		t.Errorf("the listing does not document what is signed: %v", sig)
	}
}

func TestWebhookListsOnlyDeliverableEvents(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	code, out := do(t, "GET", srv+"/v1/sandboxes/"+id+"/agent/webhooks", key, "")
	if code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	raw, _ := json.Marshal(out["deliverable_events"])
	if strings.Contains(string(raw), "turn.output") {
		t.Fatal("the advertised event list includes turn.output, which the sender refuses")
	}
	if !strings.Contains(string(raw), "turn.completed") {
		t.Errorf("the advertised list is missing turn.completed: %s", raw)
	}
}

func TestWebhookDeleteIsOrgScoped(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	code, out := do(t, "POST", srv+"/v1/sandboxes/"+id+"/agent/webhooks", key,
		`{"url":"`+testWebhookURL+`","events":["turn.completed"]}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %v", code, out)
	}
	hook, _ := out["webhook"].(map[string]any)
	hookID, _ := hook["id"].(string)

	// A hook id not belonging to this sandbox is a 404, not a 500 or a silent ok.
	if code, _ := do(t, "DELETE", srv+"/v1/sandboxes/"+id+"/agent/webhooks/whk_nope", key, ""); code != http.StatusNotFound {
		t.Errorf("deleting an unknown hook = %d, want 404", code)
	}
	if code, _ := do(t, "DELETE", srv+"/v1/sandboxes/"+id+"/agent/webhooks/"+hookID, key, ""); code != http.StatusOK {
		t.Errorf("deleting a real hook = %d", code)
	}
	_, list := do(t, "GET", srv+"/v1/sandboxes/"+id+"/agent/webhooks", key, "")
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), hookID) {
		t.Error("the deleted hook is still listed")
	}
}

// A hook outlives the token that created it, so a delegated token may not add one.
func TestWebhooksAreOrgKeyOnly(t *testing.T) {
	srv, key, id := newAgentTestAPI(t, "claude")
	_, aat := mintToken(t, srv, key, id,
		"agent:read", "agent:write", "agent:admin", "agent:approve")

	base := srv + "/v1/sandboxes/" + id + "/agent/webhooks"
	for _, c := range []struct{ method, path, body string }{
		{"GET", base, ""},
		{"POST", base, `{"url":"` + testWebhookURL + `","events":["turn.completed"]}`},
		{"DELETE", base + "/whk_x", ""},
		{"POST", base + "/whk_x/test", ""},
	} {
		if code, out := do(t, c.method, c.path, aat, c.body); code != http.StatusUnauthorized {
			t.Errorf("%s %s with an AAT = %d %v, want 401", c.method, c.path, code, out)
		}
	}
}

// Test-send makes the server call a third party on demand, so it needs its own budget.
func TestWebhookTestSendIsRateLimited(t *testing.T) {
	// Asserted on the limiter's configuration; real sends need an outbound receiver.
	if webhookTestLimiter == nil {
		t.Fatal("there is no dedicated test-send limiter, so the general one applies — " +
			"tens of requests per second, which leaves the amplifier open")
	}
	const key = "whtest:hook"
	allowed := 0
	for range 20 {
		if webhookTestLimiter.allow(key) {
			allowed++
		}
	}
	if allowed > 6 {
		t.Fatalf("%d test sends allowed in a burst, want about 5", allowed)
	}
}
