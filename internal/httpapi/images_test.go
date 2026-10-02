package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/templates"
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

// Snapshot captures land in the template directory under ids that name their
// org and sandbox; a tenant's catalogue must not show another org's.
func TestTenantCataloguesHideSnapshotTemplates(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"snap-37a8eec1-move-sb-1790000000000-0123abcd", "mytool"} {
		if err := os.MkdirAll(filepath.Join(dir, id), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id, "rootfs.squashfs"), []byte(id), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	templates.SetDir(dir)
	t.Cleanup(func() { templates.SetDir("") })
	srv, _, key := newTestServerWithStore(t)

	ids := func(path string, list func([]byte) []map[string]any) []string {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var out []string
		for _, e := range list(b) {
			id, _ := e["id"].(string)
			out = append(out, id)
		}
		return out
	}
	tmpls := ids("/v1/templates", func(b []byte) []map[string]any {
		var v []map[string]any
		_ = json.Unmarshal(b, &v)
		return v
	})
	imgs := ids("/v1/images", func(b []byte) []map[string]any {
		var v struct{ Images []map[string]any }
		_ = json.Unmarshal(b, &v)
		return v.Images
	})
	for name, got := range map[string][]string{"/v1/templates": tmpls, "/v1/images": imgs} {
		if !slices.Contains(got, "mytool") {
			t.Errorf("%s = %v, want the disk template mytool", name, got)
		}
		for _, id := range got {
			if strings.HasPrefix(id, "snap-") {
				t.Errorf("%s lists snapshot template %q", name, id)
			}
		}
	}
}
