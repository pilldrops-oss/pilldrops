package orchestrator

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

const (
	privSelf    = "sb-priv-self"
	privSibling = "sb-priv-sibling"
	privToken   = sbtoken.IdentityPrefix + "aaaabbbbccccddddaaaabbbbccccddddaaaabbbbccccddddaaaabbbbccccdddd"
	sibToken    = sbtoken.IdentityPrefix + "1111222233334444111122223333444411112222333344441111222233334444"
)

// The control plane names the caller a sandbox is; internal/sandbox decides
// what that caller may see. This pins the joint: a sandbox principal that
// reaches the ownership rule unnamed is judged as "no key at all", which is
// the one caller a private sandbox refuses — so it could not list, read or
// mint for itself, though it was the machine in question.
func TestASandboxPrincipalIsNamedAsItself(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "sp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	// Both private, both created by somebody else's key: the fork-child shape.
	for _, r := range []store.SandboxRecord{
		{ID: privSelf, OrgID: "org-a", Template: "base", State: "running",
			IdentityHash: sbtoken.HashHex(privToken), Visibility: sandbox.VisibilityPrivate, CreatedBy: "key_alice"},
		{ID: privSibling, OrgID: "org-a", Template: "base", State: "running",
			IdentityHash: sbtoken.HashHex(sibToken), Visibility: sandbox.VisibilityPrivate, CreatedBy: "key_alice"},
	} {
		if err := st.SaveSandbox(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	s := NewServer("k", "t")
	s.SetStore(st)
	upsert(s.reg, "n1", "http://node.invalid", "kk", "", nil, "",
		cluster.Capacity{Cores: 8, MemTotalMB: 64000, MemAvailMB: 64000}, s.now())
	s.reg.Place(privSelf, "n1")
	s.reg.Place(privSibling, "n1")
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)

	// It sees itself, and only itself: the sibling is private to its creator.
	code, body := doAs(t, srv, privToken, "GET", "/v1/sandboxes")
	if code != 200 {
		t.Fatalf("list as a sandbox = %d %s", code, body)
	}
	var list struct {
		Sandboxes []struct{ ID string } `json:"sandboxes"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Sandboxes) != 1 || list.Sandboxes[0].ID != privSelf {
		t.Errorf("a sandbox's list of its own private row = %s, want just %s", body, privSelf)
	}

	// Minting a preview token runs the same ownership rule on the same tier.
	if code, body := doAs(t, srv, privToken, "GET", "/v1/sandboxes/"+privSelf+"/preview-token?port=8000"); code != 200 {
		t.Errorf("a sandbox cannot mint a preview token for itself: %d %s", code, body)
	}
	// Refused, by the route allowlist (403, which names the principal) or by
	// the ownership rule (404) — either way not minted.
	if code, _ := doAs(t, srv, privToken, "GET", "/v1/sandboxes/"+privSibling+"/preview-token?port=8000"); code == 200 {
		t.Error("a sandbox minted a preview token for somebody else's private sandbox")
	}
}
