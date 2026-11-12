package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func shareServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("SHARE_DOMAIN", "share.example.com")
	t.Setenv("SHARE_INGRESS", "1")
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "share.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := NewServer("fleet-key", "nodetoken")
	s.SetStore(st)
	return s
}

func TestShareCreateAnswersALinkAndToken(t *testing.T) {
	s := shareServer(t)
	w := apiReq(t, s, "POST", "u:one", "/v1/shares", `{}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	m := decodeJSON(t, w)
	name, _ := m["name"].(string)
	if !regexp.MustCompile(`^[a-z]+-[a-z]+-[a-z0-9]{8}$`).MatchString(name) {
		t.Fatalf("name = %q", name)
	}
	if m["url"] != "https://"+name+".share.example.com" {
		t.Fatalf("url = %v", m["url"])
	}
	if m["tunnel_url"] != "wss://share.example.com/tunnel" {
		t.Fatalf("tunnel_url = %v", m["tunnel_url"])
	}
	if !s.validShareToken(m["tunnel_token"].(string), m["id"].(string)) {
		t.Fatal("the returned token does not verify")
	}
	if s.validShareToken(m["tunnel_token"].(string), "sh_other") {
		t.Fatal("a token verified for another share")
	}
}

func TestShareCreateChecksTTL(t *testing.T) {
	s := shareServer(t)
	if w := apiReq(t, s, "POST", "u:one", "/v1/shares", `{"ttl_seconds":90000}`); w.Code != http.StatusBadRequest {
		t.Fatalf("over max = %d", w.Code)
	}
	if w := apiReq(t, s, "POST", "u:one", "/v1/shares", `{"ttl_seconds":-1}`); w.Code != http.StatusBadRequest {
		t.Fatalf("negative = %d", w.Code)
	}
	if w := apiReq(t, s, "POST", "u:one", "/v1/shares", `{"ttl_seconds":3600}`); w.Code != http.StatusCreated {
		t.Fatalf("in range = %d", w.Code)
	}
}

func TestShareCreateRefusedWithNoIngress(t *testing.T) {
	s := shareServer(t)
	t.Setenv("SHARE_INGRESS", "")
	w := apiReq(t, s, "POST", "u:one", "/v1/shares", `{}`)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("create = %d", w.Code)
	}
	if m := decodeJSON(t, w); m["code"] != "no_share_ingress" {
		t.Fatalf("code = %v", m["code"])
	}
}

func TestShareCreateEnforcesTheOrgLimit(t *testing.T) {
	s := shareServer(t)
	t.Setenv("SHARE_MAX_PER_ORG", "2")
	for i := range 2 {
		if w := apiReq(t, s, "POST", "u:one", "/v1/shares", `{}`); w.Code != http.StatusCreated {
			t.Fatalf("create %d = %d", i, w.Code)
		}
	}
	w := apiReq(t, s, "POST", "u:one", "/v1/shares", `{}`)
	if w.Code != http.StatusTooManyRequests || decodeJSON(t, w)["code"] != "share_limit" {
		t.Fatalf("over limit = %d %s", w.Code, w.Body)
	}
	if w := apiReq(t, s, "POST", "u:two", "/v1/shares", `{}`); w.Code != http.StatusCreated {
		t.Fatalf("another org = %d", w.Code)
	}
}

func TestShareTokenAndDeleteAreOrgScoped(t *testing.T) {
	s := shareServer(t)
	id := decodeJSON(t, apiReq(t, s, "POST", "u:one", "/v1/shares", `{}`))["id"].(string)
	for _, c := range []struct{ method, path string }{
		{"POST", "/v1/shares/" + id + "/token"},
		{"DELETE", "/v1/shares/" + id},
	} {
		if w := apiReq(t, s, c.method, "u:two", c.path, ""); w.Code != http.StatusNotFound {
			t.Fatalf("%s as another org = %d", c.method, w.Code)
		}
	}
	if w := apiReq(t, s, "POST", "u:one", "/v1/shares/"+id+"/token", ""); w.Code != http.StatusOK {
		t.Fatalf("token = %d", w.Code)
	}
	if w := apiReq(t, s, "DELETE", "u:one", "/v1/shares/"+id, ""); w.Code != http.StatusOK {
		t.Fatalf("delete = %d", w.Code)
	}
	w := apiReq(t, s, "POST", "u:one", "/v1/shares/"+id+"/token", "")
	if w.Code != http.StatusGone || decodeJSON(t, w)["reason"] != "closed" {
		t.Fatalf("token after close = %d %s", w.Code, w.Body)
	}
}

func TestShareBlockNeedsTheOperator(t *testing.T) {
	s := shareServer(t)
	name := decodeJSON(t, apiReq(t, s, "POST", "u:one", "/v1/shares", `{}`))["name"].(string)
	if w := apiReq(t, s, "POST", "u:one", "/v1/admin/shares/"+name+"/block", ""); w.Code == http.StatusOK {
		t.Fatal("a customer blocked a share")
	}
	req := httptest.NewRequest("POST", "/v1/admin/shares/"+name+"/block", nil)
	req.Header.Set("Authorization", "Bearer fleet-key")
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("block = %d %s", w.Code, w.Body)
	}
	sh, _, _ := s.st.Load().ShareByName(t.Context(), name)
	if sh.BlockedAt == 0 {
		t.Fatal("row not blocked")
	}
}

func TestSandboxIdentityCannotCreateAShare(t *testing.T) {
	if sandboxPrincipalAllowed("POST", "/v1/shares", "sb1") {
		t.Fatal("a sandbox may create a share")
	}
}

func TestCapabilitiesReportShareLinks(t *testing.T) {
	s := shareServer(t)
	if w := apiReq(t, s, "GET", "u:one", "/v1/capabilities", ""); decodeJSON(t, w)["share_links"] != true {
		t.Fatalf("capabilities = %s", w.Body)
	}
	t.Setenv("SHARE_DOMAIN", "")
	if w := apiReq(t, s, "GET", "u:one", "/v1/capabilities", ""); decodeJSON(t, w)["share_links"] != false {
		t.Fatalf("capabilities = %s", w.Body)
	}
}

func TestShareDomainGuard(t *testing.T) {
	for _, c := range []struct {
		share, sandbox, origin string
		bad                    bool
	}{
		{"", "sb.example.com", "", false},
		{"pdshare.dev", "sb.example.com", "https://app.example.com", false},
		{"sb.example.com", "sb.example.com", "", false},
		{"sb.example.com", "sb.example.com", "https://app.example.com", true},
		{"share.sb.example.com", "sb.example.com", "", true},
		{"example.com", "sb.example.com", "", true},
		{"share.app.example.com", "", "https://app.example.com", true},
		{"example.com", "", "https://app.example.com", true},
		{"share.example.com", "", "https://app.example.com", true},
		{"share.example.com", "sb.example.com", "", true},
		{"share.example.co.uk", "sb.other.co.uk", "https://app.other.co.uk", false},
		{"share.localhost:18090", "", "", false},
	} {
		t.Setenv("SHARE_DOMAIN", c.share)
		if err := ShareDomainGuardError(c.sandbox, c.origin); (err != nil) != c.bad {
			t.Errorf("%+v: err = %v", c, err)
		}
	}
}
