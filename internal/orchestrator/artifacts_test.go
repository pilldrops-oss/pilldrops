package orchestrator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/artifact/header"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// These tests interrupt a publish between the object write and the ready
// mark and check that no reader considers the build usable. Each first
// confirms the rootfs really landed, so a publish that wrote nothing cannot pass.

// publishFixture is a control plane with a real store, a real artifact
// directory and an operator key.
type publishFixture struct {
	s      *Server
	srv    *httptest.Server
	st     *store.Store
	blobs  artifact.StorageProvider
	dir    string
	dbKind string
}

// newPublishFixture builds one. SQLite by default; Postgres when SANDBOXD_TEST_POSTGRES_URL is set.
func newPublishFixture(t *testing.T) *publishFixture {
	t.Helper()
	kind := "sqlite"
	if os.Getenv("SANDBOXD_TEST_POSTGRES_URL") != "" {
		kind = "postgres"
	}
	// Isolated per test: its own SQLite file or Postgres schema.
	st := store.OpenForTest(t)

	dir := filepath.Join(t.TempDir(), "artifacts")
	blobs, err := artifact.Open(artifact.KindFS, artifact.Options{Root: dir})
	if err != nil {
		t.Fatalf("open artifact store: %v", err)
	}

	s := NewServer("apikey", "nodetoken")
	s.SetStore(st)
	s.SetArtifactStore(blobs)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)

	return &publishFixture{s: s, srv: srv, st: st, blobs: blobs, dir: dir, dbKind: kind}
}

// publish POSTs body as the rootfs of buildID. Returns the status and body.
func (f *publishFixture) publish(t *testing.T, key, templateID, buildID string, body []byte) (int, string) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/templates/%s/artifacts?build_id=%s&kernel_version=6.1.0&envd_version=v9",
		f.srv.URL, templateID, buildID)
	req, err := http.NewRequestWithContext(t.Context(), "POST", url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// describe reads GET /v1/templates/{id}/artifacts.
func (f *publishFixture) describe(t *testing.T, key, templateID string) (int, describeBody) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), "GET",
		f.srv.URL+"/v1/templates/"+templateID+"/artifacts", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	defer resp.Body.Close()
	var out describeBody
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 200 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("describe body %q: %v", raw, err)
		}
	}
	return resp.StatusCode, out
}

type describeBody struct {
	TemplateID   string                 `json:"template_id"`
	ReadyBuildID string                 `json:"ready_build_id"`
	Builds       []store.TemplateBuild  `json:"builds"`
	Artifacts    []store.ArtifactObject `json:"artifacts"`
}

// assertNotUsable checks the status column, the describe API and the ready
// query all say the build is not usable.
func (f *publishFixture) assertNotUsable(t *testing.T, templateID, buildID string) {
	t.Helper()

	b, err := f.st.TemplateBuildByID(t.Context(), buildID)
	if err != nil {
		t.Fatalf("read build back: %v", err)
	}
	if b != nil && b.Ready() {
		t.Errorf("build %q is status=%q ready_at=%d after an INTERRUPTED publish — the database says "+
			"ready for an artifact that was never finished, which is the one state §4.5's write order "+
			"exists to make unrepresentable", buildID, b.Status, b.ReadyAt)
	}
	if b != nil && b.ReadyAt != 0 {
		t.Errorf("build %q has ready_at=%d after an interrupted publish", buildID, b.ReadyAt)
	}

	_, body := f.describe(t, "apikey", templateID)
	if body.ReadyBuildID != "" {
		t.Errorf("describe reports ready_build_id=%q for %q after an interrupted publish — an operator "+
			"(and the CLI) would be told the template is published", body.ReadyBuildID, templateID)
	}

	// The resolution query a node uses: only 'ready' rows.
	rows, err := f.st.TemplateBuilds(t.Context(), templateID, "", 0)
	if err != nil {
		t.Fatalf("list builds: %v", err)
	}
	for _, r := range rows {
		if r.Ready() {
			t.Errorf("a ready build (%q) exists for %q after an interrupted publish", r.BuildID, templateID)
		}
	}
}

