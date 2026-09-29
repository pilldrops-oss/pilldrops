package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func newTenantServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s := NewServer("fleet-key", "nodetoken")
	s.SetStore(st)
	return s, st
}

// orgFor returns the org a request resolves to after auth().
func orgFor(t *testing.T, s *Server, bearer, override string) string {
	t.Helper()
	var got string
	h := s.auth(func(w http.ResponseWriter, r *http.Request) { got = fastOrg(r) })
	req := httptest.NewRequest("GET", "/v1/sandboxes", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if override != "" {
		req.Header.Set(orgOverride, override)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code == http.StatusUnauthorized {
		return "<401>"
	}
	return got
}

func TestCustomerKeyCannotOverrideItsOrg(t *testing.T) {
	s, st := newTenantServer(t)
	_, key, err := st.CreateKey(t.Context(), "acme-key", "u:acme")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	if got := orgFor(t, s, key, ""); got != "u:acme" {
		t.Errorf("customer key resolved to %q, want its own org u:acme", got)
	}
	if got := orgFor(t, s, key, "u:victim"); got != "u:acme" {
		t.Errorf("customer key + X-Org-Override:u:victim resolved to %q — "+
			"a customer widened their own scope by sending a header", got)
	}
}

func TestOperatorKeyMayNameAnOrg(t *testing.T) {
	s, _ := newTenantServer(t)
	if got := orgFor(t, s, "fleet-key", "u:acme"); got != "u:acme" {
		t.Errorf("operator + override resolved to %q, want u:acme", got)
	}
	if got := orgFor(t, s, "fleet-key", ""); got != "default" {
		t.Errorf("operator with no override resolved to %q, want default", got)
	}
}

func TestUnauthenticatedCallerIsRefused(t *testing.T) {
	s, _ := newTenantServer(t)
	for name, tc := range map[string]struct{ bearer, override string }{
		"no credential":         {"", ""},
		"header only":           {"", "u:victim"},
		"wrong key":             {"nope", ""},
		"wrong key with header": {"nope", "u:victim"},
	} {
		if got := orgFor(t, s, tc.bearer, tc.override); got != "<401>" {
			t.Errorf("%s: resolved to %q, want 401", name, got)
		}
	}
}

func TestUnknownKeyIsNotDefaultOrg(t *testing.T) {
	s, _ := newTenantServer(t)
	if got := orgFor(t, s, "sk_does_not_exist", ""); got != "<401>" {
		t.Errorf("unknown key resolved to %q, want 401 — falling back to an org "+
			"would give any string access to the default tenant", got)
	}
}

// forward() signs whatever org it is handed, so a handler that reads the header
// directly lets a customer key plus a forged header through. A structural check,
// because a request-level one needs a live placement and skips without it.
func TestNothingReadsTheOrgOverrideHeaderDirectly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "storefast.go" {
			continue // storefast.go holds fastOrg, the one legitimate reader
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, "Header.Get(orgOverride)") {
				t.Errorf("%s:%d reads X-Org-Override directly:\n    %s\n"+
					"  Use fastOrg(r): the header is only trustworthy from an "+
					"operator, and forward() signs whatever org it is handed.",
					f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
