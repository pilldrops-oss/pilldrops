package orchestrator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

func getPublicTemplates(t *testing.T, s *Server) (*httptest.ResponseRecorder, []templates.Template) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/templates/public", nil)
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)
	var out []templates.Template
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func TestPublicTemplatesNeedNoCredential(t *testing.T) {
	s := NewServer("api-key", "node-token")

	w, out := getPublicTemplates(t, s)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a signed-out picker falls back to a stale "+
			"hand-maintained list without this (body %q)", w.Code, w.Body.String())
	}
	if len(out) == 0 {
		t.Fatal("empty catalogue")
	}
	if w.Header().Get("Cache-Control") == "" {
		t.Error("no Cache-Control: an anonymous picker load would reach the origin every time")
	}
}

// Names and flags stay so the UI can render a form; values must not appear unauthenticated.
func TestPublicTemplatesRedactEnvValues(t *testing.T) {
	s := NewServer("api-key", "node-token")

	// Find a template with an actual env value, so redaction is proven rather than absence of data.
	var withValue string
	for _, tpl := range templates.Catalog() {
		for _, e := range tpl.Env {
			if e.Value != "" {
				withValue = tpl.ID
			}
		}
	}
	if withValue == "" {
		t.Skip("no catalogue template declares an env value; nothing to redact")
	}

	_, out := getPublicTemplates(t, s)
	for _, tpl := range out {
		for _, e := range tpl.Env {
			if e.Value != "" {
				t.Errorf("%s exposes env %s=%q on the UNAUTHENTICATED endpoint", tpl.ID, e.Name, e.Value)
			}
		}
		if tpl.ID == withValue {
			if len(tpl.Env) == 0 {
				t.Errorf("%s lost its env list entirely — the UI cannot render a form without the names", tpl.ID)
			}
			for _, e := range tpl.Env {
				if e.Name == "" {
					t.Errorf("%s has an env entry with no name", tpl.ID)
				}
			}
		}
	}
}

// Env shares the catalogue's backing array; redacting in place would strip the authenticated view too.
func TestPublicRedactionDoesNotLeakIntoTheCatalogue(t *testing.T) {
	before := 0
	for _, tpl := range templates.Catalog() {
		for _, e := range tpl.Env {
			if e.Value != "" {
				before++
			}
		}
	}
	_ = publicCatalog()
	after := 0
	for _, tpl := range templates.Catalog() {
		for _, e := range tpl.Env {
			if e.Value != "" {
				after++
			}
		}
	}
	if after != before {
		t.Errorf("env values in the shared catalogue went %d -> %d after building the public "+
			"view: the redaction mutated shared backing storage", before, after)
	}
}
