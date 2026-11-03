package orchestrator

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mintToken(t *testing.T, srv *httptest.Server, key string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/join-tokens", nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("mint status = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func TestMintJoinTokenRequiresAdminAndReturnsCommand(t *testing.T) {
	t.Setenv("NODE_BOOTSTRAP_SECRET", "sekret")
	s := NewServer("adminkey", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/join-tokens", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("anonymous mint = %d, want 401", resp.StatusCode)
	}

	out := mintToken(t, srv, "adminkey")
	tok, _ := out["token"].(string)
	if !strings.HasPrefix(tok, "sfj_") {
		t.Fatalf("token = %q, want sfj_-prefixed", tok)
	}
	// The command must be runnable and must not carry the token.
	cmd, _ := out["bootstrap_cmd"].(string)
	if !strings.Contains(cmd, "install.sh") || !strings.Contains(cmd, "ORCHESTRATOR_URL") {
		t.Fatalf("bootstrap_cmd is not a usable install command: %q", cmd)
	}
	if strings.Contains(cmd, tok) {
		t.Fatalf("bootstrap_cmd carries the live token: %q", cmd)
	}
	if _, ok := out["expires_at"]; !ok {
		t.Fatal("response missing expires_at")
	}
}

func TestJoinRedeemsTokenAndReturnsConfig(t *testing.T) {
	t.Setenv("NODE_BOOTSTRAP_DSN", "postgres://u:p@h:5432/db")
	t.Setenv("NODE_BOOTSTRAP_SECRET", "sekret")
	s := NewServer("adminkey", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	tok, _ := mintToken(t, srv, "adminkey")["token"].(string)

	join := func(body string) (*http.Response, map[string]any) {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/join", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-Proto", "https") // httptest is plaintext
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("join: %v", err)
		}
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		return resp, out
	}

	resp, out := join(`{"token":"` + tok + `","zone":"par1","advertise_url":"http://1.2.3.4:18080"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("join = %d body=%v", resp.StatusCode, out)
	}
	id, _ := out["node_id"].(string)
	if id == "" {
		t.Fatal("join returned no node_id")
	}
	if out["node_api_key"] != s.nodeKeyFor(id, "par1") {
		t.Fatalf("node_api_key = %v, want derived key for %s|par1", out["node_api_key"], id)
	}
	if out["postgres_dsn"] != "postgres://u:p@h:5432/db" || out["secret_key"] != "sekret" {
		t.Fatalf("bootstrap config not returned: %v", out)
	}
	if out["node_token"] != s.nodeHeartbeatToken(id, "par1") {
		t.Fatalf("node_token = %v, want the token derived for %s|par1", out["node_token"], id)
	}
	if out["node_token"] == s.nodeToken {
		t.Fatal("node_token is the fleet-wide shared secret — any node could heartbeat as any other")
	}
	if out["node_token"] == out["node_api_key"] {
		t.Fatal("the heartbeat token equals the node API key — the derivations are not domain-separated")
	}

	if resp2, _ := join(`{"token":"` + tok + `","zone":"par1","advertise_url":"http://1.2.3.4:18080"}`); resp2.StatusCode == 200 {
		t.Fatal("replayed token should not succeed")
	}
}

func TestJoinRejectsOversizedBody(t *testing.T) {
	s := NewServer("adminkey", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	big := `{"token":"sfj_x","advertise_url":"` + strings.Repeat("A", 8192) + `"}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/join", strings.NewReader(big))
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 400 {
		t.Fatalf("oversized join body = %d, want 400", resp.StatusCode)
	}
}

func TestInstallScriptServed(t *testing.T) {
	s := NewServer("adminkey", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/install.sh") // public
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("/install.sh = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{"pilldrops-join", "JOIN_TOKEN", "docker"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("install.sh missing %q", want)
		}
	}
}

func TestNodeBinaryServed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sandboxd-linux"), []byte("ELF"), 0o644); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}

	get := func(url string) (int, string) {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("get %s: %v", url, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	t.Setenv("NODE_BIN_DIR", dir)
	s := NewServer("adminkey", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	if code, body := get(srv.URL + "/bin/sandboxd-linux"); code != 200 || !strings.Contains(body, "ELF") {
		t.Fatalf("GET /bin/sandboxd-linux = %d body=%q, want 200 + ELF", code, body)
	}
	if code, _ := get(srv.URL + "/bin/..%2f..%2fetc%2fpasswd"); code != 404 {
		t.Fatalf("traversal GET = %d, want 404", code)
	}
	if code, _ := get(srv.URL + "/bin/"); code == 200 {
		t.Fatalf("GET /bin/ (no name) = 200, want non-200")
	}

	t.Setenv("NODE_BIN_DIR", "")
	if code, _ := get(srv.URL + "/bin/sandboxd-linux"); code != 404 {
		t.Fatalf("GET with NODE_BIN_DIR unset = %d, want 404", code)
	}
}

func TestJoinRejectsBadTokenAndPlaintext(t *testing.T) {
	t.Setenv("NODE_BOOTSTRAP_SECRET", "sekret")
	s := NewServer("adminkey", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/join", strings.NewReader(`{"token":"sfj_nope","advertise_url":"http://x:18080"}`))
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("bad token = %d, want 401", resp.StatusCode)
	}

	// Valid token over plaintext: no X-Forwarded-Proto and httptest is not TLS.
	tok, _ := mintToken(t, srv, "adminkey")["token"].(string)
	req2, _ := http.NewRequest("POST", srv.URL+"/v1/join", strings.NewReader(`{"token":"`+tok+`","advertise_url":"http://x:18080"}`))
	resp2, _ := http.DefaultClient.Do(req2)
	if resp2.StatusCode != 400 {
		t.Fatalf("plaintext join = %d, want 400", resp2.StatusCode)
	}
}

// A node that joins with no secret key stores every sandbox's env unsealed in
// the shared database, so a clustered control plane refuses to mint.
func TestMintJoinTokenRefusesWithoutBootstrapSecret(t *testing.T) {
	t.Setenv("NODE_BOOTSTRAP_SECRET", "")
	t.Setenv("SANDBOXD_SECRET_KEY", "")
	s := NewServer("adminkey", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/join-tokens", nil)
	req.Header.Set("Authorization", "Bearer adminkey")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("mint with no node secret key = %d, want 503", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "NODE_BOOTSTRAP_SECRET") {
		t.Fatalf("error does not name NODE_BOOTSTRAP_SECRET: %s", body)
	}
}
