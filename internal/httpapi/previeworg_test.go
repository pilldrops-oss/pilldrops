package httpapi

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/previewtoken"
	"github.com/pilldrops-oss/pilldrops/internal/proxy"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
)

// requirePreviewToken must attach the org it verified, or the resolver refuses
// every preview.

// TestGatePassesTheVerifiedOrgDownstream reads the org back through the real
// proxy handler rather than an internal representation.
func TestGatePassesTheVerifiedOrgDownstream(t *testing.T) {
	d, st := newDaemonForPreviewTest(t)
	s := New(d, st)
	s.SetPreviewSecret([]byte(testPreviewSecret))
	sb, err := d.Create(context.Background(), daemon.CreateSpec{Template: "base", OrgID: "u:cust1"})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}

	rec := &recordingResolver{}
	h := s.requirePreviewToken(proxy.Handler(rec))

	tok := previewtoken.Token([]byte(testPreviewSecret), sb.ID, 8000, "u:cust1")
	req := httptest.NewRequest("GET", "/preview/"+sb.ID+"/8000/?ptok="+tok, nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if rec.org != "u:cust1" {
		t.Fatalf("the resolver was called with org %q, want the sandbox's owner org. "+
			"An empty value here means the gate authorized the request and then dropped "+
			"the identity it authorized, which the resolver refuses — every preview 404s.", rec.org)
	}
}

// A refused request must not reach the resolver at all.
func TestRefusedRequestCarriesNoOrg(t *testing.T) {
	d, st := newDaemonForPreviewTest(t)
	s := New(d, st)
	s.SetPreviewSecret([]byte(testPreviewSecret))
	sb, err := d.Create(context.Background(), daemon.CreateSpec{Template: "base", OrgID: "u:cust1"})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	rec := &recordingResolver{}
	h := s.requirePreviewToken(proxy.Handler(rec))

	// Correctly signed for this id and port, so only the org binding can refuse it.
	tok := previewtoken.Token([]byte(testPreviewSecret), sb.ID, 8000, "u:other")
	req := httptest.NewRequest("GET", "/preview/"+sb.ID+"/8000/?ptok="+tok, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("cross-org token: status = %d, want 403", w.Code)
	}
	if rec.called {
		t.Errorf("a refused request still reached the resolver, with org %q", rec.org)
	}

	// Positive control through the same handler.
	good := previewtoken.Token([]byte(testPreviewSecret), sb.ID, 8000, "u:cust1")
	h.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest("GET", "/preview/"+sb.ID+"/8000/?ptok="+good, nil))
	if !rec.called {
		t.Fatal("control: a valid token did not reach the resolver either; the assertion above proves nothing")
	}
}

// Only a hop signed by the control plane may name the public host. A node cannot
// mint that token, so nobody reaching a node directly can pick what the app is told.
func TestPreviewAppIsToldThePublicHostOnlyByTheControlPlane(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	var gotHost, gotProto string
	app := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotHost, gotProto = r.Header.Get("X-Forwarded-Host"), r.Header.Get("X-Forwarded-Proto")
	}))
	t.Cleanup(app.Close)
	resolver := fixedResolver(strings.TrimPrefix(app.URL, "http://"))

	for _, tc := range []struct {
		name                string
		asym                bool
		wantHost, wantProto string
	}{
		{name: "signed control-plane hop", asym: true, wantHost: "app.example", wantProto: "https"},
		{name: "direct to a standalone node", wantHost: "node.example:18080", wantProto: "http"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, st := newDaemonForPreviewTest(t)
			s := New(d, st)
			s.SetPreviewSecret([]byte(testPreviewSecret))
			sb, err := d.Create(context.Background(), daemon.CreateSpec{Template: "base", OrgID: "u:cust1"})
			if err != nil {
				t.Fatalf("create sandbox: %v", err)
			}
			tok := previewtoken.Token([]byte(testPreviewSecret), sb.ID, 8000, "u:cust1")
			if tc.asym {
				s.SetOrgAssertPublicKey(pub)
				tok = previewtoken.TokenAsym(priv, sb.ID, 8000, "u:cust1", time.Now().Add(time.Minute))
			}
			req := httptest.NewRequest("GET", "/preview/"+sb.ID+"/8000/?ptok="+tok, nil)
			req.Host = "node.example:18080"
			req.Header.Set("X-Forwarded-Host", "app.example")
			req.Header.Set("X-Forwarded-Proto", "https")
			w := httptest.NewRecorder()
			s.requirePreviewToken(proxy.Handler(resolver)).ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body %q", w.Code, w.Body.String())
			}
			if gotHost != tc.wantHost || gotProto != tc.wantProto {
				t.Fatalf("app told host %q proto %q, want %q %q", gotHost, gotProto, tc.wantHost, tc.wantProto)
			}
		})
	}
}

// fixedResolver sends every preview to one address.
type fixedResolver string

func (r fixedResolver) PreviewTarget(context.Context, string, int, string) (string, error) {
	return string(r), nil
}

// recordingResolver records its arguments; 127.0.0.1:1 refuses instantly so
// the handler takes its error path.
type recordingResolver struct {
	called bool
	org    string
}

func (r *recordingResolver) PreviewTarget(_ context.Context, _ string, _ int, org string) (string, error) {
	r.called, r.org = true, org
	return "127.0.0.1:1", nil
}
