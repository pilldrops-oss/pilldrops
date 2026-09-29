package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// Two reads of a sandbox took placement for permission: "this id is on a node
// I know" rather than "this caller may see it". A member of the same org could
// therefore list what somebody else's PRIVATE sandbox is serving and poll
// whether it is up — enumerating a machine the rest of the API says does not
// exist for them. Both now run callerOwnsSandbox, the one predicate every
// other read uses.
func TestPrivateSandboxReadsAreNotOpenToTheWholeOrg(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ports":[8000]}`))
	}))
	defer node.Close()

	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "pr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	s := NewServer("fleet-key", "nodetoken")
	s.SetStore(st)
	if err := s.reg.Upsert(cluster.NodeUpsert{ID: "n1", URL: node.URL, Key: "kk"}, s.now()); err != nil {
		t.Fatal(err)
	}
	owner, ownerKey, err := st.CreateKey(t.Context(), "alice", "u:one")
	if err != nil {
		t.Fatal(err)
	}
	_, strangerKey, err := st.CreateKey(t.Context(), "bob", "u:one")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSandbox(t.Context(), store.SandboxRecord{
		ID: "sb1", OrgID: "u:one", Template: "base", State: "running", NodeID: "n1",
		CreatedBy: owner.ID, Visibility: sandbox.VisibilityPrivate, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	s.reg.Place("sb1", "n1")

	get := func(path, key string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, req)
		return w.Code
	}

	for _, path := range []string{"/v1/sandboxes/sb1/ports", "/v1/sandboxes/sb1/preview-status"} {
		// The positive control first: without it a blanket 404 from a broken
		// route would read as the rule working.
		if code := get(path, ownerKey); code == http.StatusNotFound {
			t.Errorf("the creator GET %s = 404 — the guard refuses its owner too", path)
		}
		if code := get(path, strangerKey); code != http.StatusNotFound {
			t.Errorf("another member of the same org GET %s = %d, want 404: a private sandbox "+
				"is still enumerable by a key that may not see it", path, code)
		}
	}
}
