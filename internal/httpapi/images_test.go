package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func doJSON(t *testing.T, method, url, key string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, rdr)
	req.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	b, _ := io.ReadAll(resp.Body)
	if len(b) > 0 {
		_ = json.Unmarshal(b, &out)
	}
	return resp.StatusCode, out
}

func TestImagesListAndPatch(t *testing.T) {
	srv, st, key := newTestServerWithStore(t)
	// Catalog flags are global, so writing them needs an operator key.
	_, opKey, err := st.CreateKeyWithRole(t.Context(), "op", "default", "operator")
	if err != nil {
		t.Fatalf("operator key: %v", err)
	}

	code, out := doJSON(t, "GET", srv.URL+"/v1/images", key, nil)
	if code != 200 {
		t.Fatalf("GET /v1/images = %d, want 200", code)
	}
	imgs, ok := out["images"].([]any)
	if !ok || len(imgs) == 0 {
		t.Fatalf("images empty: %v", out)
	}
	first := imgs[0].(map[string]any)
	if first["published"] != true {
		t.Fatalf("default published = %v, want true", first["published"])
	}
	if first["featured"] != false {
		t.Fatalf("default featured = %v, want false", first["featured"])
	}

	code, patched := doJSON(t, "PATCH", srv.URL+"/v1/images/base", opKey,
		map[string]any{"published": false, "featured": true})
	if code != 200 {
		t.Fatalf("PATCH /v1/images/base = %d, want 200", code)
	}
	if patched["published"] != false || patched["featured"] != true {
		t.Fatalf("patched flags not applied: %v", patched)
	}

	// Unpublished images are hidden from /v1/templates but still listed in /v1/images.
	req, _ := http.NewRequest("GET", srv.URL+"/v1/templates", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, _ := http.DefaultClient.Do(req)
	var tmpls []map[string]any
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	_ = json.Unmarshal(b, &tmpls)
	for _, tm := range tmpls {
		if tm["id"] == "base" {
			t.Fatalf("unpublished 'base' still visible in /v1/templates")
		}
	}

	_, out = doJSON(t, "GET", srv.URL+"/v1/images", key, nil)
	found := false
	for _, raw := range out["images"].([]any) {
		img := raw.(map[string]any)
		if img["id"] == "base" {
			found = true
			if img["published"] != false || img["featured"] != true {
				t.Fatalf("admin view flags wrong: %v", img)
			}
		}
	}
	if !found {
		t.Fatal("base missing from admin images list after unpublish")
	}
}

func TestImagePatchUnknown(t *testing.T) {
	srv, st, _ := newTestServerWithStore(t)
	_, opKey, err := st.CreateKeyWithRole(t.Context(), "op", "default", "operator")
	if err != nil {
		t.Fatalf("operator key: %v", err)
	}
	if code, _ := doJSON(t, "PATCH", srv.URL+"/v1/images/nope-not-real", opKey,
		map[string]any{"published": true}); code != 404 {
		t.Fatalf("PATCH unknown image = %d, want 404", code)
	}
}

// Image flags are one global row per image, so a user key must not be able to write them.
func TestImagePatchIsOperatorOnly(t *testing.T) {
	srv, st, userKey := newTestServerWithStore(t)

	code, _ := doJSON(t, "PATCH", srv.URL+"/v1/images/base", userKey,
		map[string]any{"published": false})
	if code != http.StatusForbidden {
		t.Fatalf("user key PATCH /v1/images/base = %d, want 403", code)
	}
	flags, err := st.GetImageFlags(t.Context())
	if err != nil {
		t.Fatalf("flags: %v", err)
	}
	if f, ok := flags["base"]; ok && f.Published != nil && !*f.Published {
		t.Fatal("a user key unpublished 'base' fleet-wide despite the 403")
	}

	_, opKey, err := st.CreateKeyWithRole(t.Context(), "op", "default", "operator")
	if err != nil {
		t.Fatalf("operator key: %v", err)
	}
	if code, _ := doJSON(t, "PATCH", srv.URL+"/v1/images/base", opKey,
		map[string]any{"published": true}); code != 200 {
		t.Fatalf("operator key PATCH /v1/images/base = %d, want 200", code)
	}
}
