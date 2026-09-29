package orchestrator

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

func node(id, runtime string) *cluster.Node {
	return &cluster.Node{ID: id, Runtime: runtime}
}

func TestPickBuildNodeHonoursTheRequestedRuntime(t *testing.T) {
	fleet := []*cluster.Node{node("a", "docker"), node("b", "firecracker"), node("c", "docker")}

	got, ok := pickBuildNode(fleet, "firecracker")
	if !ok || got.ID != "b" {
		t.Fatalf("runtime=firecracker picked %v (ok=%v), want b", got, ok)
	}
	got, ok = pickBuildNode(fleet, "docker")
	if !ok || got.ID != "a" {
		t.Fatalf("runtime=docker picked %v (ok=%v), want a", got, ok)
	}
	// No preference: docker first, since a custom image can only launch there.
	got, ok = pickBuildNode(fleet, "")
	if !ok || got.ID != "a" {
		t.Fatalf("no preference picked %v (ok=%v), want a", got, ok)
	}
}

func TestPickBuildNodeNeverFallsBackToAnotherRuntime(t *testing.T) {
	onlyDocker := []*cluster.Node{node("a", "docker"), node("b", "docker")}
	if got, ok := pickBuildNode(onlyDocker, "firecracker"); ok {
		t.Fatalf("runtime=firecracker fell back to %s, a docker node", got.ID)
	}
	msg := noBuildNodeMessage(onlyDocker, "firecracker")
	for _, want := range []string{"firecracker", "docker×2", "not interchangeable"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the 503 must name %q so an operator can see what the fleet has; got %q",
				want, msg)
		}
	}
}

// fakeTemplateNode is a Firecracker sandboxd answering a rootfs build and export.
type fakeTemplateNode struct {
	id  string
	srv *httptest.Server

	mu sync.Mutex
	// gotBuildPayload and gotExportPayload are what the control plane asked for on each route.
	gotBuildPayload  string
	gotExportPayload string

	rootfs []byte
	// reportPayload is what the node claims it built; empty mimics an older node.
	reportPayload string
}

func newFakeTemplateNode(t *testing.T, id string) *fakeTemplateNode {
	t.Helper()
	n := &fakeTemplateNode{
		id: id,
		// squashfs superblock magic
		rootfs:        append([]byte("hsqs"), bytes.Repeat([]byte("squashfs-rootfs"), 512)...),
		reportPayload: artifact.NameRootfs,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/images/build", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		n.mu.Lock()
		n.gotBuildPayload = r.URL.Query().Get("payload")
		report := n.reportPayload
		n.mu.Unlock()
		name := r.URL.Query().Get("name")
		out := map[string]any{
			"ok": true, "tag": "sf-acme-" + name + ":latest",
			"log": "built on " + id, "template_id": "sf-acme-" + name,
			"envd_sha256": "e3b0c44298fc1c14",
		}
		if report != "" {
			out["payload"] = report
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /v1/images/export", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		n.gotExportPayload = r.URL.Query().Get("payload")
		body := n.rootfs
		n.mu.Unlock()
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
	})
	n.srv = httptest.NewServer(mux)
	t.Cleanup(n.srv.Close)
	return n
}

// templateFleet is a control plane with one Firecracker node.
type templateFleet struct {
	*publishFixture
	node *fakeTemplateNode
}

func newTemplateFleet(t *testing.T) *templateFleet {
	t.Helper()
	f := newPublishFixture(t)
	n := newFakeTemplateNode(t, "node-fc")
	upsert(f.s.reg, n.id, n.srv.URL, "nodekey-"+n.id, "", nil, "v1",
		cluster.Capacity{Cores: 4, MemTotalMB: 8192, MemAvailMB: 8192}, time.Now())
	f.s.reg.SetNodeMeta(n.id, cluster.NodeMeta{Runtime: "firecracker", Isolation: true, CanFetch: true})
	return &templateFleet{publishFixture: f, node: n}
}

func (f *templateFleet) build(t *testing.T, query string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), "POST",
		f.srv.URL+"/v1/images/build?"+query, bytes.NewReader([]byte("build-context")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer apikey")
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return resp.StatusCode, out
}

