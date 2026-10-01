package orchestrator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// previewAccessFleet is revokeServer's sandbox, created by a real key, behind
// the real routes, plus a second member of the same org.
func previewAccessFleet(t *testing.T) (s *Server, orch *httptest.Server, owner, member string) {
	t.Helper()
	s, st := revokeServer(t, sandbox.VisibilityOrg)
	ownerKey, owner, err := st.CreateKey(t.Context(), "owner", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, member, err = st.CreateKey(t.Context(), "member", "acme"); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveSandbox(t.Context(), store.SandboxRecord{
		ID: "sb1", Template: "base", OrgID: "acme", State: "running", CreatedBy: ownerKey.ID,
	}); err != nil {
		t.Fatal(err)
	}
	orch = httptest.NewServer(s.Routes())
	t.Cleanup(orch.Close)
	return s, orch, owner, member
}

func setPreviewAccess(t *testing.T, orch *httptest.Server, key, body string, headers ...string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, orch.URL+"/v1/sandboxes/sb1/preview-access", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// B24: public was create-only. Turning it off after launch must stop the
// tokenless preview and retract what was handed out while it was on, as
// unsharing does; the owner can mint back in.
func TestPreviewAccessFlipsAndRetractsPublicLinks(t *testing.T) {
	s, orch, owner, member := previewAccessFleet(t)
	ctx := context.Background()
	bare := func(port int) bool {
		return s.authorizePreview(httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/preview/sb1/3000/", nil), "sb1", port)
	}

	if code := setPreviewAccess(t, orch, member, `{"public":true}`); code != http.StatusNotFound {
		t.Errorf("another member flipping it = %d, want 404", code)
	}
	if code := setPreviewAccess(t, orch, owner, `{}`); code != http.StatusBadRequest {
		t.Errorf("no public field = %d, want 400", code)
	}
	if code := setPreviewAccess(t, orch, owner, `{"public":true}`); code != http.StatusOK {
		t.Fatalf("owner turning it on = %d", code)
	}
	if !bare(3000) {
		t.Fatal("a public preview still needs a token")
	}
	if bare(rt.TerminalPort) {
		t.Error("turning the preview public opened the terminal port without a token")
	}
	publicEra := s.previewToken(ctx, "sb1", 3000)

	if code := setPreviewAccess(t, orch, owner, `{"public":false}`); code != http.StatusOK {
		t.Fatalf("owner turning it off = %d", code)
	}
	if bare(3000) {
		t.Error("the preview still opens without a token after public was turned off")
	}
	if opens(s, publicEra) {
		t.Error("a link minted while public still opens after public was turned off")
	}
	if !opens(s, s.previewToken(ctx, "sb1", 3000)) {
		t.Error("the owner cannot mint a working link after turning public off")
	}
	// The operator credential acting for the org may flip it too.
	if code := setPreviewAccess(t, orch, "fleet-key", `{"public":true}`, orgOverride, "acme"); code != http.StatusOK {
		t.Errorf("operator flipping it = %d, want 200", code)
	}
}

// The detail page needs to know which way the switch points.
func TestGetReportsPublicPreview(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"sb1","template":"base","status":"running"}`))
	}))
	t.Cleanup(node.Close)
	s, orch, owner, _ := previewAccessFleet(t)
	if err := upsert(s.reg, "n2", node.URL, "kk", "", nil, "",
		cluster.Capacity{Cores: 8, MemTotalMB: 64000, MemAvailMB: 64000}, s.now()); err != nil {
		t.Fatal(err)
	}
	s.reg.Place("sb1", "n2")

	get := func() any {
		req, _ := http.NewRequest(http.MethodGet, orch.URL+"/v1/sandboxes/sb1", nil)
		req.Header.Set("Authorization", "Bearer "+owner)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var doc map[string]any
		if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&doc) != nil {
			t.Fatalf("get = %d", resp.StatusCode)
		}
		return doc["public_preview"]
	}
	if got := get(); got != false {
		t.Errorf("public_preview = %v, want false", got)
	}
	if code := setPreviewAccess(t, orch, owner, `{"public":true}`); code != http.StatusOK {
		t.Fatalf("turn on = %d", code)
	}
	if got := get(); got != true {
		t.Errorf("public_preview = %v after turning it on, want true", got)
	}
}

// The generation was a value, so off -> on -> off restored the first "off"
// generation and revived every token minted under it. It is a count now.
func TestFlippingBackDoesNotReviveOldPreviewTokens(t *testing.T) {
	s, orch, owner, _ := previewAccessFleet(t)
	ctx := context.Background()
	firstOff := s.previewToken(ctx, "sb1", 3000)
	if !opens(s, firstOff) {
		t.Fatal("a fresh token does not open the preview — this test proves nothing")
	}
	for _, public := range []string{"true", "false"} {
		if code := setPreviewAccess(t, orch, owner, `{"public":`+public+`}`); code != http.StatusOK {
			t.Fatalf("flip to %s = %d", public, code)
		}
	}
	if opens(s, firstOff) {
		t.Error("a token minted before off -> on -> off opens the preview again")
	}
	if !opens(s, s.previewToken(ctx, "sb1", 3000)) {
		t.Error("a token minted after the flips does not open the preview")
	}
	// Re-asserting the current state is not a flip and retracts nothing.
	current := s.previewToken(ctx, "sb1", 3000)
	if code := setPreviewAccess(t, orch, owner, `{"public":false}`); code != http.StatusOK {
		t.Fatalf("no-op flip = %d", code)
	}
	if !opens(s, current) {
		t.Error("setting public to what it already was retracted the owner's link")
	}
}

// The portal mints from what GET says; it must say the generation the gate
// checks, flips included.
func TestGetNamesThePreviewGenerationTheGateChecks(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"sb1","template":"base","status":"running"}`))
	}))
	t.Cleanup(node.Close)
	s, orch, owner, _ := previewAccessFleet(t)
	if err := upsert(s.reg, "n2", node.URL, "kk", "", nil, "",
		cluster.Capacity{Cores: 8, MemTotalMB: 64000, MemAvailMB: 64000}, s.now()); err != nil {
		t.Fatal(err)
	}
	s.reg.Place("sb1", "n2")
	for _, public := range []string{"true", "false"} {
		if code := setPreviewAccess(t, orch, owner, `{"public":`+public+`}`); code != http.StatusOK {
			t.Fatalf("flip = %d", code)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, orch.URL+"/v1/sandboxes/sb1", nil)
	req.Header.Set("Authorization", "Bearer "+owner)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc map[string]any
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&doc) != nil {
		t.Fatalf("get = %d", resp.StatusCode)
	}
	want, _ := s.previewGeneration(context.Background(), "sb1")
	if doc["preview_generation"] != want || want == sandbox.PreviewGeneration("org", false, 0) {
		t.Errorf("preview_generation = %v, want %q (after two flips)", doc["preview_generation"], want)
	}
}
