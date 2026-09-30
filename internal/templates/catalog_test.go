package templates

import (
	"os"
	"path/filepath"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

func TestCatalogShape(t *testing.T) {
	c := Catalog()
	if len(c) != len(catalog) {
		t.Fatalf("catalog size = %d, want %d (built-ins; no registry dir set)", len(c), len(catalog))
	}
	if c[0].ID != "base" {
		t.Fatalf("first template = %q, want base", c[0].ID)
	}
	c[0].Name = "MUTATED"
	if Catalog()[0].Name == "MUTATED" {
		t.Fatalf("Catalog() leaked its backing slice")
	}
}

// TestOneAppPort pins the single app-port convention: sandboxd routes the
// preview to 8000 (fcAppPort, internal/runtime/firecracker/adapter.go), so a
// catalog row on 8080 publishes a port nothing proxies.
func TestOneAppPort(t *testing.T) {
	for _, e := range Catalog() {
		if e.Port == 8080 || e.DefaultPort == 8080 {
			t.Errorf("%s: port=%d default_port=%d, want 8000 (the only app port)", e.ID, e.Port, e.DefaultPort)
		}
	}
}

func TestDefaultResolves(t *testing.T) {
	if defaultID != "base" {
		t.Fatalf("default = %q, want base", defaultID)
	}
	if _, ok := Resolve(defaultID); !ok {
		t.Fatalf("default %q not resolvable", defaultID)
	}
}

func TestResolveCanonicalAndAliases(t *testing.T) {
	cases := map[string]string{
		"base":                "base",
		"code-interpreter":    "code-interpreter",
		"python-3.11":         "code-interpreter",
		"python-data-science": "code-interpreter",
		"node":                "node",
		"node-20":             "node",
		"go":                  "go",
	}
	for in, want := range cases {
		got, ok := Resolve(in)
		if !ok {
			t.Errorf("Resolve(%q) = not found, want %q", in, want)
			continue
		}
		if got.ID != want {
			t.Errorf("Resolve(%q).ID = %q, want %q", in, got.ID, want)
		}
	}
	if _, ok := Resolve("cobol-74"); ok {
		t.Fatalf("Resolve(cobol-74) = found, want not found")
	}
}

// Every id the portal picker advertises must Resolve on the node or create 400s.
func TestPortalListedTemplatesAreCreatable(t *testing.T) {
	portalIDs := []string{
		"base", "python-3.11", "node-20", "code-interpreter-v1",
		"desktop", "go-1.22", "rust-1.77",
	}
	for _, id := range portalIDs {
		if _, ok := Resolve(id); !ok {
			t.Errorf("portal lists template %q but the node can't Resolve it → create would 400", id)
		}
	}
}

func TestLaunchMetadataPopulated(t *testing.T) {
	byID := map[string]Template{}
	for _, tpl := range Catalog() {
		byID[tpl.ID] = tpl
	}
	cases := []struct {
		id              string
		wantPreinstall  bool
		wantStart       string
		wantHasServer   bool
		wantServerDesc  bool
		wantDefaultPort int
	}{
		{"code-interpreter", true, "python3 -m http.server $PORT", false, false, 8000},
		{"go", true, "go run .", false, false, 8000},
		{"rust", true, "cargo run", false, false, 8000},
		// The desktop template previews on the platform desktop port: its viewer
		// is envd's noVNC, not an app on 8000.
		{"desktop", true, "", true, true, rt.DesktopPort},
		{"base", true, "", false, false, 8000},
	}
	for _, tc := range cases {
		tpl, ok := byID[tc.id]
		if !ok {
			t.Errorf("template %q missing from catalog", tc.id)
			continue
		}
		if tc.wantPreinstall && len(tpl.Preinstalled) == 0 {
			t.Errorf("%s: Preinstalled empty, want non-empty", tc.id)
		}
		if tpl.StartSuggestion != tc.wantStart {
			t.Errorf("%s: StartSuggestion = %q, want %q", tc.id, tpl.StartSuggestion, tc.wantStart)
		}
		if tpl.HasServer != tc.wantHasServer {
			t.Errorf("%s: HasServer = %v, want %v", tc.id, tpl.HasServer, tc.wantHasServer)
		}
		if (tpl.ServerDesc != "") != tc.wantServerDesc {
			t.Errorf("%s: ServerDesc = %q, want non-empty=%v", tc.id, tpl.ServerDesc, tc.wantServerDesc)
		}
		if tpl.DefaultPort != tc.wantDefaultPort {
			t.Errorf("%s: DefaultPort = %d, want %d", tc.id, tpl.DefaultPort, tc.wantDefaultPort)
		}
		if tpl.HasServer && tpl.StartSuggestion != "" {
			t.Errorf("%s: HasServer template must have empty StartSuggestion, got %q", tc.id, tpl.StartSuggestion)
		}
	}

	for _, tpl := range Catalog() {
		if tpl.DefaultPort <= 0 {
			t.Errorf("%s: DefaultPort = %d, want > 0", tpl.ID, tpl.DefaultPort)
		}
		if tpl.Port != 0 && tpl.DefaultPort != tpl.Port {
			t.Errorf("%s: DefaultPort = %d, want Port %d", tpl.ID, tpl.DefaultPort, tpl.Port)
		}
	}
}

func TestEveryTemplateHasDockerfile(t *testing.T) {
	for _, tpl := range Catalog() {
		// Image-backed templates boot a published image and have no Dockerfile.
		if tpl.Image != "" {
			continue
		}
		p := filepath.Join("..", "..", "templates", tpl.ID, "Dockerfile")
		if _, err := os.Stat(p); err != nil {
			t.Errorf("template %q missing Dockerfile at %s: %v", tpl.ID, p, err)
		}
	}
}

func TestImageBackedTemplatesDeclarePorts(t *testing.T) {
	for _, tpl := range Catalog() {
		if tpl.Image == "" {
			continue
		}
		if len(tpl.Ports) == 0 {
			t.Errorf("%s declares Image %q but no Ports — nothing could connect to it", tpl.ID, tpl.Image)
		}
		if tpl.DefaultPort == 0 {
			t.Errorf("%s declares Image %q but no DefaultPort — the preview would prefill 8000", tpl.ID, tpl.Image)
		}
	}
}

// The engine ships base images to build on; curated templates live elsewhere.
func TestCatalogIsTheBaseImages(t *testing.T) {
	want := map[string]bool{"base": true, "docker": true, "code-interpreter": true, "browser": true,
		"desktop": true, "playwright": true, "node": true, "go": true, "rust": true}
	got := map[string]bool{}
	for _, tp := range Catalog() {
		got[tp.ID] = true
		if !want[tp.ID] {
			t.Errorf("catalog has %q, which is not a base image", tp.ID)
		}
	}
	for id := range want {
		if !got[id] {
			t.Errorf("catalog is missing base image %q", id)
		}
	}
}
