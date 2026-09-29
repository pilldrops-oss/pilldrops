package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/previewtoken"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/process"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// testPreviewSecret is the shared secret preview-auth tests mint tokens against.
const testPreviewSecret = "test-preview-secret"

// newDaemonForPreviewTest builds a bare daemon and store so tests can seed a
// sandbox with a known owner org.
func newDaemonForPreviewTest(t *testing.T) (*daemon.Daemon, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	d := daemon.New(daemon.Config{
		Runtime:     process.New(filepath.Join(dir, "sb")),
		Store:       daemon.StoreAdapter{S: st},
		SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	return d, st
}

// stubNext records whether requirePreviewToken let the request through.
func stubNext() (http.Handler, *bool) {
	called := new(bool)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*called = true
		w.WriteHeader(http.StatusOK)
	})
	return h, called
}

func TestPreviewTokenRequiredWhenSecretConfigured(t *testing.T) {
	d, st := newDaemonForPreviewTest(t)
	s := New(d, st)
	s.SetPreviewSecret([]byte(testPreviewSecret))
	sb, err := d.Create(context.Background(), daemon.CreateSpec{Template: "base", OrgID: "u:cust1"})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	next, called := stubNext()
	h := s.requirePreviewToken(next)

	cases := []struct {
		name  string
		token string
		want  int
	}{
		{"no token", "", http.StatusForbidden},
		{"wrong token", "not-a-real-token", http.StatusForbidden},
		{"correct org-bound token", previewtoken.Token([]byte(testPreviewSecret), sb.ID, 8000, "u:cust1"), http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			*called = false
			path := "/preview/" + sb.ID + "/8000/"
			if tc.token != "" {
				path += "?ptok=" + tc.token
			}
			req := httptest.NewRequest("GET", path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Errorf("status = %d, want %d", w.Code, tc.want)
			}
			wantCalled := tc.want == http.StatusOK
			if *called != wantCalled {
				t.Errorf("next called = %v, want %v", *called, wantCalled)
			}
		})
	}
}

// A validly signed token for the right id and port but the wrong org is refused.
func TestPreviewTokenCrossOrgRefused(t *testing.T) {
	d, st := newDaemonForPreviewTest(t)
	s := New(d, st)
	s.SetPreviewSecret([]byte(testPreviewSecret))
	sb, err := d.Create(context.Background(), daemon.CreateSpec{Template: "base", OrgID: "u:cust1"})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	next, called := stubNext()
	h := s.requirePreviewToken(next)

	tok := previewtoken.Token([]byte(testPreviewSecret), sb.ID, 8000, "u:other-org")
	req := httptest.NewRequest("GET", "/preview/"+sb.ID+"/8000/?ptok="+tok, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-org token = %d, want 403", w.Code)
	}
	if *called {
		t.Fatal("cross-org token reached the wrapped handler")
	}
}

// An org-blind token (the orchestrator's fallback) is accepted: it still needs
// the shared secret to forge.
func TestPreviewOrgBlindTokenStillHMACGated(t *testing.T) {
	d, st := newDaemonForPreviewTest(t)
	s := New(d, st)
	s.SetPreviewSecret([]byte(testPreviewSecret))
	sb, err := d.Create(context.Background(), daemon.CreateSpec{Template: "base", OrgID: "u:cust1"})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	next, called := stubNext()
	h := s.requirePreviewToken(next)

	blind := previewtoken.Token([]byte(testPreviewSecret), sb.ID, 8000, "")
	req := httptest.NewRequest("GET", "/preview/"+sb.ID+"/8000/?ptok="+blind, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("org-blind token (still secret-bound) = %d, want 200", w.Code)
	}
	if !*called {
		t.Fatal("org-blind token did not reach the wrapped handler")
	}
}

