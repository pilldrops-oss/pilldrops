package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// authResolver is a stubResolver that also carries a terminal credential.
type authResolver struct {
	stubResolver
	token   string
	err     error
	askedID string
}

func (a *authResolver) TerminalToken(_ context.Context, id string) (string, error) {
	a.askedID = id
	return a.token, a.err
}

// echoUpstream records the headers the proxy sent it.
func echoUpstream(t *testing.T) (target string, seen *http.Header) {
	t.Helper()
	var h http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h = r.Header.Clone()
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), &h
}

func TestTerminalPortForwardsEnvdToken(t *testing.T) {
	target, seen := echoUpstream(t)
	res := &authResolver{stubResolver: stubResolver{target: target}, token: "sk-envd-secret"}

	req := gatedReq("GET", "/preview/sb_1/7681/ws")
	Handler(res).ServeHTTP(httptest.NewRecorder(), req)

	if got := seen.Get(sbtoken.MetadataKey); got != "sk-envd-secret" {
		t.Fatalf("upstream saw %s = %q, want the envd token", sbtoken.MetadataKey, got)
	}
	if res.askedID != "sb_1" {
		t.Errorf("credential looked up for %q, want sb_1", res.askedID)
	}
}

// The credential opens a root shell, and an app can read its own request headers.
func TestAppPortDoesNotReceiveTheEnvdToken(t *testing.T) {
	target, seen := echoUpstream(t)
	res := &authResolver{stubResolver: stubResolver{target: target}, token: "sk-envd-secret"}

	req := gatedReq("GET", "/preview/sb_1/8000/")
	Handler(res).ServeHTTP(httptest.NewRecorder(), req)

	if got := seen.Get(sbtoken.MetadataKey); got != "" {
		t.Fatalf("the customer's app received the terminal credential: %q", got)
	}
}

func TestTerminalUpgradeIsReOriginedToTheTarget(t *testing.T) {
	target, seen := echoUpstream(t)
	res := &authResolver{stubResolver: stubResolver{target: target}, token: "tok"}

	req := gatedReq("GET", "/preview/sb_1/7681/ws")
	req.Header.Set("Origin", "http://dashboard.example.com")
	Handler(res).ServeHTTP(httptest.NewRecorder(), req)

	if got := seen.Get("Origin"); got != "http://"+target {
		t.Fatalf("Origin = %q, want http://%s — envd would refuse the upgrade", got, target)
	}
}

// Rewriting an app port's Origin would hide a real cross-origin request from the app.
func TestAppPortOriginIsUntouched(t *testing.T) {
	target, seen := echoUpstream(t)
	res := &authResolver{stubResolver: stubResolver{target: target}, token: "tok"}

	req := gatedReq("GET", "/preview/sb_1/8000/")
	req.Header.Set("Origin", "http://dashboard.example.com")
	Handler(res).ServeHTTP(httptest.NewRecorder(), req)

	if got := seen.Get("Origin"); got != "http://dashboard.example.com" {
		t.Fatalf("Origin = %q, want the client's own origin preserved", got)
	}
}

func TestTerminalWorksWithoutTheOptionalCapability(t *testing.T) {
	target, seen := echoUpstream(t)
	res := &stubResolver{target: target}

	req := gatedReq("GET", "/preview/sb_1/7681/ws")
	rec := httptest.NewRecorder()
	Handler(res).ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code = %d, want 200 — a Resolver without the capability must still proxy", rec.Code)
	}
	if got := seen.Get(sbtoken.MetadataKey); got != "" {
		t.Fatalf("forwarded a credential it could not have had: %q", got)
	}
}

// envd answers the auth question itself, so the page is still worth serving.
func TestTerminalProxiesWhenTheCredentialLookupFails(t *testing.T) {
	target, seen := echoUpstream(t)
	res := &authResolver{
		stubResolver: stubResolver{target: target},
		err:          rt.ErrUnsupported,
	}

	req := gatedReq("GET", "/preview/sb_1/7681/ws")
	rec := httptest.NewRecorder()
	Handler(res).ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if got := seen.Get(sbtoken.MetadataKey); got != "" {
		t.Fatalf("forwarded a token from a failed lookup: %q", got)
	}
}
