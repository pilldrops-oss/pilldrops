package orchestrator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// fakeBuildNode is a stand-in sandboxd answering the two routes the build path uses.
type fakeBuildNode struct {
	id  string
	srv *httptest.Server

	mu         sync.Mutex
	builds     int
	exports    int
	gotContext []byte

	// image stands in for the docker save tarball; the publish path only hashes it.
	image   []byte
	buildOK bool
	// exportStatus, when non-zero, is returned instead of the tarball.
	exportStatus int
}

func newFakeBuildNode(t *testing.T, id string) *fakeBuildNode {
	t.Helper()
	n := &fakeBuildNode{
		id:      id,
		image:   []byte("docker-save-tarball-from-" + id + strings.Repeat("!", 512)),
		buildOK: true,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/images/build", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		n.mu.Lock()
		n.builds++
		n.gotContext = body
		ok := n.buildOK
		n.mu.Unlock()
		name := r.URL.Query().Get("name")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": ok, "tag": "sf-acme-" + name + ":latest", "log": "Successfully built on " + id,
		})
	})
	mux.HandleFunc("GET /v1/images/export", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		n.exports++
		status, img := n.exportStatus, n.image
		n.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte("no image tagged " + r.URL.Query().Get("tag") + " on this node"))
			return
		}
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = w.Write(img)
	})
	n.srv = httptest.NewServer(mux)
	t.Cleanup(n.srv.Close)
	return n
}

func (n *fakeBuildNode) counts() (builds, exports int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.builds, n.exports
}

// buildFleet is a control plane with n registered docker nodes.
type buildFleet struct {
	*publishFixture
	nodes []*fakeBuildNode
}

func newBuildFleet(t *testing.T, size int) *buildFleet {
	t.Helper()
	f := newPublishFixture(t)
	fleet := &buildFleet{publishFixture: f}
	now := time.Now()
	for i := 0; i < size; i++ {
		n := newFakeBuildNode(t, fmt.Sprintf("node-%c", 'a'+i))
		fleet.nodes = append(fleet.nodes, n)
		// Upsert's sixth argument is the node version, not the runtime; that is SetNodeMeta's.
		upsert(f.s.reg, n.id, n.srv.URL, "nodekey-"+n.id, "", nil, "v1",
			cluster.Capacity{Cores: 4, MemTotalMB: 8192, MemAvailMB: 8192}, now)
		f.s.reg.SetNodeMeta(n.id, cluster.NodeMeta{Runtime: "docker", Isolation: true, CanFetch: true})
	}
	return fleet
}

// build POSTs a build context and returns the status and decoded response.
func (f *buildFleet) build(t *testing.T, name string, context []byte) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), "POST",
		f.srv.URL+"/v1/images/build?name="+name, bytes.NewReader(context))
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
		t.Fatalf("decode build response %q: %v", raw, err)
	}
	return resp.StatusCode, out
}

// touched reports which nodes ran a build, and which were asked for an export.
func (f *buildFleet) touched() (built []string, exported []string) {
	for _, n := range f.nodes {
		b, e := n.counts()
		if b > 0 {
			built = append(built, fmt.Sprintf("%s(%d)", n.id, b))
		}
		if e > 0 {
			exported = append(exported, fmt.Sprintf("%s(%d)", n.id, e))
		}
	}
	return built, exported
}

// A fleet of three is needed: a fleet of one cannot tell "built on the only
// node" from "built on every node".
func TestACustomImageBuildTouchesExactlyOneNode(t *testing.T) {
	f := newBuildFleet(t, 3)

	code, out := f.build(t, "mytool", []byte("FROM alpine\nRUN true\n"))
	if code != 200 {
		t.Fatalf("build returned %d: %v", code, out)
	}

	built, exported := f.touched()
	if len(built) != 1 {
		t.Fatalf("the build ran on %d of %d nodes (%s) — §6.2's whole claim is that it runs on ONE, "+
			"and a fan-out is what made the fleet ceiling 20 minutes x node count",
			len(built), len(f.nodes), strings.Join(built, " "))
	}
	if len(exported) != 1 || strings.Split(exported[0], "(")[0] != strings.Split(built[0], "(")[0] {
		t.Fatalf("built on %s but exported from %v — the publish must read the image from the node "+
			"that actually built it", built, exported)
	}
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("build reported ok=false: %v", out)
	}
}

func TestOnlyTheChosenNodeReceivesTheBuildContext(t *testing.T) {
	f := newBuildFleet(t, 3)
	ctxBytes := []byte("FROM alpine\n" + strings.Repeat("# padding\n", 1000))

	if code, out := f.build(t, "tool", ctxBytes); code != 200 {
		t.Fatalf("build returned %d: %v", code, out)
	}

	var withContext []string
	for _, n := range f.nodes {
		n.mu.Lock()
		got := len(n.gotContext)
		n.mu.Unlock()
		if got > 0 {
			withContext = append(withContext, fmt.Sprintf("%s(%d bytes)", n.id, got))
		}
	}
	if len(withContext) != 1 {
		t.Fatalf("%d of %d nodes received the build context (%s); want exactly 1",
			len(withContext), len(f.nodes), strings.Join(withContext, " "))
	}
	// Spooling through a temp file must not truncate it.
	for _, n := range f.nodes {
		n.mu.Lock()
		got := n.gotContext
		n.mu.Unlock()
		if len(got) > 0 && !bytes.Equal(got, ctxBytes) {
			t.Fatalf("%s received %d bytes, want the full %d-byte context untruncated",
				n.id, len(got), len(ctxBytes))
		}
	}
}