// assertObjectWritten proves the interrupt happened after the data object landed.
func (f *publishFixture) assertObjectWritten(t *testing.T, buildID string, wantSize int) {
	t.Helper()
	key, err := artifact.Key(buildID, artifact.NameRootfs)
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.blobs.Stat(t.Context(), key)
	if err != nil {
		t.Fatalf("the rootfs object is NOT in the store, so this test did not interrupt the publish "+
			"between the object write and the ready-mark — it interrupted it earlier, and proves "+
			"nothing about the ordering: %v", err)
	}
	if info.Size != int64(wantSize) {
		t.Errorf("stored rootfs is %d bytes, want %d", info.Size, wantSize)
	}
}

// faultyStore fails on the first Put/Get whose key ends with failOn.
type faultyStore struct {
	artifact.StorageProvider
	failPutOn string
	failGetOn string
	// onPut runs before the named Put is refused, so a test can cancel the in-flight request.
	onPut func()
}

var errInjected = errors.New("injected store failure")

func (f *faultyStore) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if f.failPutOn != "" && strings.HasSuffix(key, f.failPutOn) {
		if f.onPut != nil {
			f.onPut()
		}
		return errInjected
	}
	return f.StorageProvider.Put(ctx, key, r, size)
}

func (f *faultyStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if f.failGetOn != "" && strings.HasSuffix(key, f.failGetOn) {
		return nil, errInjected
	}
	return f.StorageProvider.Get(ctx, key)
}

func TestPublishInterruptedAfterTheObjectWriteLeavesNoReadyBuild(t *testing.T) {
	payload := bytes.Repeat([]byte("squashfs"), 512)

	for _, tc := range []struct {
		name  string
		fault func(artifact.StorageProvider) artifact.StorageProvider
	}{
		{
			"header write fails",
			func(p artifact.StorageProvider) artifact.StorageProvider {
				return &faultyStore{StorageProvider: p, failPutOn: artifact.HeaderSuffix}
			},
		},
		{
			"metadata write fails",
			func(p artifact.StorageProvider) artifact.StorageProvider {
				return &faultyStore{StorageProvider: p, failPutOn: artifact.NameMetadata}
			},
		},
		{
			"verification read fails",
			func(p artifact.StorageProvider) artifact.StorageProvider {
				return &faultyStore{StorageProvider: p, failGetOn: artifact.NameMetadata}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPublishFixture(t)
			f.s.SetArtifactStore(tc.fault(f.blobs))

			tmpl := "tmpl-" + strings.ReplaceAll(strings.ToLower(tc.name), " ", "-")
			build := "bld-" + strings.ReplaceAll(strings.ToLower(tc.name), " ", "_")

			code, body := f.publish(t, "apikey", tmpl, build, payload)
			if code < 400 {
				t.Fatalf("an interrupted publish returned %d %q, want an error status", code, body)
			}
			f.assertObjectWritten(t, build, len(payload))
			f.assertNotUsable(t, tmpl, build)
		})
	}
}

// Unlike the fault cases, a cancelled context also fails the tidy-up that marks the row failed.
func TestPublishKilledMidFlightLeavesNoReadyBuild(t *testing.T) {
	f := newPublishFixture(t)
	payload := bytes.Repeat([]byte("rootfs-bytes"), 400)

	ctx, cancel := context.WithCancel(t.Context())
	// The rootfs Put has completed by the time the header Put is attempted.
	f.s.SetArtifactStore(&faultyStore{
		StorageProvider: f.blobs,
		failPutOn:       artifact.HeaderSuffix,
		onPut:           cancel,
	})

	url := f.srv.URL + "/v1/templates/killed/artifacts?build_id=bld-killed"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer apikey")
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Log("the killed request returned a response rather than a transport error; " +
			"the assertions below are what matter either way")
	}

	f.assertObjectWritten(t, "bld-killed", len(payload))
	f.assertNotUsable(t, "killed", "bld-killed")

	objs, err := f.st.ArtifactObjects(t.Context(), "bld-killed")
	if err != nil {
		t.Fatalf("list artifact rows: %v", err)
	}
	if len(objs) != 0 {
		t.Errorf("a killed publish recorded %d artifact row(s): %+v — those rows assert bytes that "+
			"were verified, and nothing verified these", len(objs), objs)
	}
}

