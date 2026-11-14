package orchestrator

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func TestValidConnectorHost(t *testing.T) {
	t.Parallel()
	platform := []string{"pilldrops.com", "pilldrops.app"}
	for _, ok := range []string{"internal.corp", "db.prod.example.com", "10.0.0.5"} {
		if err := validConnectorHost(ok, platform); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"localhost", "LOCALHOST", "db", "com", "co.uk", "pilldrops.com", "api.pilldrops.com", "x.pilldrops.app", "evil.localhost", "pd", "private", "bravo.pd", "db.private", "X.Private.", "a.com\n10.0.0.1 api.pilldrops.com", "evil.com:22", "x y.com", "-a.com", "a..com"} {
		if validConnectorHost(bad, platform) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestConnectorRoutesRefuseSandboxPrincipal(t *testing.T) {
	ts := newConnectorTestServer(t)
	for _, path := range []string{"/v1/connectors", "/v1/connectors/session"} {
		resp := ts.do(t, "POST", path, ts.sandboxToken, `{"name":"prod"}`)
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s as sandbox: %d", path, resp.StatusCode)
		}
	}
}

func TestConnectorCreateListRevoke(t *testing.T) {
	ts := newConnectorTestServer(t)
	resp := ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"prod"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, ts.body(resp))
	}
	if resp := ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"prod"}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate: %d", resp.StatusCode)
	}
	if resp := ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"Bad_Name"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad name: %d", resp.StatusCode)
	}
	list := ts.do(t, "GET", "/v1/connectors", ts.key, "")
	if !strings.Contains(ts.body(list), `"name":"prod"`) {
		t.Fatal("list misses prod")
	}
	if resp := ts.do(t, "GET", "/v1/connectors", ts.otherOrgKey, ""); strings.Contains(ts.body(resp), "prod") {
		t.Fatal("another org sees prod")
	}
	id := ts.connectorID(t, "prod")
	if resp := ts.do(t, "DELETE", "/v1/connectors/"+id, ts.otherOrgKey, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("other org revoke: %d", resp.StatusCode)
	}
	if resp := ts.do(t, "DELETE", "/v1/connectors/"+id, ts.key, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
}

func TestConnectorLimitPerOrg(t *testing.T) {
	ts := newConnectorTestServer(t)
	for i := range maxConnectorsPerOrg {
		if resp := ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"c`+string(rune('a'+i))+`"}`); resp.StatusCode != http.StatusCreated {
			t.Fatalf("create %d: %d", i, resp.StatusCode)
		}
	}
	if resp := ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"over"}`); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over limit: %d", resp.StatusCode)
	}
}

func TestConnectorSessionWithKey(t *testing.T) {
	ts := newConnectorTestServer(t)
	ts.do(t, "POST", "/v1/connectors", ts.key, `{"name":"prod"}`)
	id := ts.connectorID(t, "prod")
	kr := ts.do(t, "POST", "/v1/connectors/"+id+"/keys", ts.key, "")
	plain := ts.jsonField(t, kr, "key")
	if !strings.HasPrefix(plain, "pd_conn_") {
		t.Fatalf("key %q", plain)
	}
	sr := ts.do(t, "POST", "/v1/connectors/session", plain, `{"name":"prod"}`)
	if sr.StatusCode != http.StatusOK || ts.jsonField(t, sr, "tunnel_token") == "" {
		t.Fatalf("session with key: %d", sr.StatusCode)
	}
	if sr := ts.do(t, "POST", "/v1/connectors/session", plain, `{"name":"other"}`); sr.StatusCode != http.StatusForbidden {
		t.Fatalf("key for another connector: %d", sr.StatusCode)
	}
	if resp := ts.do(t, "GET", "/v1/connectors", plain, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("connector key on the API: %d", resp.StatusCode)
	}
}

type connectorTestServer struct {
	s                              *Server
	srv                            *httptest.Server
	key, otherOrgKey, sandboxToken string
}

func newConnectorTestServer(t *testing.T) *connectorTestServer {
	t.Helper()
	t.Setenv("PRIVATE_INGRESS", "1")
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "connectors.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sbTok := sbtoken.IdentityPrefix + strings.Repeat("ab", 32)
	rec := store.SandboxRecord{ID: "sb-1-conn", OrgID: "org-a", Template: "base", State: "running", IdentityHash: sbtoken.HashHex(sbTok)}
	if err := st.SaveSandbox(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	s := NewServer("fleet-key", "nodetoken")
	s.SetStore(st)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	return &connectorTestServer{s: s, srv: srv, key: orgKey(t, s, "org-a"), otherOrgKey: orgKey(t, s, "org-b"), sandboxToken: sbTok}
}

func (ts *connectorTestServer) do(t *testing.T, method, path, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, ts.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// body reads and closes a response, so a later read of the same one is empty.
func (ts *connectorTestServer) body(resp *http.Response) string {
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func (ts *connectorTestServer) jsonField(t *testing.T, resp *http.Response, field string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(ts.body(resp)), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	v, _ := m[field].(string)
	return v
}

func (ts *connectorTestServer) connectorID(t *testing.T, name string) string {
	t.Helper()
	var out struct {
		Connectors []struct{ ID, Name string } `json:"connectors"`
	}
	if err := json.Unmarshal([]byte(ts.body(ts.do(t, "GET", "/v1/connectors", ts.key, ""))), &out); err != nil {
		t.Fatal(err)
	}
	for _, c := range out.Connectors {
		if c.Name == name {
			return c.ID
		}
	}
	t.Fatalf("connector %q not listed", name)
	return ""
}

func TestConnectorCapabilityNeedsACapableNode(t *testing.T) {
	ts := newConnectorTestServer(t)
	caps := func() bool {
		var m map[string]any
		if err := json.Unmarshal([]byte(ts.body(ts.do(t, "GET", "/v1/capabilities", ts.key, ""))), &m); err != nil {
			t.Fatal(err)
		}
		return m["private_access"] == true
	}
	if caps() {
		t.Fatal("private_access with no capable node")
	}
	if err := ts.s.reg.Upsert(cluster.NodeUpsert{ID: "n1", URL: "http://n1.invalid", Key: "k-n1",
		Cap: cluster.Capacity{Cores: 8, MemTotalMB: 64000, MemAvailMB: 64000}}, ts.s.now()); err != nil {
		t.Fatal(err)
	}
	ts.s.reg.SetNodeMeta("n1", cluster.NodeMeta{Runtime: "docker", PrivateAccess: true})
	if !caps() {
		t.Fatal("private_access false with a capable node")
	}
	t.Setenv("PRIVATE_INGRESS", "")
	if caps() {
		t.Fatal("private_access with PRIVATE_INGRESS unset")
	}
}
