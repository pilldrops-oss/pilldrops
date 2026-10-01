package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	neturl "net/url"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/orgassert"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// doAs issues a request the way the control plane does: an operator key, a
// signed org assertion, and optionally the customer key the assertion names.
// sentKeyID is what goes in the header; signedKeyID is what the signature
// covers, so the two differ when a forgery is being tested.
func doAs(t *testing.T, method, url, key, org, signedKeyID, signedRole, sentKeyID, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+key)
	if org != "" {
		u, err := neturl.Parse(url)
		if err != nil {
			t.Fatalf("parse %q: %v", url, err)
		}
		var principal []string
		if signedKeyID != "" {
			principal = []string{signedKeyID, signedRole}
		}
		req.Header.Set("X-Org-Override",
			orgassert.Sign([]byte(testOrgOverrideSecret), org, testNodeID, method, u.Path, time.Now(), principal...))
	}
	if sentKeyID != "" {
		req.Header.Set("X-Key-Id", sentKeyID)
		req.Header.Set("X-Key-Role", signedRole)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func str(out map[string]any, field string) string {
	v, _ := out[field].(string)
	return v
}

// twoMembersAndAnOperator mints K1, K2 and KOP in one org and returns their
// plaintext keys. Every visibility check needs all three.
func twoMembersAndAnOperator(t *testing.T, st *store.Store) (k1, k2, kop string) {
	t.Helper()
	var err error
	if _, k1, err = st.CreateKeyWithRole(t.Context(), "K1", "default", "user"); err != nil {
		t.Fatalf("K1: %v", err)
	}
	if _, k2, err = st.CreateKeyWithRole(t.Context(), "K2", "default", "user"); err != nil {
		t.Fatalf("K2: %v", err)
	}
	if _, kop, err = st.CreateKeyWithRole(t.Context(), "KOP", "default", "operator"); err != nil {
		t.Fatalf("KOP: %v", err)
	}
	return k1, k2, kop
}

// listIDs returns the sandbox ids GET /v1/sandboxes shows this key.
func listIDs(t *testing.T, srv, key string) map[string]bool {
	t.Helper()
	code, out := do(t, "GET", srv+"/v1/sandboxes", key, "")
	if code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	ids := map[string]bool{}
	arr, _ := out["sandboxes"].([]any)
	for _, row := range arr {
		m, _ := row.(map[string]any)
		if id, _ := m["id"].(string); id != "" {
			ids[id] = true
		}
	}
	return ids
}

// A private sandbox is its creator's and an operator's; to every other member
// of the same org it does not exist.
func TestPrivateSandboxIsInvisibleToOtherMembers(t *testing.T) {
	srv, st, _ := newTestServerWithStore(t)
	k1, k2, kop := twoMembersAndAnOperator(t, st)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", k1, `{"template":"base","visibility":"private"}`)
	if code != http.StatusCreated {
		t.Fatalf("private create = %d (%v)", code, out)
	}
	id := str(out, "id")
	if str(out, "visibility") != "private" {
		t.Fatalf("visibility = %q, want private", str(out, "visibility"))
	}

	// K2: absent everywhere, and refused as if it were not there.
	if listIDs(t, srv.URL, k2)[id] {
		t.Error("K2 lists another member's private sandbox")
	}
	for _, probe := range []struct{ method, path, body string }{
		{"GET", "/v1/sandboxes/" + id, ""},
		{"POST", "/v1/sandboxes/" + id + "/exec", `{"cmd":"true"}`},
		{"POST", "/v1/sandboxes/" + id + "/visibility", `{"visibility":"org"}`},
		// Both of these answered any key in the org, so a private sandbox was
		// still enumerable — what it is serving, and whether it is up — by
		// someone who may not see that it exists.
		{"GET", "/v1/sandboxes/" + id + "/ports", ""},
		{"GET", "/v1/sandboxes/" + id + "/preview-status", ""},
		{"GET", "/v1/sandboxes/" + id + "/deploy", ""},
		{"DELETE", "/v1/sandboxes/" + id, ""},
	} {
		if code, _ := do(t, probe.method, srv.URL+probe.path, k2, probe.body); code != http.StatusNotFound {
			t.Errorf("K2 %s %s = %d, want 404", probe.method, probe.path, code)
		}
	}
	// Positive control for the two above: a guard stuck on is as wrong as one
	// stuck off, so the creator must still get an answer. Not asserted as 200 —
	// what the fake runtime reports about ports is not this test's question,
	// only that the ownership rule let the request through.
	for _, path := range []string{"/v1/sandboxes/" + id + "/ports", "/v1/sandboxes/" + id + "/preview-status", "/v1/sandboxes/" + id + "/deploy"} {
		if code, _ := do(t, "GET", srv.URL+path, k1, ""); code == http.StatusNotFound {
			t.Errorf("the creator GET %s = 404: the guard refuses its owner too", path)
		}
	}
	// Positive controls: the creator and the operator get through, so the 404s
	// above are the rule and not a broken sandbox.
	if !listIDs(t, srv.URL, k1)[id] {
		t.Error("the creator does not see its own private sandbox")
	}
	if !listIDs(t, srv.URL, kop)[id] {
		t.Error("an operator does not see a private sandbox")
	}
	if code, _ := do(t, "GET", srv.URL+"/v1/sandboxes/"+id, k1, ""); code != http.StatusOK {
		t.Errorf("creator GET = %d, want 200", code)
	}
	if code, _ := do(t, "GET", srv.URL+"/v1/sandboxes/"+id, kop, ""); code != http.StatusOK {
		t.Errorf("operator GET = %d, want 200", code)
	}
}

// A paused sandbox is judged from the record: the daemon is never consulted,
// so pausing does not leak it into another member's list.
func TestPausedPrivateSandboxStaysHidden(t *testing.T) {
	srv, st, _ := newTestServerWithStore(t)
	k1, k2, _ := twoMembersAndAnOperator(t, st)

	_, out := do(t, "POST", srv.URL+"/v1/sandboxes", k1, `{"template":"base","visibility":"private"}`)
	id := str(out, "id")
	if code, _ := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/pause", k1, ""); code != http.StatusOK {
		t.Fatalf("pause failed, so the test would prove nothing")
	}
	if listIDs(t, srv.URL, k2)[id] {
		t.Error("K2 lists a paused private sandbox")
	}
	if !listIDs(t, srv.URL, k1)[id] {
		t.Error("the creator lost its paused sandbox")
	}
	if code, _ := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/resume", k1, ""); code != http.StatusOK {
		t.Error("the creator cannot resume its own paused private sandbox")
	}
}

