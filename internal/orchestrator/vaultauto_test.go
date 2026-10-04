package orchestrator

import (
	"net/http"
	"sync"
	"testing"
)

// B8: rotating a value reset auto_inject to off, and the only way to flip it
// was to re-send the value, which a sealed secret's owner cannot read back.
func TestAutoInjectSurvivesARotationAndFlipsWithoutTheValue(t *testing.T) {
	srv, st := newVaultServer(t)
	autoOf := func() bool {
		t.Helper()
		list, err := st.ListSecrets(t.Context(), "default")
		if err != nil || len(list) != 1 {
			t.Fatalf("list = %v %v", list, err)
		}
		return list[0].AutoInject
	}

	if code, _ := do(t, srv, "POST", "/v1/secrets", "fleet-key", `{"name":"TOK","value":"v1","auto_inject":true}`); code != http.StatusOK {
		t.Fatalf("put = %d", code)
	}
	if code, out := do(t, srv, "POST", "/v1/secrets", "fleet-key", `{"name":"TOK","value":"v2"}`); code != http.StatusOK || out["auto_inject"] != true {
		t.Fatalf("rotate = %d %v, want auto_inject kept true", code, out)
	}
	if !autoOf() {
		t.Error("a rotation that never mentioned auto_inject turned it off")
	}

	if code, _ := do(t, srv, "PATCH", "/v1/secrets/TOK", "fleet-key", `{"auto_inject":false}`); code != http.StatusOK {
		t.Fatalf("patch = %d", code)
	}
	if autoOf() {
		t.Error("PATCH auto_inject:false left it on")
	}
	if v, _, _ := st.SecretValue(t.Context(), "default", "TOK"); v != "v2" {
		t.Errorf("the toggle changed the value to %q", v)
	}
	if code, _ := do(t, srv, "PATCH", "/v1/secrets/TOK", "fleet-key", `{}`); code != http.StatusBadRequest {
		t.Errorf("patch without auto_inject = %d, want 400", code)
	}
	if code, _ := do(t, srv, "PATCH", "/v1/secrets/NOPE", "fleet-key", `{"auto_inject":true}`); code != http.StatusNotFound {
		t.Errorf("patch of a missing secret = %d, want 404", code)
	}
}

// A rotation read auto_inject and wrote it back, so a PATCH landing between
// the two was silently undone. The keep is now part of the write itself.
func TestAutoInjectToggleIsNotLostToAConcurrentRotation(t *testing.T) {
	srv, st := newVaultServer(t)
	for i := range 40 {
		if code, _ := do(t, srv, "POST", "/v1/secrets", "fleet-key", `{"name":"TOK","value":"v","auto_inject":true}`); code != http.StatusOK {
			t.Fatalf("put = %d", code)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); do(t, srv, "PATCH", "/v1/secrets/TOK", "fleet-key", `{"auto_inject":false}`) }()
		go func() {
			defer wg.Done()
			do(t, srv, "POST", "/v1/secrets", "fleet-key", `{"name":"TOK","value":"rotated"}`)
		}()
		wg.Wait()
		list, err := st.ListSecrets(t.Context(), "default")
		if err != nil || len(list) != 1 {
			t.Fatalf("list = %v %v", list, err)
		}
		if list[0].AutoInject {
			t.Fatalf("round %d: the PATCH to false was lost to a rotation that never mentioned auto_inject", i)
		}
	}
}
