package orchestrator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// revokeServer is a control plane with a real store holding one sandbox placed
// on a node, so visibility can actually be flipped under the gate.
func revokeServer(t *testing.T, visibility string) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "revoke.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() }) //nolint:errcheck // test cleanup
	s := NewServer("fleet-key", "nodetoken")
	s.SetStore(st)
	upsert(s.reg, "n1", "http://node.invalid", "kk", "", nil, "",
		cluster.Capacity{Cores: 8, MemTotalMB: 64000, MemAvailMB: 64000}, s.now())
	s.reg.Place("sb1", "n1")
	setVisibility(t, st, visibility)
	return s, st
}

func setVisibility(t *testing.T, st *store.Store, visibility string) {
	t.Helper()
	if err := st.SaveSandbox(t.Context(), store.SandboxRecord{
		ID: "sb1", Template: "base", OrgID: "acme", State: "running",
		CreatedBy: "key_owner", Visibility: visibility,
	}); err != nil {
		t.Fatalf("save sandbox %q: %v", visibility, err)
	}
}

// opens runs a token through the real gate, authorizePreview.
func opens(s *Server, tok string) bool {
	req := httptest.NewRequest(http.MethodGet, "/preview/sb1/3000/?token="+tok, nil)
	return s.authorizePreview(httptest.NewRecorder(), req, "sb1", 3000)
}

// The defect: a preview link handed out while a sandbox was org-visible kept
// serving after the sandbox was made private. Both halves are asserted here —
// the old link stops, and the owner who holds an API key is not locked out —
// because either one alone passes trivially (refuse everything, or change
// nothing).
func TestUnshareRetractsPreviewLinksButNotTheOwner(t *testing.T) {
	s, st := revokeServer(t, sandbox.VisibilityOrg)
	ctx := context.Background()

	shared := s.previewToken(ctx, "sb1", 3000)
	if shared == "" || !opens(s, shared) {
		t.Fatal("a token minted while the sandbox was org-visible does not open it — this test proves nothing")
	}

	setVisibility(t, st, sandbox.VisibilityPrivate)
	if opens(s, shared) {
		t.Error("the pre-unshare link still opens the preview after the sandbox was made private")
	}

	// The owner is not locked out: a fresh mint under the new generation opens it.
	fresh := s.previewToken(ctx, "sb1", 3000)
	if fresh == "" || !opens(s, fresh) {
		t.Error("the owner cannot mint a working preview token for their own private sandbox")
	}
	if fresh == shared {
		t.Error("the token did not change across a visibility flip, so nothing was revoked")
	}

	// And sharing again retracts the private-era link in turn: one rule, both ways.
	setVisibility(t, st, sandbox.VisibilityOrg)
	if opens(s, fresh) {
		t.Error("a link issued while private still opens the preview after the sandbox was shared")
	}
	if !opens(s, s.previewToken(ctx, "sb1", 3000)) {
		t.Error("a token minted after the share does not open the preview")
	}
}

// A cookie is the same grant as the link and must be retracted with it, or the
// visitor who opened the page once keeps it for the cookie's whole MaxAge.
func TestUnshareRetractsThePreviewCookieToo(t *testing.T) {
	s, st := revokeServer(t, sandbox.VisibilityOrg)
	tok := s.previewToken(context.Background(), "sb1", 3000)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/preview/sb1/3000/?token="+tok, nil)
	if !s.authorizePreview(w, req, "sb1", 3000) {
		t.Fatal("the link did not open while shared")
	}
	cookies := w.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no cookie was set, so this test proves nothing")
	}

	setVisibility(t, st, sandbox.VisibilityPrivate)
	bare := httptest.NewRequest(http.MethodGet, "/preview/sb1/3000/", nil)
	for _, c := range cookies {
		bare.AddCookie(c)
	}
	if s.authorizePreview(httptest.NewRecorder(), bare, "sb1", 3000) {
		t.Error("the cookie from the shared era still opens the preview after the sandbox was made private")
	}
}

// A legacy row written before the visibility column reads back "" and must
// behave exactly as "org", or every pre-migration sandbox loses its preview.
func TestEmptyVisibilityIsTheOrgGeneration(t *testing.T) {
	s, _ := revokeServer(t, "")
	ctx := context.Background()
	gen, ok := s.previewGeneration(ctx, "sb1")
	if !ok || gen != sandbox.VisibilityOrg {
		t.Fatalf("previewGeneration on a legacy row = (%q, %v), want (%q, true)", gen, ok, sandbox.VisibilityOrg)
	}
	if !opens(s, s.previewToken(ctx, "sb1", 3000)) {
		t.Error("a pre-migration row cannot be previewed at all")
	}
}

// A public preview opened every port without a token, the terminal and the
// desktop included, and the node hands the terminal hop envd's shell
// credential: anyone with the URL of a public sandbox had a shell on it.
func TestPublicPreviewDoesNotOpenPlatformPorts(t *testing.T) {
	s, st := revokeServer(t, sandbox.VisibilityOrg)
	if err := st.SetPublicPreview(t.Context(), "sb1", true); err != nil {
		t.Fatalf("set public: %v", err)
	}
	gate := func(port int, query string) bool {
		path := "/preview/sb1/" + strconv.Itoa(port) + "/" + query
		return s.authorizePreview(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil), "sb1", port)
	}
	if !gate(3000, "") {
		t.Fatal("a public sandbox's app port needs a token — this test proves nothing")
	}
	for _, port := range []int{rt.TerminalPort, rt.DesktopPort} {
		if gate(port, "") {
			t.Errorf("port %d of a public sandbox opens without a token", port)
		}
		if gate(port, "?token="+s.previewToken(t.Context(), "sb1", 3000)) {
			t.Errorf("an app-port token opens port %d", port)
		}
		if !gate(port, "?token="+s.previewToken(t.Context(), "sb1", port)) {
			t.Errorf("the owner's token for port %d does not open it", port)
		}
	}
}