// Visible is not the same as yours: a member may see an org sandbox and still
// not flip it, and the refusal is a 404 so it does not confirm who owns what.
func TestOnlyTheCreatorOrAnOperatorFlipsVisibility(t *testing.T) {
	srv, st, _ := newTestServerWithStore(t)
	k1, k2, kop := twoMembersAndAnOperator(t, st)

	_, out := do(t, "POST", srv.URL+"/v1/sandboxes", k1, `{"template":"base"}`)
	id := str(out, "id")
	if code, _ := do(t, "GET", srv.URL+"/v1/sandboxes/"+id, k2, ""); code != http.StatusOK {
		t.Fatal("K2 cannot see an org-visible sandbox, so the rest proves nothing")
	}
	if code, _ := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/visibility", k2, `{"visibility":"private"}`); code != http.StatusNotFound {
		t.Errorf("K2 flipping someone else's sandbox = %d, want 404", code)
	}
	if code, got := do(t, "GET", srv.URL+"/v1/sandboxes/"+id, k2, ""); code != http.StatusOK || str(got, "visibility") != "org" {
		t.Errorf("after the refused flip: code=%d visibility=%q, want 200/org", code, str(got, "visibility"))
	}

	code, flip := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/visibility", k1, `{"visibility":"private"}`)
	if code != http.StatusOK || str(flip, "visibility") != "private" {
		t.Fatalf("creator flip = %d (%v)", code, flip)
	}
	if flip["relogin"] != true {
		t.Error("unshare does not say relogin: true — nothing is restored, and the response should say so")
	}
	if code, _ := do(t, "GET", srv.URL+"/v1/sandboxes/"+id, k2, ""); code != http.StatusNotFound {
		t.Errorf("K2 still sees the sandbox after it went private = %d, want 404", code)
	}
	// An operator may flip it back.
	if code, _ := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/visibility", kop, `{"visibility":"org"}`); code != http.StatusOK {
		t.Error("an operator cannot flip a member's sandbox")
	}
	if code, _ := do(t, "GET", srv.URL+"/v1/sandboxes/"+id, k2, ""); code != http.StatusOK {
		t.Error("the sandbox did not come back for K2 after being shared again")
	}
}

