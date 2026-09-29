package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The API key is the only credential the control plane understands, so these
// tests are the whole authentication contract.

func TestAPIKeyIsRequired(t *testing.T) {
	s := NewServer("sekret", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	for name, key := range map[string]string{
		"no key":                 "",
		"wrong key":              "not-the-key",
		"empty bear":             "Bearer ",
		"prefix of the real key": "sekre",
	} {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/nodes", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", name, resp.StatusCode)
		}
	}
}

func TestCorrectAPIKeyIsAccepted(t *testing.T) {
	s := NewServer("sekret", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/nodes", nil)
	req.Header.Set("Authorization", "Bearer sekret")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("authenticated request = %d, want 200", resp.StatusCode)
	}
}

func TestUnsetAPIKeyFailsClosed(t *testing.T) {
	t.Setenv("DEV_INSECURE", "")
	s := NewServer("", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/v1/nodes")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("an orchestrator with no API key served an authenticated route anonymously")
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503 (refusing to serve unconfigured)", resp.StatusCode)
	}
}
