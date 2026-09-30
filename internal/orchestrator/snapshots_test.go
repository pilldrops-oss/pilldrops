package orchestrator

import (
	"bytes"
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
	"sync"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// fakeSnapshotNode is a sandboxd answering the snapshot-export API: capture,
// stream one object, drop the export directory.
type fakeSnapshotNode struct {
	srv *httptest.Server

	mu sync.Mutex
	// mark distinguishes one capture's bytes from the next, so a test can tell
	// which version a stored object came from.
	mark string
	// captures counts capture requests; exports names the directories deleted.
	captures int
	deleted  []string
	// payload is what the node claims it captured.
	payload string
	// rootfs, when set, is the root image every capture was taken against: the
	// capture record carries its digest (a lie when lieDigest) and the rootfs
	// object serves it. rootfsReads counts how often it was streamed.
	rootfs      []byte
	lieDigest   bool
	rootfsReads int
}

func newFakeSnapshotNode(t *testing.T) *fakeSnapshotNode {
	t.Helper()
	n := &fakeSnapshotNode{mark: "v1", payload: artifact.PayloadSnapshot}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes/{id}/snapshot-export", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		n.mu.Lock()
		n.captures++
		payload := n.payload
		n.mu.Unlock()
		kind, tag := "memory", ""
		if payload == artifact.NameImage {
			// A docker capture is a committed image and says so; the control
			// plane pulls it through the image export, not this API.
			kind, tag = "disk", "pd-ckpt-sb-1-export"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck // test
			"paused_ms": 42, "size_bytes": 3000, "payload": payload, "kind": kind, "tag": tag,
			"template": "base", "vcpu": 2, "mem_mb": 2048,
			"tap_name": "tap-sb1", "vm_ip": "10.128.7.2",
		})
	})
	// The docker half of a save rides the image export the fleet already has.
	mux.HandleFunc("GET /v1/images/export", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		mark := n.mark
		n.mu.Unlock()
		_, _ = w.Write([]byte(mark + ":image.tar:" + strings.Repeat("d", 96))) //nolint:errcheck // test
	})
	mux.HandleFunc("GET /v1/sandboxes/{id}/snapshot-export/{build}/{name}", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		mark, rootfs, lie := n.mark, n.rootfs, n.lieDigest
		if rootfs != nil && r.PathValue("name") == artifact.NameRootfs {
			n.rootfsReads++
		}
		n.mu.Unlock()
		if rootfs != nil {
			switch r.PathValue("name") {
			case artifact.NameRootfs:
				_, _ = w.Write(rootfs) //nolint:errcheck // test
				return
			case artifact.NameSnapMeta:
				sum := sha256.Sum256(rootfs)
				if lie {
					sum = sha256.Sum256([]byte("some other image"))
				}
				_ = json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck // test
					"template": "base", "rootfs_digest": "sha256:" + hex.EncodeToString(sum[:])})
				return
			}
		}
		// Each object is distinct and non-empty; a publisher that streamed the
		// same object three times would pass a "three files exist" check.
		_, _ = w.Write([]byte(mark + ":" + r.PathValue("name") + ":" +
			strings.Repeat("x", 64+len(r.PathValue("name")))))
	})
	mux.HandleFunc("DELETE /v1/sandboxes/{id}/snapshot-export/{build}", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		n.deleted = append(n.deleted, r.PathValue("build"))
		n.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	n.srv = httptest.NewServer(mux)
	t.Cleanup(n.srv.Close)
	return n
}

type snapshotFleet struct {
	*publishFixture
	node *fakeSnapshotNode
}

func newSnapshotFleet(t *testing.T) *snapshotFleet {
	t.Helper()
	f := newPublishFixture(t)
	n := newFakeSnapshotNode(t)
	if err := upsert(f.s.reg, "node-fc", n.srv.URL, "nodekey", "", nil, "v1",
		cluster.Capacity{Cores: 4, MemTotalMB: 8192, MemAvailMB: 8192}, time.Now()); err != nil {
		t.Fatal(err)
	}
	f.s.reg.SetNodeMeta("node-fc", cluster.NodeMeta{Runtime: "firecracker", Isolation: true, CanFetch: true})
	f.s.reg.Place("sb-1", "node-fc")
	return &snapshotFleet{publishFixture: f, node: n}
}

// do issues an API call as key's org.
func (f *snapshotFleet) do(t *testing.T, method, path, key, body string) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, f.srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	out["_raw"] = string(raw)
	return resp.StatusCode, out
}