// The creator is the caller's own key on a direct create, and every sandbox is
// org-visible unless asked otherwise: the behaviour before this change.
func TestCreateStampsTheCallersKeyAndDefaultsToOrg(t *testing.T) {
	srv, st, userKey := newTestServerWithStore(t)
	keys, err := st.ListKeys(t.Context(), "default")
	if err != nil || len(keys) != 1 {
		t.Fatalf("ListKeys = %d keys, err=%v", len(keys), err)
	}

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", userKey, `{"template":"base"}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d (%v)", code, out)
	}
	if got := str(out, "created_by"); got != keys[0].ID {
		t.Errorf("created_by = %q, want the calling key %q", got, keys[0].ID)
	}
	if got := str(out, "visibility"); got != "org" {
		t.Errorf("visibility = %q, want org", got)
	}

	code, got := do(t, "GET", srv.URL+"/v1/sandboxes/"+str(out, "id"), userKey, "")
	if code != http.StatusOK || str(got, "created_by") != keys[0].ID || str(got, "visibility") != "org" {
		t.Errorf("GET = %d created_by=%q visibility=%q", code, str(got, "created_by"), str(got, "visibility"))
	}
}

// Through the control plane the creator is the key the signed assertion names,
// not the operator key on the wire. Missing this is the quiet failure: a
// sandbox created through the proxy would belong to nobody and be org-visible
// however private its creator asked for.
func TestForwardedCreateIsAttributedToTheAssertedKey(t *testing.T) {
	srv, st, _ := newTestServerWithStore(t)
	_, opKey, err := st.CreateKeyWithRole(t.Context(), "orch", "default", "operator")
	if err != nil {
		t.Fatalf("operator key: %v", err)
	}

	code, out := doAs(t, "POST", srv.URL+"/v1/sandboxes", opKey, "u:cust1", "key_alice", "", "key_alice", `{"template":"base"}`)
	if code != http.StatusCreated {
		t.Fatalf("forwarded create = %d (%v)", code, out)
	}
	if got := str(out, "created_by"); got != "key_alice" {
		t.Fatalf("created_by = %q, want key_alice — the hop dropped the caller's identity", got)
	}
}

// A key id the assertion does not cover is nobody's: the request still runs,
// and what it creates belongs to no one rather than to the named key.
func TestAnUnsignedKeyIdIsIgnored(t *testing.T) {
	srv, st, userKey := newTestServerWithStore(t)
	_, opKey, err := st.CreateKeyWithRole(t.Context(), "orch", "default", "operator")
	if err != nil {
		t.Fatalf("operator key: %v", err)
	}

	// Forged: signed for nobody, header claims to be Alice.
	code, out := doAs(t, "POST", srv.URL+"/v1/sandboxes", opKey, "u:cust1", "", "", "key_alice", `{"template":"base"}`)
	if code != http.StatusCreated {
		t.Fatalf("create with a forged key id = %d (%v)", code, out)
	}
	if got := str(out, "created_by"); got != "" {
		t.Fatalf("created_by = %q from an unsigned header, want empty", got)
	}

	// A tenant hitting the node directly cannot assert a key id either.
	req, _ := http.NewRequest("POST", srv.URL+"/v1/sandboxes", bytes.NewBufferString(`{"template":"base"}`))
	req.Header.Set("Authorization", "Bearer "+userKey)
	req.Header.Set("X-Key-Id", "zzz")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("direct create: %v", err)
	}
	defer resp.Body.Close()
	var direct map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&direct)
	if got := str(direct, "created_by"); got == "zzz" || got == "" {
		t.Fatalf("direct create with a bare X-Key-Id: created_by = %q, want the caller's own key id", got)
	}
}

// Preview tokens are bound to a flip count, so the node counts each real flip:
// org -> private -> org must not land back on the generation it started at.
func TestVisibilityFlipsAreCounted(t *testing.T) {
	srv, st, key := newTestServerWithStore(t)
	_, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	id := str(out, "id")
	for _, v := range []string{"private", "org", "org"} {
		if code, got := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/visibility", key, `{"visibility":"`+v+`"}`); code != http.StatusOK {
			t.Fatalf("flip to %s = %d (%v)", v, code, got)
		}
	}
	// The last one re-asserts org: not a flip.
	if n, err := st.PreviewGeneration(t.Context(), id); err != nil || n != 2 {
		t.Errorf("preview generation = %d, %v; want 2 flips counted", n, err)
	}
}
