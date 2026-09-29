package httpapi

import (
	"net/http"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/orgassert"
	"github.com/pilldrops-oss/pilldrops/internal/auth/orgassert/orgasserttest"
	"github.com/pilldrops-oss/pilldrops/internal/nodetls"
)

// Replay tests run through the real auth middleware, and every refusal is
// paired with a positive control so "refused" cannot mean "never worked".

// callWithAssertion issues a request carrying hdr as X-Org-Override with an operator key.
func callWithAssertion(t *testing.T, srvURL, opKey, method, path, hdr string) int {
	t.Helper()
	req, err := http.NewRequest(method, srvURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+opKey)
	if hdr != "" {
		req.Header.Set("X-Org-Override", hdr)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestReplayedOrgAssertionIsRefused(t *testing.T) {
	srv, st, _ := newTestServerWithStore(t)
	_, opKey, err := st.CreateKeyWithRole(t.Context(), "orch", "default", "operator")
	if err != nil {
		t.Fatal(err)
	}

	const path = "/v1/sandboxes"
	hdr := orgassert.Sign([]byte(testOrgOverrideSecret), "u:cust1", testNodeID, "GET", path, time.Now())
	if hdr == "" {
		t.Fatal("could not mint an assertion")
	}

	// Positive control.
	if code := callWithAssertion(t, srv.URL, opKey, "GET", path, hdr); code != http.StatusOK {
		t.Fatalf("the first presentation of a valid assertion was refused: %d", code)
	}

	if code := callWithAssertion(t, srv.URL, opKey, "GET", path, hdr); code != http.StatusForbidden {
		t.Errorf("replaying the same assertion = %d, want 403 — a captured header is reusable "+
			"for its whole 60-second life", code)
	}

	// A fresh assertion still works, so the node is not refusing the org or route.
	fresh := orgassert.Sign([]byte(testOrgOverrideSecret), "u:cust1", testNodeID, "GET", path, time.Now())
	if code := callWithAssertion(t, srv.URL, opKey, "GET", path, fresh); code != http.StatusOK {
		t.Errorf("a fresh assertion after the refusal = %d, want 200 — the nonce cache is "+
			"refusing more than the replay", code)
	}
}

// The cross-node replay: a node that receives an assertion re-presents it to a sibling.
func TestAnAssertionMintedForAnotherNodeIsRefused(t *testing.T) {
	srv, st, _ := newTestServerWithStore(t)
	_, opKey, err := st.CreateKeyWithRole(t.Context(), "orch", "default", "operator")
	if err != nil {
		t.Fatal(err)
	}
	const path = "/v1/sandboxes"

	forThisNode := orgassert.Sign([]byte(testOrgOverrideSecret), "u:cust1", testNodeID, "GET", path, time.Now())
	forSibling := orgassert.Sign([]byte(testOrgOverrideSecret), "u:cust1", "some-other-node", "GET", path, time.Now())
	if forThisNode == forSibling {
		t.Fatal("the two assertions are identical — bind() is ignoring the node id")
	}

	if code := callWithAssertion(t, srv.URL, opKey, "GET", path, forSibling); code != http.StatusForbidden {
		t.Errorf("an assertion minted for another node = %d, want 403 — one compromised node can "+
			"act as any tenant on every other node in the fleet", code)
	}
	// Positive control.
	if code := callWithAssertion(t, srv.URL, opKey, "GET", path, forThisNode); code != http.StatusOK {
		t.Errorf("the same assertion minted for this node = %d, want 200", code)
	}
}

// A pre-nonce assertion is tolerated by default so a node can be upgraded
// ahead of its control plane, and refused under enforcement.
func TestNonceLessAssertionIsToleratedThenRefusedUnderEnforcement(t *testing.T) {
	srv, st, _ := newTestServerWithStore(t)
	_, opKey, err := st.CreateKeyWithRole(t.Context(), "orch", "default", "operator")
	if err != nil {
		t.Fatal(err)
	}
	const path = "/v1/sandboxes"
	legacy := legacyAssertion([]byte(testOrgOverrideSecret), "u:cust1", "GET", path, time.Now())

	t.Setenv(nodetls.RequireSecureHopEnv, "")
	if code := callWithAssertion(t, srv.URL, opKey, "GET", path, legacy); code != http.StatusOK {
		t.Fatalf("a pre-upgrade control plane's assertion = %d, want 200 — the node cannot be "+
			"upgraded before the control plane otherwise", code)
	}
	t.Setenv(nodetls.RequireSecureHopEnv, "1")
	if code := callWithAssertion(t, srv.URL, opKey, "GET", path, legacy); code != http.StatusForbidden {
		t.Errorf("a nonce-less assertion under enforcement = %d, want 403", code)
	}
	// Positive control under enforcement.
	current := orgassert.Sign([]byte(testOrgOverrideSecret), "u:cust1", testNodeID, "GET", path, time.Now())
	if code := callWithAssertion(t, srv.URL, opKey, "GET", path, current); code != http.StatusOK {
		t.Errorf("a current assertion under enforcement = %d, want 200", code)
	}
}

func TestNonceCacheClaimsOnFirstUse(t *testing.T) {
	c := newNonceCache()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return base }

	if !c.use("n1") {
		t.Fatal("first use must be allowed")
	}
	if c.use("n1") {
		t.Error("second use must be refused")
	}
	if !c.use("n2") {
		t.Error("an unrelated nonce must still be allowed")
	}
	// An empty nonce is never a repeat, or every legacy assertion would be
	// refused as a replay of every other one. Two named calls because c.use mutates.
	firstEmpty := c.use("")
	secondEmpty := c.use("")
	if !firstEmpty || !secondEmpty {
		t.Errorf("an empty nonce must not be treated as a repeat: first=%v second=%v",
			firstEmpty, secondEmpty)
	}
	c.now = func() time.Time { return base.Add(nonceTTL + time.Second) }
	if !c.use("n1") {
		t.Error("an expired entry must not be retained forever")
	}
}

// legacyAssertion mints the pre-nonce token shape, from a test-only package
// because nothing outside tests should be able to mint it.
func legacyAssertion(secret []byte, org, method, path string, now time.Time) string {
	return orgasserttest.SignLegacy(secret, org, method, path, now)
}
