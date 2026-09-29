package orchestrator

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// artifactReq issues a request to the read mount with the given bearer.
func (f *publishFixture) artifactReq(t *testing.T, method, key, bearer string, body io.Reader) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, f.srv.URL+artifact.ArtifactPathPrefix+key, body)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, key, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// seedNode registers a node and returns its derived node key.
func (f *publishFixture) seedNode(t *testing.T, id, zone string) string {
	t.Helper()
	upsert(f.s.reg, id, "http://"+id, f.s.nodeKeyFor(id, zone), zone, nil, "test",
		cluster.Capacity{MemTotalMB: 4096, MemAvailMB: 4096}, time.Now())
	return f.s.nodeKeyFor(id, zone)
}

func TestANodeCanReadTheSharedStore(t *testing.T) {
	f := newPublishFixture(t)
	key := f.seedNode(t, "n1", "default")
	if err := f.blobs.Put(t.Context(), "bld-1/rootfs", strings.NewReader("payload"), 7); err != nil {
		t.Fatal(err)
	}

	code, body := f.artifactReq(t, "GET", "bld-1/rootfs", key, nil)
	if code != http.StatusOK || body != "payload" {
		t.Errorf("a registered node could not read the shared store: %d %q", code, body)
	}
	if code, _ := f.artifactReq(t, "HEAD", "bld-1/rootfs", key, nil); code != http.StatusOK {
		t.Errorf("HEAD (which is Stat) got %d", code)
	}
}

// A writable mount behind the node key would let one compromised node replace what every node boots.
func TestTheReadMountRefusesWrites(t *testing.T) {
	f := newPublishFixture(t)
	key := f.seedNode(t, "n1", "default")
	if err := f.blobs.Put(t.Context(), "bld-1/rootfs", strings.NewReader("original"), 8); err != nil {
		t.Fatal(err)
	}

	for _, method := range []string{"PUT", "DELETE"} {
		code, body := f.artifactReq(t, method, "bld-1/rootfs", key, strings.NewReader("evil"))
		if code != http.StatusMethodNotAllowed {
			t.Errorf("%s with a node key got %d, want 405 — a node that can write the shared "+
				"store can replace what every other node boots: %s", method, code, body)
		}
	}

	rc, err := f.blobs.Get(t.Context(), "bld-1/rootfs")
	if err != nil {
		t.Fatalf("the object is gone after a refused DELETE: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "original" {
		t.Errorf("the object is now %q — a refused write changed it anyway", got)
	}
}

func TestTheReadMountNeedsACredential(t *testing.T) {
	f := newPublishFixture(t)
	f.seedNode(t, "n1", "default")
	if err := f.blobs.Put(t.Context(), "bld-1/rootfs", strings.NewReader("payload"), 7); err != nil {
		t.Fatal(err)
	}
	for _, bearer := range []string{"", "sf_node_wrong", "nodetoken"} {
		code, _ := f.artifactReq(t, "GET", "bld-1/rootfs", bearer, nil)
		if code != http.StatusUnauthorized {
			t.Errorf("bearer %q read the shared store with status %d, want 401", bearer, code)
		}
	}
}

// A well-formed derived key for an unregistered node is what a decommissioned box presents.
func TestAnUnregisteredNodesKeyDoesNotWork(t *testing.T) {
	f := newPublishFixture(t)
	f.seedNode(t, "n1", "default")
	stranger := f.s.nodeKeyFor("n-not-registered", "default")
	if code, _ := f.artifactReq(t, "GET", "bld-1/rootfs", stranger, nil); code != http.StatusUnauthorized {
		t.Errorf("a key derived for an unregistered node got %d, want 401", code)
	}
}

func TestRangeSurvivesTheMount(t *testing.T) {
	f := newPublishFixture(t)
	key := f.seedNode(t, "n1", "default")
	if err := f.blobs.Put(t.Context(), "bld-1/rootfs", strings.NewReader("0123456789"), 10); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(t.Context(), "GET", f.srv.URL+artifact.ArtifactPathPrefix+"bld-1/rootfs", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Range", "bytes=2-5")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent || string(b) != "2345" {
		t.Errorf("ranged read got %d %q, want 206 %q — a node fetching a block would get the "+
			"whole artifact instead", resp.StatusCode, b, "2345")
	}
}

// The StorageProvider a node constructs must work against this server end to end.
func TestTheClientAndTheMountAgree(t *testing.T) {
	f := newPublishFixture(t)
	key := f.seedNode(t, "n1", "default")
	if err := f.blobs.Put(t.Context(), "bld-1/rootfs", strings.NewReader("payload"), 7); err != nil {
		t.Fatal(err)
	}

	client, err := artifact.Open(artifact.KindControlPlane, artifact.Options{BaseURL: f.srv.URL, Credential: key})
	if err != nil {
		t.Fatalf("open the controlplane store the way a node does: %v", err)
	}
	rc, err := client.Get(t.Context(), "bld-1/rootfs")
	if err != nil {
		t.Fatalf("a node's own client could not read through the mount: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "payload" {
		t.Errorf("read %q through the node's client, want %q", got, "payload")
	}

	// Callers branch on ErrNotFound, so it must survive the transport.
	if _, err := client.Get(t.Context(), "bld-1/absent"); err == nil {
		t.Error("a missing artifact read as present")
	} else if !isNotFound(err) {
		t.Errorf("a missing artifact gave %v, want artifact.ErrNotFound", err)
	}

	if err := client.Put(t.Context(), "bld-1/rootfs", strings.NewReader("evil"), 4); err == nil {
		t.Error("a node wrote to the shared store through its own client")
	}
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(fmt.Sprint(err), artifact.ErrNotFound.Error())
}
