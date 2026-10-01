package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
)

// B8 on the portal's own vault: a rotation reset auto_inject, and flipping it
// needed the value again.
func TestPortalAutoInjectSurvivesARotationAndFlipsWithoutTheValue(t *testing.T) {
	vs := testVault(t)
	r := chi.NewRouter()
	registerVaultRoutes(r, vs, func(*http.Request) string { return "u:alice" })
	send := func(method, path, body string) int {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w.Code
	}
	autoOf := func() bool {
		list := vs.List("u:alice")
		if len(list) != 1 {
			t.Fatalf("list = %v", list)
		}
		return list[0].AutoInject
	}

	if code := send("POST", "/v1/secrets", `{"name":"TOK","value":"v1","auto_inject":true}`); code != 200 {
		t.Fatalf("put = %d", code)
	}
	if code := send("POST", "/v1/secrets", `{"name":"TOK","value":"v2"}`); code != 200 {
		t.Fatalf("rotate = %d", code)
	}
	if !autoOf() {
		t.Error("a rotation that never mentioned auto_inject turned it off")
	}
	if code := send("PATCH", "/v1/secrets/TOK", `{"auto_inject":false}`); code != 200 {
		t.Fatalf("patch = %d", code)
	}
	if autoOf() {
		t.Error("PATCH auto_inject:false left it on")
	}
	if v, _ := vs.Value("u:alice", "TOK"); v != "v2" {
		t.Errorf("the toggle changed the value to %q", v)
	}
	if code := send("PATCH", "/v1/secrets/NOPE", `{"auto_inject":true}`); code != 404 {
		t.Errorf("patch of a missing secret = %d, want 404", code)
	}
}

// The rotation's keep is decided under the vault lock, so a PATCH racing it
// is never overwritten by a value read before it.
func TestPortalAutoInjectToggleIsNotLostToAConcurrentRotation(t *testing.T) {
	vs := testVault(t)
	r := chi.NewRouter()
	registerVaultRoutes(r, vs, func(*http.Request) string { return "u:alice" })
	send := func(method, path, body string) {
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, strings.NewReader(body)))
	}
	for i := range 40 {
		send("POST", "/v1/secrets", `{"name":"TOK","value":"v","auto_inject":true}`)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); send("PATCH", "/v1/secrets/TOK", `{"auto_inject":false}`) }()
		go func() { defer wg.Done(); send("POST", "/v1/secrets", `{"name":"TOK","value":"rotated"}`) }()
		wg.Wait()
		if vs.List("u:alice")[0].AutoInject {
			t.Fatalf("round %d: the PATCH to false was lost to a rotation", i)
		}
	}
}