func TestTheDeprecatedNodesArrayStillDecodesForAnOldClient(t *testing.T) {
	f := newBuildFleet(t, 3)

	req, err := http.NewRequestWithContext(t.Context(), "POST",
		f.srv.URL+"/v1/images/build?name=legacy", strings.NewReader("FROM alpine\n"))
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

	// The exact shape the old handler emitted.
	var old struct {
		OK    bool   `json:"ok"`
		Tag   string `json:"tag"`
		Log   string `json:"log"`
		Nodes []struct {
			Node string `json:"node"`
			OK   bool   `json:"ok"`
			Err  string `json:"error,omitempty"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &old); err != nil {
		t.Fatalf("the pre-§6 response shape no longer decodes: %v\nbody: %s", err, raw)
	}
	if !old.OK {
		t.Errorf("old client reads ok=false: %s", raw)
	}
	if old.Tag == "" {
		t.Errorf("old client reads an empty tag: %s", raw)
	}
	if len(old.Nodes) != 1 {
		t.Fatalf("deprecated nodes[] has %d elements, want exactly 1 — D6 specifies a "+
			"single-element array for the release the field survives into: %s", len(old.Nodes), raw)
	}
	if !old.Nodes[0].OK || old.Nodes[0].Node == "" {
		t.Errorf("deprecated nodes[0] = %+v, want the node that built it, ok", old.Nodes[0])
	}

	var fresh imageBuildResponse
	if err := json.Unmarshal(raw, &fresh); err != nil {
		t.Fatalf("decode new shape: %v", err)
	}
	if fresh.Build.Node != old.Nodes[0].Node {
		t.Errorf("build.node=%q but deprecated nodes[0].node=%q — the two views of one build "+
			"must not disagree", fresh.Build.Node, old.Nodes[0].Node)
	}
	if !fresh.Publish.OK || fresh.Publish.BuildID == "" {
		t.Errorf("publish=%+v, want a completed publish on a 3-node fleet", fresh.Publish)
	}
}

// Asserted against the store itself, not the response that claims it.
func TestABuiltImageIsPublishedToTheSharedStore(t *testing.T) {
	f := newBuildFleet(t, 3)

	code, out := f.build(t, "mytool", []byte("FROM alpine\n"))
	if code != 200 {
		t.Fatalf("build returned %d: %v", code, out)
	}
	pub, _ := out["publish"].(map[string]any)
	if ok, _ := pub["ok"].(bool); !ok {
		t.Fatalf("publish did not succeed: %v", pub)
	}
	buildID, _ := pub["build_id"].(string)
	templateID, _ := pub["template_id"].(string)
	if templateID != "sf-acme-mytool" {
		t.Errorf("template_id = %q, want sf-acme-mytool (the tag without its version)", templateID)
	}

	got, err := f.st.ReadyBuildForTemplate(t.Context(), templateID)
	if err != nil {
		t.Fatalf("a node resolving %q found no ready build: %v", templateID, err)
	}
	if got != buildID {
		t.Errorf("ReadyBuildForTemplate = %q, want %q", got, buildID)
	}

	// Under the docker payload name; under rootfs a Firecracker node would
	// hard-link a tarball where a squashfs belongs.
	key, err := artifact.Key(buildID, artifact.NameImage)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := f.blobs.Get(t.Context(), key)
	if err != nil {
		t.Fatalf("the published image is not readable from the shared store at %s: %v", key, err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if !bytes.Equal(body, f.nodes[0].image) {
		t.Errorf("the store holds %d bytes; the node exported %d", len(body), len(f.nodes[0].image))
	}
	// A node's cache refuses an artifact without its block map.
	hkey, _ := artifact.HeaderKey(buildID, artifact.NameImage)
	if _, err := f.blobs.Stat(t.Context(), hkey); err != nil {
		t.Errorf("no block map at %s: a node's cache refuses an artifact it cannot check: %v", hkey, err)
	}

	// metadata.json names the payload so a node can skip a build it cannot use before fetching it.
	mkey, _ := artifact.Key(buildID, artifact.NameMetadata)
	mrc, err := f.blobs.Get(t.Context(), mkey)
	if err != nil {
		t.Fatalf("no metadata.json: %v", err)
	}
	defer mrc.Close()
	mraw, _ := io.ReadAll(mrc)
	meta, err := artifact.ParseMetadata(mraw)
	if err != nil {
		t.Fatalf("parse metadata.json: %v", err)
	}
	if meta.PayloadName() != artifact.NameImage {
		t.Errorf("metadata.json payload = %q, want %q", meta.PayloadName(), artifact.NameImage)
	}
}

func TestAnUnpublishableBuildOnAMultiNodeFleetIsNotReportedOK(t *testing.T) {
	f := newBuildFleet(t, 3)
	f.s.artifacts.Store(nil)

	code, out := f.build(t, "mytool", []byte("FROM alpine\n"))
	if code != 200 {
		t.Fatalf("build returned %d: %v", code, out)
	}
	if ok, _ := out["ok"].(bool); ok {
		t.Fatalf("a build that could not be published on a 3-node fleet reported ok=true: %v\n"+
			"the image exists on one node and a create can be scheduled onto the other two", out)
	}
	// The build itself succeeded and the response must still say so.
	build, _ := out["build"].(map[string]any)
	if ok, _ := build["ok"].(bool); !ok {
		t.Errorf("build.ok = false, but the node built it fine: %v", out)
	}
	pub, _ := out["publish"].(map[string]any)
	if msg, _ := pub["error"].(string); msg == "" {
		t.Errorf("publish.error is empty; a caller told ok=false must be told why: %v", out)
	}
}

func TestASingleNodeInstallStillSucceedsWithNoArtifactStore(t *testing.T) {
	f := newBuildFleet(t, 1)
	f.s.artifacts.Store(nil)

	code, out := f.build(t, "mytool", []byte("FROM alpine\n"))
	if code != 200 {
		t.Fatalf("build returned %d: %v", code, out)
	}
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("a single-node install with no artifact store must still build: %v", out)
	}
}

// A ready row whose bytes are missing must be unrepresentable.
func TestAFailedExportDoesNotBlessAnUnfetchableBuild(t *testing.T) {
	f := newBuildFleet(t, 3)
	for _, n := range f.nodes {
		n.exportStatus = http.StatusNotFound
	}

	code, out := f.build(t, "mytool", []byte("FROM alpine\n"))
	if code != 200 {
		t.Fatalf("build returned %d: %v", code, out)
	}
	if ok, _ := out["ok"].(bool); ok {
		t.Errorf("a build whose image could not be exported reported ok=true: %v", out)
	}
	if _, err := f.st.ReadyBuildForTemplate(t.Context(), "sf-acme-mytool"); err == nil {
		t.Fatal("a build whose export failed is listed as READY — a node resolving it would " +
			"fetch bytes that were never published")
	}
}

// A custom image can only be launched on a docker node.
func TestTheBuildLandsOnADockerNodeWhenTheFleetIsMixed(t *testing.T) {
	f := newPublishFixture(t)
	now := time.Now()
	fc1 := newFakeBuildNode(t, "node-a-firecracker")
	fc2 := newFakeBuildNode(t, "node-b-firecracker")
	dock := newFakeBuildNode(t, "node-c-docker")
	cap := cluster.Capacity{Cores: 4, MemTotalMB: 8192, MemAvailMB: 8192}
	// Firecracker nodes sort first, so "first healthy node" would pick one of them.
	upsert(f.s.reg, fc1.id, fc1.srv.URL, "k1", "", nil, "v1", cap, now)
	upsert(f.s.reg, fc2.id, fc2.srv.URL, "k2", "", nil, "v1", cap, now)
	upsert(f.s.reg, dock.id, dock.srv.URL, "k3", "", nil, "v1", cap, now)
	f.s.reg.SetNodeMeta(fc1.id, cluster.NodeMeta{Runtime: "firecracker", Isolation: true, Templates: []string{"base"}, CanFetch: true})
	f.s.reg.SetNodeMeta(fc2.id, cluster.NodeMeta{Runtime: "firecracker", Isolation: true, Templates: []string{"base"}, CanFetch: true})
	f.s.reg.SetNodeMeta(dock.id, cluster.NodeMeta{Runtime: "docker", Isolation: true, CanFetch: true})

	fleet := &buildFleet{publishFixture: f, nodes: []*fakeBuildNode{fc1, fc2, dock}}
	if code, out := fleet.build(t, "mytool", []byte("FROM alpine\n")); code != 200 {
		t.Fatalf("build returned %d: %v", code, out)
	}

	if b, _ := dock.counts(); b != 1 {
		t.Errorf("the docker node ran %d builds, want 1", b)
	}
	for _, n := range []*fakeBuildNode{fc1, fc2} {
		if b, _ := n.counts(); b != 0 {
			t.Errorf("%s (firecracker) ran %d builds; a custom image can only be launched on a "+
				"docker node, so building it here produces a tag nothing will ever boot", n.id, b)
		}
	}
}

func TestNoHealthyNodeIsA503(t *testing.T) {
	f := newPublishFixture(t)
	fleet := &buildFleet{publishFixture: f}
	req, _ := http.NewRequestWithContext(t.Context(), "POST",
		fleet.srv.URL+"/v1/images/build?name=x", strings.NewReader("FROM alpine\n"))
	req.Header.Set("Authorization", "Bearer apikey")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("no healthy node = %d, want 503", resp.StatusCode)
	}
}