// saveAndWait saves the name and polls until it leaves "building".
func (f *snapshotFleet) saveAndWait(t *testing.T, name string) map[string]any {
	t.Helper()
	status, out := f.do(t, "POST", "/v1/sandboxes/sb-1/snapshot", "apikey",
		fmt.Sprintf(`{"name":%q}`, name))
	if status != http.StatusAccepted {
		t.Fatalf("save %s: status %d body %v", name, status, out["_raw"])
	}
	if out["status"] != "building" {
		t.Fatalf("save answered status %v, want building", out["status"])
	}
	// Every response says which kind it is, from the 202 onward, so a caller
	// never has to assume a memory image exists.
	if k := out["snapshot_kind"]; k != "memory" && k != "disk" {
		t.Fatalf("the 202 reported snapshot_kind %v, want memory or disk", k)
	}
	// Poll for THIS version: the summary reports the newest ready version, so
	// while v2 builds it still answers v1 and a poll on the summary would
	// return at once with the previous version's answer.
	build, _ := out["build_id"].(string)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		_, got := f.do(t, "GET", "/v1/snapshots/"+name, "apikey", "")
		versions, _ := got["versions"].([]any)
		for _, v := range versions {
			m, _ := v.(map[string]any)
			if m["build_id"] == build && m["status"] != "building" {
				return m
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("snapshot %s (build %s) never left building", name, build)
	return nil
}

func objectPath(dir, buildID, name string) string { return filepath.Join(dir, buildID, name) }

// waitExportDropped waits for the node's export directory for build to be
// removed. It is dropped after the publish, not before: the objects stream off
// the node while the store is being written.
func (f *snapshotFleet) waitExportDropped(t *testing.T, build string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		f.node.mu.Lock()
		deleted := append([]string(nil), f.node.deleted...)
		f.node.mu.Unlock()
		for _, d := range deleted {
			if d == build {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the export directory for %s was never dropped; it is the size of the guest's RAM "+
		"and every save would leave one behind", build)
}

// waitPurged waits for a superseded version's row to read "deleted". The purge
// runs after the new version is marked ready — deliberately, so an older
// version stays bootable for the whole publish — so "ready" is not yet "purged".
func (f *snapshotFleet) waitPurged(t *testing.T, name, buildID string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		_, got := f.do(t, "GET", "/v1/snapshots/"+name, "apikey", "")
		versions, _ := got["versions"].([]any)
		for _, v := range versions {
			m, _ := v.(map[string]any)
			if m["build_id"] == buildID && m["status"] == "deleted" {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("version %s of %s was never purged after a newer one became ready", buildID, name)
}

func TestSavingASnapshotPublishesAllThreeObjectsAndOnlyThenIsReady(t *testing.T) {
	f := newSnapshotFleet(t)

	got := f.saveAndWait(t, "golden")
	if got["status"] != "ready" {
		t.Fatalf("snapshot did not become ready: %v", got)
	}
	if got["snapshot_kind"] != "memory" || got["runtime"] != "firecracker" {
		t.Errorf("a Firecracker snapshot reported kind %v runtime %v", got["snapshot_kind"], got["runtime"])
	}
	// The capture's facts reached the row, not just the object store.
	if got["template"] != "base" || got["vcpu"] != float64(2) || got["mem_mb"] != float64(2048) {
		t.Errorf("the row does not carry what the capture reported: %v", got)
	}
	buildV1, _ := got["build_id"].(string)
	if buildV1 == "" {
		t.Fatal("no build id")
	}

	// Every object, its block map, and metadata.json — and each object distinct.
	seen := map[string]bool{}
	for _, name := range artifact.SnapshotObjects() {
		raw, err := os.ReadFile(objectPath(f.dir, buildV1, name))
		if err != nil {
			t.Fatalf("%s of the published snapshot is missing: %v", name, err)
		}
		if len(raw) == 0 {
			t.Errorf("%s is empty", name)
		}
		if seen[string(raw)] {
			t.Errorf("%s holds the same bytes as another object; one stream was published three times", name)
		}
		seen[string(raw)] = true
		if !bytes.HasPrefix(raw, []byte("v1:"+name)) {
			t.Errorf("%s holds %q, not this capture's bytes", name, raw[:min(len(raw), 24)])
		}
		if _, err := os.Stat(objectPath(f.dir, buildV1, name+artifact.HeaderSuffix)); err != nil {
			t.Errorf("%s has no block map, so no node could fetch it: %v", name, err)
		}
	}
	rawMeta, err := os.ReadFile(objectPath(f.dir, buildV1, artifact.NameMetadata))
	if err != nil {
		t.Fatalf("metadata.json: %v", err)
	}
	meta, err := artifact.ParseMetadata(rawMeta)
	if err != nil {
		t.Fatal(err)
	}
	if meta.PayloadName() != artifact.PayloadSnapshot {
		t.Errorf("metadata says payload %q; a node would fetch it as a template rootfs", meta.PayloadName())
	}
	if meta.Snapshot == nil || meta.Snapshot.VMIP != "10.128.7.2" || meta.Snapshot.Template != "base" {
		t.Fatalf("metadata carries no usable snapshot block: %+v", meta.Snapshot)
	}

	// The node's export directory is dropped once the objects are in the store;
	// it is the size of the guest's RAM. It happens after the ready mark —
	// the objects are streamed off the node during the publish — so it is
	// waited for rather than assumed.
	f.waitExportDropped(t, buildV1)

	// The name resolves for a node that knows nothing but the template id.
	tmpl := store.SnapshotTemplateID("default", "golden")
	if b, err := f.st.ReadyBuildForTemplate(t.Context(), tmpl); err != nil || b != buildV1 {
		t.Fatalf("a node resolving %s got %q (err %v), want %s", tmpl, b, err, buildV1)
	}

	// Version 2 replaces it: new bytes, and version 1's objects are gone.
	f.node.mu.Lock()
	f.node.mark = "v2"
	f.node.mu.Unlock()
	got = f.saveAndWait(t, "golden")
	if got["version"] != float64(2) || got["status"] != "ready" {
		t.Fatalf("second save: %v", got)
	}
	buildV2, _ := got["build_id"].(string)
	if buildV2 == buildV1 {
		t.Fatal("the second version reused the first's build id")
	}
	if b, err := f.st.ReadyBuildForTemplate(t.Context(), tmpl); err != nil || b != buildV2 {
		t.Fatalf("after v2 a node resolves %q (err %v), want %s", b, err, buildV2)
	}
	f.waitPurged(t, "golden", buildV1)
	for _, name := range artifact.SnapshotObjects() {
		if _, err := os.Stat(objectPath(f.dir, buildV1, name)); err == nil {
			t.Errorf("version 1's %s is still in the store after version 2 became ready", name)
		}
		raw, err := os.ReadFile(objectPath(f.dir, buildV2, name))
		if err != nil || !bytes.HasPrefix(raw, []byte("v2:")) {
			t.Errorf("version 2's %s is not the second capture's bytes (%v)", name, err)
		}
	}
	// The row survives the purge, so the history stays legible.
	_, listed := f.do(t, "GET", "/v1/snapshots", "apikey", "")
	snaps, _ := listed["snapshots"].([]any)
	if len(snaps) != 2 {
		t.Fatalf("list shows %d versions, want 2 (v2 ready, v1 deleted): %v", len(snaps), listed["_raw"])
	}
	statuses := map[string]bool{}
	for _, s := range snaps {
		m, _ := s.(map[string]any)
		statuses[fmt.Sprint(m["status"])] = true
		if m["snapshot_kind"] != "memory" {
			t.Errorf("a listed version reports kind %v, want memory", m["snapshot_kind"])
		}
	}
	if !statuses["ready"] || !statuses["deleted"] {
		t.Errorf("listed statuses %v, want one ready and one deleted", statuses)
	}
}

func TestDeletingASnapshotRemovesEveryVersionAndTheNameStopsResolving(t *testing.T) {
	f := newSnapshotFleet(t)
	got := f.saveAndWait(t, "golden")
	buildID, _ := got["build_id"].(string)
	tmpl := store.SnapshotTemplateID("default", "golden")

	// Positive control: it resolves before the delete.
	if _, err := f.st.ReadyBuildForTemplate(t.Context(), tmpl); err != nil {
		t.Fatalf("the snapshot did not resolve before the delete: %v", err)
	}

	status, out := f.do(t, "DELETE", "/v1/snapshots/golden", "apikey", "")
	if status != http.StatusOK || out["deleted_versions"] != float64(1) {
		t.Fatalf("delete: status %d body %v", status, out["_raw"])
	}
	if _, err := f.st.ReadyBuildForTemplate(t.Context(), tmpl); !errors.Is(err, store.ErrNoReadyBuild) {
		t.Fatalf("after the delete the template still resolves (err %v)", err)
	}
	for _, name := range artifact.SnapshotObjects() {
		if _, err := os.Stat(objectPath(f.dir, buildID, name)); err == nil {
			t.Errorf("%s survived the delete", name)
		}
	}
	if status, _ := f.do(t, "DELETE", "/v1/snapshots/golden", "apikey", ""); status != http.StatusNotFound {
		t.Errorf("deleting the name twice answered %d, want 404", status)
	}
}

func TestASnapshotNameIsRefusedWhenItCannotBeATemplateID(t *testing.T) {
	f := newSnapshotFleet(t)
	for _, bad := range []string{"", "Golden", "has_underscore", "-lead", "a/b", strings.Repeat("a", 41)} {
		status, _ := f.do(t, "POST", "/v1/sandboxes/sb-1/snapshot", "apikey",
			fmt.Sprintf(`{"name":%q}`, bad))
		if status != http.StatusBadRequest {
			t.Errorf("name %q answered %d, want 400", bad, status)
		}
	}
	// Positive control: the same route accepts a legal name. Waited for, so the
	// save is not still writing into the test's directories when it ends.
	f.saveAndWait(t, "ok-1")
	// And a save of an unknown sandbox is a 404, not a build row nobody can reach.
	if status, _ := f.do(t, "POST", "/v1/sandboxes/sb-nope/snapshot", "apikey", `{"name":"other"}`); status != http.StatusNotFound {
		t.Errorf("saving an unplaced sandbox answered %d, want 404", status)
	}
}

func TestADockerSaveIsPublishedAsADiskImageAndSaysSo(t *testing.T) {
	f := newSnapshotFleet(t)
	f.s.reg.SetNodeMeta("node-fc", cluster.NodeMeta{Runtime: "docker", Isolation: true, CanFetch: true})
	f.node.mu.Lock()
	f.node.payload = artifact.NameImage
	f.node.mu.Unlock()

	got := f.saveAndWait(t, "golden")
	if got["status"] != "ready" {
		t.Fatalf("docker save: %v", got)
	}
	if got["snapshot_kind"] != "disk" {
		t.Fatalf("a Docker snapshot reported kind %v; it is a committed filesystem and carries no "+
			"memory image, and a caller must not be told otherwise", got["snapshot_kind"])
	}
	buildID, _ := got["build_id"].(string)
	if _, err := os.Stat(objectPath(f.dir, buildID, artifact.NameImage)); err != nil {
		t.Errorf("no %s was published for a docker snapshot: %v", artifact.NameImage, err)
	}
	if _, err := os.Stat(objectPath(f.dir, buildID, artifact.NameMemfile)); err == nil {
		t.Error("a docker snapshot published a memfile; there is no guest memory to capture")
	}
	raw, err := os.ReadFile(objectPath(f.dir, buildID, artifact.NameMetadata))
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := artifact.ParseMetadata(raw)
	if meta.PayloadName() != artifact.NameImage {
		t.Errorf("docker snapshot metadata says payload %q, want %q", meta.PayloadName(), artifact.NameImage)
	}
}

// A root image is stored once per digest and shared: a second capture of it
// does not stream it again, purging the first capture keeps it, and an image
// that does not match the digest its capture records is refused.
func TestSnapshotRootImageIsStoredOnceAndSurvivesAPurge(t *testing.T) {
	f := newSnapshotFleet(t)
	image := []byte(strings.Repeat("squashfs-image-bytes|", 400))
	f.node.mu.Lock()
	f.node.rootfs = image
	f.node.mu.Unlock()
	sum := sha256.Sum256(image)
	sharedKey, _ := artifact.Key("rfs-"+hex.EncodeToString(sum[:]), artifact.NameRootfs)

	v1 := f.saveAndWait(t, "shared")
	if v1["status"] != "ready" {
		t.Fatalf("v1 did not become ready: %v", v1)
	}
	if _, err := f.blobs.Stat(t.Context(), sharedKey); err != nil {
		t.Fatalf("the root image was not stored under its digest: %v", err)
	}
	f.node.mu.Lock()
	f.node.mark = "v2"
	f.node.mu.Unlock()
	v2 := f.saveAndWait(t, "shared")
	if v2["status"] != "ready" {
		t.Fatalf("v2 did not become ready: %v", v2)
	}
	f.node.mu.Lock()
	reads := f.node.rootfsReads
	f.node.mu.Unlock()
	if reads != 1 {
		t.Errorf("the root image was streamed %d times for two captures of it, want 1", reads)
	}
	f.waitPurged(t, "shared", v1["build_id"].(string))
	if _, err := f.blobs.Stat(t.Context(), sharedKey); err != nil {
		t.Fatalf("purging v1 deleted the root image v2 still reads: %v", err)
	}

	f.node.mu.Lock()
	f.node.rootfs = []byte("a different image under a digest that is not its own")
	f.node.lieDigest = true
	f.node.mu.Unlock()
	bad := f.saveAndWait(t, "liar")
	if bad["status"] == "ready" {
		t.Fatal("a root image that does not match its digest was published")
	}
}