func TestPublishIsRejectedWithoutOperatorCredentials(t *testing.T) {
	f := newPublishFixture(t)
	_, userKey, err := f.st.CreateKeyWithRole(t.Context(), "customer", "acme", "user")
	if err != nil {
		t.Fatalf("create customer key: %v", err)
	}

	for _, tc := range []struct {
		name, key string
		want      int
	}{
		{"no credential at all", "", http.StatusUnauthorized},
		{"a wrong bearer", "not-the-key", http.StatusUnauthorized},
		{"a valid CUSTOMER key", userKey, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := f.publish(t, tc.key, "base", "bld-unauth", []byte("rootfs"))
			if code != tc.want {
				t.Errorf("publish with %s = %d %q, want %d", tc.name, code, body, tc.want)
			}
			// The refusal must happen before any byte is written.
			key, _ := artifact.Key("bld-unauth", artifact.NameRootfs)
			if _, err := f.blobs.Stat(t.Context(), key); !errors.Is(err, artifact.ErrNotFound) {
				t.Errorf("a refused publish still wrote %s (stat err = %v)", key, err)
			}
			if b, _ := f.st.TemplateBuildByID(t.Context(), "bld-unauth"); b != nil {
				t.Errorf("a refused publish still created build row %+v", b)
			}
		})
	}
}

func TestDescribeIsScopedToTheCallersOrg(t *testing.T) {
	f := newPublishFixture(t)
	if code, body := f.publishOrg(t, "acme", "shared", "bld-acme", []byte("acme-rootfs")); code != 201 {
		t.Fatalf("publish for acme = %d %q", code, body)
	}
	_, otherKey, err := f.st.CreateKeyWithRole(t.Context(), "other", "globex", "user")
	if err != nil {
		t.Fatal(err)
	}

	if _, body := f.describe(t, "apikey", "shared"); body.ReadyBuildID != "bld-acme" {
		t.Errorf("operator describe ready_build_id = %q, want bld-acme", body.ReadyBuildID)
	}
	code, body := f.describe(t, otherKey, "shared")
	if code != 200 {
		t.Fatalf("customer describe = %d", code)
	}
	if body.ReadyBuildID != "" || len(body.Builds) != 0 {
		t.Errorf("a key for org globex saw org acme's builds (%q, %d rows) — describe is not org-scoped",
			body.ReadyBuildID, len(body.Builds))
	}
}