func TestPreviewFailsClosedWhenSecretUnset(t *testing.T) {
	d, st := newDaemonForPreviewTest(t)
	s := New(d, st) // SetPreviewSecret deliberately NOT called
	sb, err := d.Create(context.Background(), daemon.CreateSpec{Template: "base", OrgID: "u:cust1"})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	next, called := stubNext()
	h := s.requirePreviewToken(next)

	req := httptest.NewRequest("GET", "/preview/"+sb.ID+"/8000/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatalf("no preview secret, no DEV_INSECURE: status = %d, want refused (not 200)", w.Code)
	}
	if *called {
		t.Fatal("request reached the wrapped handler with no preview secret configured")
	}
}

// DEV_INSECURE opens the gate only when no secret exists to gate with.
func TestPreviewDevInsecureFallsBackWhenUnconfigured(t *testing.T) {
	t.Setenv("DEV_INSECURE", "1")
	d, st := newDaemonForPreviewTest(t)
	s := New(d, st) // SetPreviewSecret deliberately NOT called
	sb, err := d.Create(context.Background(), daemon.CreateSpec{Template: "base", OrgID: "u:cust1"})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	next, called := stubNext()
	h := s.requirePreviewToken(next)

	req := httptest.NewRequest("GET", "/preview/"+sb.ID+"/8000/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("DEV_INSECURE with no secret configured: status = %d, want 200 (dev escape hatch honored)", w.Code)
	}
	if !*called {
		t.Fatal("DEV_INSECURE escape hatch did not let the request through")
	}
}

func TestPreviewTokenUnknownSandboxRefused(t *testing.T) {
	d, st := newDaemonForPreviewTest(t)
	s := New(d, st)
	s.SetPreviewSecret([]byte(testPreviewSecret))
	next, called := stubNext()
	h := s.requirePreviewToken(next)

	tok := previewtoken.Token([]byte(testPreviewSecret), "does-not-exist", 8000, "u:cust1")
	req := httptest.NewRequest("GET", "/preview/does-not-exist/8000/?ptok="+tok, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown sandbox = %d, want 404", w.Code)
	}
	if *called {
		t.Fatal("unknown sandbox reached the wrapped handler")
	}
}

// VerifyPreview must make the same decision as requirePreviewToken so the
// subdomain form cannot drift from the path form.
func TestVerifyPreviewAgreesWithRequirePreviewToken(t *testing.T) {
	d, st := newDaemonForPreviewTest(t)
	s := New(d, st)
	s.SetPreviewSecret([]byte(testPreviewSecret))
	sb, err := d.Create(context.Background(), daemon.CreateSpec{Template: "base", OrgID: "u:cust1"})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	cases := []struct {
		name  string
		token string
		want  bool
	}{
		{"no token", "", false},
		{"wrong token", "not-a-real-token", false},
		{"cross-org token", previewtoken.Token([]byte(testPreviewSecret), sb.ID, 8000, "u:other-org"), false},
		{"correct org-bound token", previewtoken.Token([]byte(testPreviewSecret), sb.ID, 8000, "u:cust1"), true},
		{"org-blind token", previewtoken.Token([]byte(testPreviewSecret), sb.ID, 8000, ""), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			org, got := s.VerifyPreview(sb.ID, 8000, tc.token)
			if got != tc.want {
				t.Errorf("VerifyPreview(...) = %v, want %v", got, tc.want)
			}
			// An allowed verdict with no org would let PreviewTarget treat "" as a wildcard.
			if tc.want && org != "u:cust1" {
				t.Errorf("VerifyPreview allowed the request but returned org %q, want the sandbox's owner org", org)
			}
			if !tc.want && org != "" {
				t.Errorf("VerifyPreview refused the request but still returned org %q", org)
			}
		})
	}
}

func TestVerifyPreviewFailsClosedWhenSecretUnset(t *testing.T) {
	d, st := newDaemonForPreviewTest(t)
	s := New(d, st) // SetPreviewSecret deliberately NOT called
	sb, err := d.Create(context.Background(), daemon.CreateSpec{Template: "base", OrgID: "u:cust1"})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	if org, ok := s.VerifyPreview(sb.ID, 8000, ""); ok || org != "" {
		t.Fatalf("VerifyPreview allowed through with no secret configured and no dev hatch: (%q, %v)", org, ok)
	}
}