// The bug guarded against is not "nothing published" but "published under the
// other runtime's name", which every status code in the response is happy with.
func TestAFirecrackerBuildPublishesATemplateArtifact(t *testing.T) {
	f := newTemplateFleet(t)
	status, out := f.build(t, "name=tool&runtime=firecracker")
	if status != 200 || out["ok"] != true {
		t.Fatalf("build: status=%d out=%v", status, out)
	}

	// Both legs must ask for a rootfs, or a docker save streams out under a template's provenance.
	f.node.mu.Lock()
	gotBuild, gotExport := f.node.gotBuildPayload, f.node.gotExportPayload
	f.node.mu.Unlock()
	if gotBuild != artifact.NameRootfs {
		t.Errorf("the node was asked to build payload=%q, want %q", gotBuild, artifact.NameRootfs)
	}
	if gotExport != artifact.NameRootfs {
		t.Errorf("the node was asked to export payload=%q, want %q", gotExport, artifact.NameRootfs)
	}

	pub, _ := out["publish"].(map[string]any)
	if pub == nil || pub["ok"] != true {
		t.Fatalf("publish did not succeed: %v", out["publish"])
	}
	if pub["payload"] != artifact.NameRootfs {
		t.Errorf("publish reported payload %v, want %q", pub["payload"], artifact.NameRootfs)
	}
	buildID, _ := pub["build_id"].(string)
	if buildID == "" {
		t.Fatal("publish reported no build id")
	}

	rootfs := filepath.Join(f.dir, buildID, artifact.NameRootfs)
	body, err := os.ReadFile(rootfs)
	if err != nil {
		t.Fatalf("the published data object is not at %s: %v", rootfs, err)
	}
	if !bytes.HasPrefix(body, []byte("hsqs")) {
		t.Errorf("%s does not start with the squashfs magic; it is not a template rootfs", rootfs)
	}
	if _, err := os.Stat(filepath.Join(f.dir, buildID, artifact.NameImage)); err == nil {
		t.Errorf("a %s object was ALSO written; the build published both kinds", artifact.NameImage)
	}

	raw, err := os.ReadFile(filepath.Join(f.dir, buildID, artifact.NameMetadata))
	if err != nil {
		t.Fatalf("metadata.json: %v", err)
	}
	meta, err := artifact.ParseMetadata(raw)
	if err != nil {
		t.Fatal(err)
	}
	if meta.PayloadName() != artifact.NameRootfs {
		t.Errorf("metadata.json says payload=%q, want %q", meta.PayloadName(), artifact.NameRootfs)
	}
	if meta.TemplateID != "sf-acme-tool" {
		t.Errorf("metadata.json template_id=%q, want sf-acme-tool", meta.TemplateID)
	}
	if meta.EnvdVersion != "e3b0c44298fc1c14" {
		t.Errorf("metadata.json envd_version=%q, want the node's reported hash", meta.EnvdVersion)
	}
}

// An older node reports no payload and can only have built an image tarball.
func TestANodeThatReportsNoPayloadIsTreatedAsAnImageBuild(t *testing.T) {
	f := newTemplateFleet(t)
	f.node.reportPayload = ""

	status, out := f.build(t, "name=tool")
	if status != 200 || out["ok"] != true {
		t.Fatalf("build: status=%d out=%v", status, out)
	}
	pub, _ := out["publish"].(map[string]any)
	if pub["payload"] != artifact.NameImage {
		t.Fatalf("a silent node's build was published as %v, want %q",
			pub["payload"], artifact.NameImage)
	}
	buildID, _ := pub["build_id"].(string)
	if _, err := os.Stat(filepath.Join(f.dir, buildID, artifact.NameImage)); err != nil {
		t.Errorf("no %s object was written: %v", artifact.NameImage, err)
	}
}

func TestAnUnrecognisedPayloadIsRefusedRatherThanDefaulted(t *testing.T) {
	f := newTemplateFleet(t)
	f.node.reportPayload = "rootfs.ext4"

	status, out := f.build(t, "name=tool&runtime=firecracker")
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	pub, _ := out["publish"].(map[string]any)
	if pub["ok"] == true {
		t.Fatal("an unrecognised payload was published anyway")
	}
	msg, _ := pub["error"].(string)
	if !strings.Contains(msg, "rootfs.ext4") {
		t.Errorf("the error must quote what the node said; got %q", msg)
	}
}

func TestAFirecrackerBuildOnADockerOnlyFleetIs503(t *testing.T) {
	f := newBuildFleet(t, 2) // both docker
	req, err := http.NewRequestWithContext(t.Context(), "POST",
		f.srv.URL+"/v1/images/build?name=tool&runtime=firecracker", bytes.NewReader([]byte("ctx")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer apikey")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 503 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "docker×2") {
		t.Errorf("the 503 must say what the fleet actually has; got %s", raw)
	}
	built, _ := f.touched()
	if len(built) != 0 {
		t.Errorf("nothing should have been built: %v", built)
	}
}
