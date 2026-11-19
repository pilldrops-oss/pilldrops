package orchestrator

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A customer PATCH must not set a node's private targets: only pushPrivate does.
func TestSandboxPatchStripsPrivate(t *testing.T) {
	ts := newConnectorTestServer(t)
	node := ts.fakeNode(t, true)
	resp := ts.do(t, "PATCH", "/v1/sandboxes/sb1", ts.key, `{"name":"renamed","private":[{"name":"db","port":5432}],"Private":[]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch: %d %s", resp.StatusCode, ts.body(resp))
	}
	got := node.lastPatch(t, "sb1")
	if strings.Contains(strings.ToLower(got), "private") || !strings.Contains(got, "renamed") {
		t.Fatalf("node got %s", got)
	}
}

// The second half of a move places like a create. A sandbox with stored
// private targets must not land on a node that cannot route them: the failed
// create there would otherwise undo itself by the original's id.
func TestMoveOfAGrantedSandboxNeedsAPrivateNode(t *testing.T) {
	for _, private := range []bool{true, false} {
		ts := newConnectorTestServer(t)
		node := ts.fakeNode(t, false)
		ctx := context.WithValue(withOrg(withOperator(t.Context()), "org-a"), moveCtxKey{}, moveCtx{from: "elsewhere", private: private})
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/sandboxes", bytes.NewReader([]byte(`{"template":"base","id":"sb1"}`)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		ts.s.handleCreate(w, req)
		node.mu.Lock()
		creates := len(node.creates)
		node.mu.Unlock()
		if private && (creates != 0 || w.Code < 300) {
			t.Fatalf("granted move reached a node without private access: %d %s", w.Code, w.Body.String())
		}
		if !private && creates != 1 {
			t.Fatalf("control: an ungranted move did not reach the node: %d %s", w.Code, w.Body.String())
		}
		if _, ok, _ := ts.s.st.Load().SandboxByID(t.Context(), "sb1"); !ok {
			t.Fatal("the source sandbox's row is gone")
		}
	}
}

func TestRehomeOfAGrantedSandboxNeedsAPrivateNode(t *testing.T) {
	ts := newConnectorTestServer(t)
	ts.fakeNodeNamed(t, "plain", false)
	rec, _, _ := ts.s.st.Load().SandboxByID(t.Context(), "sb-1-conn")
	rec.Private = `[{"name":"db","port":5432}]`
	req := httptest.NewRequestWithContext(withOperator(t.Context()), http.MethodPost, "/", nil)
	w := httptest.NewRecorder()
	if _, ok := ts.s.rehome(w, req, rec); ok {
		t.Fatal("rehomed a granted sandbox onto a node without private access")
	}
	capable := ts.fakeNodeNamed(t, "capable", true)
	w = httptest.NewRecorder()
	n, ok := ts.s.rehome(w, req, rec)
	if !ok || n.ID != capable.id {
		t.Fatalf("rehome = %v %v %s, want the capable node", n, ok, w.Body.String())
	}
}
