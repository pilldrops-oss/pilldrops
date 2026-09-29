package orchestrator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/auth/previewtoken"
	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

func mintServer(t *testing.T) *Server {
	t.Helper()
	s := NewServer("fleet-key", "nodetoken")
	upsert(s.reg, "n1", "http://node.invalid", "kk", "", nil, "",
		cluster.Capacity{Cores: 8, MemTotalMB: 64000, MemAvailMB: 64000}, s.now())
	s.reg.Place("sb1", "n1")
	return s
}

func mintReq(t *testing.T, s *Server, bearer, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)
	return w
}

func TestPreviewMintIssuesATokenForTheRequestedPort(t *testing.T) {
	s := mintServer(t)

	w := mintReq(t, s, "fleet-key", "/v1/sandboxes/sb1/preview-token?port=3000")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
	var got struct {
		Token string `json:"token"`
		Port  int    `json:"port"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Port != 3000 {
		t.Errorf("port = %d, want 3000", got.Port)
	}
	// Validate rather than compare against a second mint: tokens carry their own expiry.
	key := previewtoken.BrowserKey(s.apiKey)
	if !previewtoken.BrowserValid(key, got.Token, "sb1", 3000, "org") {
		t.Errorf("token %q is not accepted by the gate for :3000", got.Token)
	}
	if previewtoken.BrowserValid(key, got.Token, "sb1", 8080, "org") {
		t.Error("the :3000 token also opens :8080")
	}
}

// 404 rather than 403, so the endpoint is not a membership oracle.
func TestPreviewMintRefusesAnUnknownSandbox(t *testing.T) {
	s := mintServer(t)

	w := mintReq(t, s, "fleet-key", "/v1/sandboxes/sb-does-not-exist/preview-token?port=3000")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an unplaced sandbox", w.Code)
	}
}

// A share-link recipient holds only a preview token and must not be able to
// mint one for another port.
func TestPreviewMintRequiresAnAPICredential(t *testing.T) {
	s := mintServer(t)
	shareLinkToken := s.previewToken(context.Background(), "sb1", 3000)

	for _, tc := range []struct{ name, bearer string }{
		{"no credential at all", ""},
		{"a preview token is not an API key", shareLinkToken},
		{"a wrong API key", "not-the-fleet-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := mintReq(t, s, tc.bearer, "/v1/sandboxes/sb1/preview-token?port=8080")
			if w.Code == http.StatusOK {
				t.Errorf("minted a token for %s — status 200, body %q", tc.name, w.Body.String())
			}
		})
	}
}

func TestPreviewMintValidatesThePort(t *testing.T) {
	s := mintServer(t)

	for _, tc := range []struct{ name, query string }{
		{"missing", ""},
		{"empty", "?port="},
		{"zero", "?port=0"},
		{"negative", "?port=-1"},
		{"overflow", "?port=99999"},
		{"not a number", "?port=http"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := mintReq(t, s, "fleet-key", "/v1/sandboxes/sb1/preview-token"+tc.query)
			if w.Code != http.StatusBadRequest {
				t.Errorf("port %s: status = %d, want 400", tc.name, w.Code)
			}
		})
	}
}

// Checks the port binding through the real gate, authorizePreview.
func TestMintedTokenOpensOnlyItsOwnPort(t *testing.T) {
	s := mintServer(t)
	tok := s.previewToken(context.Background(), "sb1", 3000)

	check := func(port int) bool {
		req := httptest.NewRequest(http.MethodGet, "/preview/sb1/"+strconv.Itoa(port)+"/?token="+tok, nil)
		return s.authorizePreview(httptest.NewRecorder(), req, "sb1", port)
	}
	if !check(3000) {
		t.Error("the token did not open its own port")
	}
	if check(8080) {
		t.Error("the :3000 token opened :8080 through authorizePreview")
	}
}
