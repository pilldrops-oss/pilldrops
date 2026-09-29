package templates

import (
	"os"
	"path/filepath"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// publish fakes a built template on disk: a rootfs marker plus an optional manifest.
func publish(t *testing.T, dir, id, manifest string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, id), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id, "rootfs.squashfs"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(dir, id, "template.json"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRegistryDiscoversPublishedImages(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { SetDir("") })

	publish(t, dir, "claude-terminal", `{
		"name": "Claude Terminal", "description": "browser terminal",
		"icon": "ct", "cpu": 2, "memory_mb": 2048, "port": 8080
	}`)
	publish(t, dir, "mystery", "")
	SetDir(dir)

	got, ok := Resolve("claude-terminal")
	if !ok {
		t.Fatal("published template not resolvable")
	}
	if got.Name != "Claude Terminal" || got.Port != 8080 || got.CPU != 2 {
		t.Fatalf("manifest not applied: %+v", got)
	}

	got, ok = Resolve("mystery")
	if !ok || got.CPU != 1 || got.MemoryMB != 512 || got.Visibility != "public" {
		t.Fatalf("defaults not applied: ok=%v %+v", ok, got)
	}

	seen := map[string]int{}
	for _, tpl := range Catalog() {
		seen[tpl.ID]++
	}
	if seen["claude-terminal"] != 1 || seen["mystery"] != 1 || seen["base"] != 1 {
		t.Fatalf("catalog union wrong: %v", seen)
	}

	if _, ok := Resolve("desktop"); !ok {
		t.Fatal("built-in lost")
	}
}

func TestRegistryManifestCarriesLaunchMetadata(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { SetDir("") })

	publish(t, dir, "custom-api", `{
		"name": "Custom API", "port": 9000,
		"preinstalled": ["python3", "poetry"],
		"start_suggestion": "poetry run app --port $PORT",
		"setup_suggestion": ["poetry install"],
		"docs_url": "https://example.com/docs",
		"env": [{"name": "API_KEY", "required": true, "secret": true}]
	}`)
	publish(t, dir, "custom-server", `{
		"name": "Custom Server", "port": 7000,
		"has_server": true, "server_desc": "streamed on the preview URL"
	}`)
	publish(t, dir, "plain", "")
	SetDir(dir)

	got, ok := Resolve("custom-api")
	if !ok {
		t.Fatal("custom-api not resolvable")
	}
	if len(got.Preinstalled) != 2 || got.Preinstalled[0] != "python3" {
		t.Errorf("Preinstalled not applied: %v", got.Preinstalled)
	}
	if got.StartSuggestion != "poetry run app --port $PORT" {
		t.Errorf("StartSuggestion not applied: %q", got.StartSuggestion)
	}
	if len(got.SetupSuggestion) != 1 || got.SetupSuggestion[0] != "poetry install" {
		t.Errorf("SetupSuggestion not applied: %v", got.SetupSuggestion)
	}
	if got.DocsURL != "https://example.com/docs" {
		t.Errorf("DocsURL not applied: %q", got.DocsURL)
	}
	if len(got.Env) != 1 || got.Env[0].Name != "API_KEY" || !got.Env[0].Required || !got.Env[0].Secret {
		t.Errorf("Env not applied: %+v", got.Env)
	}
	if got.DefaultPort != 9000 {
		t.Errorf("DefaultPort = %d, want 9000 (from Port)", got.DefaultPort)
	}

	srv, ok := Resolve("custom-server")
	if !ok {
		t.Fatal("custom-server not resolvable")
	}
	if !srv.HasServer || srv.ServerDesc != "streamed on the preview URL" {
		t.Errorf("server metadata not applied: has_server=%v desc=%q", srv.HasServer, srv.ServerDesc)
	}
	if srv.DefaultPort != 7000 {
		t.Errorf("DefaultPort = %d, want 7000", srv.DefaultPort)
	}

	plain, ok := Resolve("plain")
	if !ok {
		t.Fatal("plain not resolvable")
	}
	if plain.DefaultPort != 8000 {
		t.Errorf("plain DefaultPort = %d, want 8000 floor", plain.DefaultPort)
	}
}

func TestRegistryManifestOverridesBuiltin(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { SetDir("") })
	publish(t, dir, "desktop", `{"memory_mb": 4096}`)
	SetDir(dir)

	got, ok := Resolve("desktop")
	if !ok {
		t.Fatal("desktop not resolvable")
	}
	if got.MemoryMB != 4096 || got.Port != rt.DesktopPort || got.Name != "Desktop" {
		t.Fatalf("override merge wrong: %+v", got)
	}
	n := 0
	for _, tpl := range Catalog() {
		if tpl.ID == "desktop" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("desktop listed %d times", n)
	}
}
