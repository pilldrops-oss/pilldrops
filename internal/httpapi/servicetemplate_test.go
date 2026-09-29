package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// Service templates declare image and ports in the catalogue; these tests
// assert on what the runtime received, not on the helpers.

func serviceTemplateServer(t *testing.T) (*httptest.Server, *runtimetest.Fake, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	_, key, err := st.CreateKey(t.Context(), "test", "default")
	if err != nil {
		t.Fatal(err)
	}
	fake := runtimetest.NewFake()
	d := daemon.New(daemon.Config{
		Runtime:     fake,
		Store:       daemon.StoreAdapter{S: st},
		SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	s := New(d, st)
	attachTestFunctions(s, d, st, dir)
	s.SetOrgOverrideSecret([]byte(testOrgOverrideSecret))
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	return srv, fake, key
}

func createWith(t *testing.T, srv *httptest.Server, key, body string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sandboxes", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestCreatingAServiceTemplateBootsItsImage(t *testing.T) {
	srv, fake, key := serviceTemplateServer(t)
	want, ok := templates.Resolve("postgres")
	if !ok || want.Image == "" {
		t.Fatal("postgres is not a catalogue service template")
	}

	if code := createWith(t, srv, key, `{"template":"postgres"}`); code >= 400 {
		t.Fatalf("create postgres = %d", code)
	}
	spec, ok := fake.LastBootSpec()
	if !ok {
		t.Fatal("nothing was booted")
	}
	if spec.Template.Image != want.Image {
		t.Errorf("booted image = %q, want the catalogue's %q — the sandbox would run the "+
			"base rootfs with `sleep infinity` instead of a database", spec.Template.Image, want.Image)
	}
	if len(spec.Template.Ports) == 0 {
		t.Error("booted with no published ports — nothing could connect to the service")
	}
}

func TestRequestImageOverridesTheCatalogue(t *testing.T) {
	srv, fake, key := serviceTemplateServer(t)

	if code := createWith(t, srv, key, `{"template":"postgres","image":"postgres:15","ports":[9999]}`); code >= 400 {
		t.Fatalf("create = %d", code)
	}
	spec, _ := fake.LastBootSpec()
	if spec.Template.Image != "postgres:15" {
		t.Errorf("booted image = %q, want the requested postgres:15", spec.Template.Image)
	}
	// Ports replace rather than merge.
	if len(spec.Template.Ports) != 1 || spec.Template.Ports[0] != 9999 {
		t.Errorf("booted ports = %v, want exactly [9999]", spec.Template.Ports)
	}
}

func TestOrdinaryTemplateBootsNoImage(t *testing.T) {
	srv, fake, key := serviceTemplateServer(t)

	if code := createWith(t, srv, key, `{"template":"base"}`); code >= 400 {
		t.Fatalf("create base = %d", code)
	}
	spec, _ := fake.LastBootSpec()
	if spec.Template.Image != "" {
		t.Errorf("base booted with image %q — it should use the platform rootfs", spec.Template.Image)
	}
}

// A service template's catalogue resources must reach the boot spec, or mysql
// is clamped to the 128 MiB floor and OOM-killed at init.
func TestServiceTemplateCarriesItsResources(t *testing.T) {
	srv, fake, key := serviceTemplateServer(t)
	want, ok := templates.Resolve("mysql")
	if !ok || want.MemoryMB == 0 {
		t.Fatal("mysql declares no memory in the catalogue")
	}

	if code := createWith(t, srv, key, `{"template":"mysql"}`); code >= 400 {
		t.Fatalf("create mysql = %d", code)
	}
	spec, ok := fake.LastBootSpec()
	if !ok {
		t.Fatal("nothing was booted")
	}
	if spec.MemMiB < want.MemoryMB {
		t.Errorf("booted with %d MiB, want at least the catalogue's %d — a service clamped to "+
			"the 128 MiB floor is OOM-killed at init (exit 137), which reads as a broken image",
			spec.MemMiB, want.MemoryMB)
	}
	if spec.VCPU < want.CPU {
		t.Errorf("booted with %d vCPU, want at least the catalogue's %d", spec.VCPU, want.CPU)
	}
}
