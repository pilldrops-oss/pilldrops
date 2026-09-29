package orchestrator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The MCP mount passes the mux to a handler registered on that same mux, which
// is correct but looks wrong enough that someone may "fix" it.

func mcpPost(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)
	return w
}

// Discovery must answer without a credential; that is MCP's own contract.
func TestMCPInitializeIsServed(t *testing.T) {
	s := NewServer("api-key", "node-token")

	w := mcpPost(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
	var resp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("not JSON-RPC: %v (body %q)", err, w.Body.String())
	}
	if resp.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want 2.0", resp.JSONRPC)
	}
	if len(resp.Error) > 0 {
		t.Errorf("initialize returned an error: %s", resp.Error)
	}
	if len(resp.Result) == 0 {
		t.Error("initialize returned no result — the endpoint is mounted but not answering")
	}
}

func TestMCPListsTools(t *testing.T) {
	s := NewServer("api-key", "node-token")

	w := mcpPost(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Result.Tools) == 0 {
		t.Fatal("tools/list returned nothing")
	}
	// create_sandbox needs fleet scheduling, so MCP must live on this tier.
	var found bool
	for _, tool := range resp.Result.Tools {
		if tool.Name == "create_sandbox" {
			found = true
		}
	}
	if !found {
		names := make([]string, 0, len(resp.Result.Tools))
		for _, tool := range resp.Result.Tools {
			names = append(names, tool.Name)
		}
		t.Errorf("create_sandbox not offered; tools are %v", names)
	}
}

// Stateless streamable HTTP: a 405 with Allow tells a client there is no SSE
// stream, where a 404 would look like the endpoint is absent.
func TestMCPRefusesNonPOST(t *testing.T) {
	s := NewServer("api-key", "node-token")
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /mcp = %d, want 405", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "POST" {
		t.Errorf("Allow = %q, want POST", got)
	}
}