func (f *publishFixture) publishOrg(t *testing.T, org, templateID, buildID string, body []byte) (int, string) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/templates/%s/artifacts?build_id=%s&org=%s", f.srv.URL, templateID, buildID, org)
	req, err := http.NewRequestWithContext(t.Context(), "POST", url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer apikey")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// The control for the interrupt tests: a complete publish must actually succeed.
func TestPublishWritesEveryObjectAndThenMarksReady(t *testing.T) {
	f := newPublishFixture(t)
	payload := bytes.Repeat([]byte("real-squashfs-bytes"), 1024)
	want := sha256.Sum256(payload)

	code, body := f.publish(t, "apikey", "base", "bld-happy", payload)
	if code != http.StatusCreated {
		t.Fatalf("publish = %d %q, want 201", code, body)
	}

	b, err := f.st.TemplateBuildByID(t.Context(), "bld-happy")
	if err != nil || b == nil {
		t.Fatalf("build row: %v", err)
	}
	if !b.Ready() {
		t.Fatalf("a completed publish left status=%q", b.Status)
	}
	if b.RootfsSize != int64(len(payload)) || b.RootfsSHA256 != hex.EncodeToString(want[:]) {
		t.Errorf("build records size=%d sha=%s, want %d / %s",
			b.RootfsSize, b.RootfsSHA256, len(payload), hex.EncodeToString(want[:]))
	}
	// The row's version triple must equal the copy in metadata.json.
	if b.KernelVersion != "6.1.0" || b.EnvdVersion != "v9" {
		t.Errorf("build records kernel=%q envd=%q, want 6.1.0 / v9", b.KernelVersion, b.EnvdVersion)
	}

	rawMeta := f.mustGet(t, "bld-happy/"+artifact.NameMetadata)
	meta, err := artifact.ParseMetadata(rawMeta)
	if err != nil {
		t.Fatalf("metadata.json: %v", err)
	}
	if meta.KernelVersion != b.KernelVersion || meta.EnvdVersion != b.EnvdVersion {
		t.Errorf("metadata.json (%q/%q) and the database copy (%q/%q) disagree — the database is "+
			"caching a version triple the artifact does not carry",
			meta.KernelVersion, meta.EnvdVersion, b.KernelVersion, b.EnvdVersion)
	}

	hdr, err := header.Unmarshal(f.mustGet(t, "bld-happy/"+artifact.NameRootfs+artifact.HeaderSuffix))
	if err != nil {
		t.Fatalf("header: %v", err)
	}
	got, ok := hdr.SelfChecksum()
	if !ok || got != want {
		t.Errorf("the published header's checksum is %x, want %x — a node's cache verifies a fetch "+
			"against this and would discard the artifact", got[:8], want[:8])
	}
	if hdr.TotalSize != uint64(len(payload)) {
		t.Errorf("header TotalSize = %d, want %d", hdr.TotalSize, len(payload))
	}

	_, desc := f.describe(t, "apikey", "base")
	if desc.ReadyBuildID != "bld-happy" {
		t.Errorf("describe ready_build_id = %q, want bld-happy", desc.ReadyBuildID)
	}
	if len(desc.Artifacts) != 3 {
		t.Errorf("describe returned %d artifact rows, want 3 (rootfs, its header, metadata.json): %+v",
			len(desc.Artifacts), desc.Artifacts)
	}
}

func (f *publishFixture) mustGet(t *testing.T, key string) []byte {
	t.Helper()
	rc, err := f.blobs.Get(t.Context(), key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return raw
}

func TestPublishRefusesWithoutAnArtifactStore(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "cp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := NewServer("apikey", "nodetoken")
	s.SetStore(st)
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	req, _ := http.NewRequestWithContext(t.Context(), "POST",
		srv.URL+"/v1/templates/base/artifacts", bytes.NewReader([]byte("x")))
	req.Header.Set("Authorization", "Bearer apikey")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("publish with no artifact store = %d, want 503", resp.StatusCode)
	}
	if !strings.Contains(string(raw), "ORCHESTRATOR_ARTIFACT_DIR") {
		t.Errorf("the refusal does not name the setting an operator has to set: %s", raw)
	}
	if b, _ := st.TemplateBuildByID(t.Context(), "bld-nostore"); b != nil {
		t.Errorf("a refused publish still created a build row")
	}
}

func TestPublishOverAnExistingBuildIDIsRefused(t *testing.T) {
	f := newPublishFixture(t)
	if code, body := f.publish(t, "apikey", "base", "bld-dup", []byte("first")); code != 201 {
		t.Fatalf("first publish = %d %q", code, body)
	}
	code, body := f.publish(t, "apikey", "base", "bld-dup", []byte("second"))
	if code != http.StatusConflict {
		t.Errorf("re-publishing build bld-dup = %d %q, want 409", code, body)
	}
	if !strings.Contains(body, "already exists") {
		t.Errorf("the refusal does not say why: %s", body)
	}
}

func TestPublishRouteIsNotProxiedToANode(t *testing.T) {
	f := newPublishFixture(t)
	// With no nodes registered the "/v1/" catch-all would answer 503.
	code, body := f.publish(t, "apikey", "base", "bld-routed", []byte("bytes"))
	if code != http.StatusCreated {
		t.Fatalf("publish = %d %q, want 201 — if this is a 'no healthy node' error the route fell "+
			"through to the node proxy", code, body)
	}
	if b, _ := f.st.TemplateBuildByID(t.Context(), "bld-routed"); b == nil {
		t.Error("the publish was answered but no build row exists, so it was not this tier that served it")
	}
}

func TestPublishIsExemptFromTheGenericBodyCap(t *testing.T) {
	if !bodyCapExempt("/v1/templates/base/artifacts") {
		t.Error("the publish route is subject to the generic 10 MiB body cap, so every real template " +
			"would be rejected as too large")
	}
	// The exemption must be no wider than the route.
	for _, p := range []string{"/v1/templates", "/v1/templates/base", "/v1/templates/base/other", "/v1/sandboxes"} {
		if bodyCapExempt(p) {
			t.Errorf("%s is exempt from the body cap but should not be", p)
		}
	}
}
