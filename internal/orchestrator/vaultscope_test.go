package orchestrator

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// The rule itself, in one table: which (scope, sandbox visibility, caller)
// combinations may inject. "all" must behave exactly as before scopes existed.
func TestScopeAllowsMatchesTheVisibilityRule(t *testing.T) {
	for _, tc := range []struct {
		name               string
		scope, owner       string
		visibility, caller string
		refuse             bool
		mentions           string
	}{
		{"all into an org sandbox", store.ScopeAll, "key_alice", "org", "key_bob", false, ""},
		{"all into a private sandbox", store.ScopeAll, "key_alice", "private", "key_bob", false, ""},
		{"private into its owner's private sandbox", store.ScopePrivate, "key_alice", "private", "key_alice", false, ""},
		{"private into an org sandbox", store.ScopePrivate, "key_alice", "org", "key_alice", true, "private sandbox"},
		{"private into someone else's private sandbox", store.ScopePrivate, "key_alice", "private", "key_bob", true, "key that stored it"},
		{"private with no recorded owner", store.ScopePrivate, "", "private", "key_alice", true, "key that stored it"},
		{"shared into an org sandbox", store.ScopeShared, "key_alice", "org", "key_bob", false, ""},
		{"shared into a private sandbox", store.ScopeShared, "key_alice", "private", "key_alice", true, "not a private one"},
	} {
		err := scopeAllows("API_KEY", tc.scope, tc.owner, tc.visibility, tc.caller)
		if tc.refuse && err == nil {
			t.Errorf("%s: allowed, want a refusal", tc.name)
			continue
		}
		if !tc.refuse && err != nil {
			t.Errorf("%s: refused (%v), want allowed", tc.name, err)
			continue
		}
		if tc.refuse && (!strings.Contains(err.Error(), "API_KEY") || !strings.Contains(err.Error(), tc.mentions)) {
			t.Errorf("%s: refusal %q does not name the secret and the reason (%q)", tc.name, err, tc.mentions)
		}
	}
}

// End to end through resolveSecretRefs: the create body's own visibility and
// the calling key decide, and a refusal fails the create rather than quietly
// launching a sandbox missing the credential it asked for.
func TestResolveSecretRefsAppliesTheScope(t *testing.T) {
	s := NewServer("k", "t")
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "vault.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	st.SetAllowPlaintextVault(true)
	s.SetStore(st)
	if _, err := st.PutSecret(t.Context(), "u:cust9", "PRIVATE_KEY", "sk-mine", true, false, "k:alice"); err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	if ok, err := st.SetSecretScope(t.Context(), "u:cust9", "PRIVATE_KEY", store.ScopePrivate); err != nil || !ok {
		t.Fatalf("SetSecretScope: %v", err)
	}

	asAlice := func(body string) ([]byte, error) {
		r := httptest.NewRequest("POST", "/v1/sandboxes", strings.NewReader(body))
		r.Header.Set("X-Org-Override", "u:cust9")
		ctx := withKey(withOperator(r.Context()), "k:alice", "user")
		return s.resolveSecretRefs(r.WithContext(ctx), []byte(body))
	}

	out, err := asAlice(`{"template":"base","visibility":"private","secret_refs":["PRIVATE_KEY"]}`)
	if err != nil {
		t.Fatalf("the owner's private create was refused: %v", err)
	}
	if !strings.Contains(string(out), "PRIVATE_KEY=sk-mine") {
		t.Fatalf("the secret did not reach the create body: %s", out)
	}

	if _, err := asAlice(`{"template":"base","secret_refs":["PRIVATE_KEY"]}`); err == nil {
		t.Error("a create with no visibility (so org-visible) took a private secret")
	} else if !strings.Contains(err.Error(), "PRIVATE_KEY") {
		t.Errorf("refusal does not name the secret: %v", err)
	}

	r := httptest.NewRequest("POST", "/v1/sandboxes", nil)
	ctx := withKey(withOperator(r.Context()), "k:bob", "user")
	r.Header.Set("X-Org-Override", "u:cust9")
	if _, err := s.resolveSecretRefs(r.WithContext(ctx),
		[]byte(`{"template":"base","visibility":"private","secret_refs":["PRIVATE_KEY"]}`)); err == nil {
		t.Error("another member's private sandbox took someone else's private secret")
	}
}
