package orchestrator

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// createRecorder is a node that accepts a create and remembers the body it got.
type createRecorder struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []sandbox.Spec
}

func newCreateRecorder(t *testing.T, id string) *createRecorder {
	t.Helper()
	n := &createRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body) //nolint:errcheck // test
		var spec sandbox.Spec
		_ = json.Unmarshal(raw, &spec) //nolint:errcheck // asserted on below
		n.mu.Lock()
		n.got = append(n.got, spec)
		n.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck // test
			"id": "sb-child-" + id, "app_url": "http://" + id + "/preview",
			"boot_type": "snapshot", "boot_ms": 42,
		})
	})
	n.srv = httptest.NewServer(mux)
	t.Cleanup(n.srv.Close)
	return n
}

func (n *createRecorder) last(t *testing.T) sandbox.Spec {
	t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.got) == 0 {
		t.Fatal("the node was never asked to create anything")
	}
	return n.got[len(n.got)-1]
}

// snapshotCreateFleet is one Firecracker node that answers creates, plus a
// Docker node that must never be chosen for a memory snapshot.
type snapshotCreateFleet struct {
	*snapshotFleet
	fc     *createRecorder
	docker *createRecorder
}

func newSnapshotCreateFleet(t *testing.T) *snapshotCreateFleet {
	t.Helper()
	f := newSnapshotFleet(t)
	// The capture node answers only the snapshot-export routes, so it is
	// cordoned out of placement; a create landing there would 404 on the node
	// and look like a routing bug rather than a scheduling one.
	f.s.reg.SetCordon("node-fc", true)
	fc := newCreateRecorder(t, "fc")
	dk := newCreateRecorder(t, "dk")
	// The Docker node is the *better* fit — Pick prefers the least free RAM that
	// still fits — so it is the node a create lands on unless the runtime is
	// pinned. Without that, "it went to the Firecracker node" would prove nothing.
	for _, n := range []struct {
		id, url, runtime string
		availMB          int
	}{
		{"node-create-fc", fc.srv.URL, "firecracker", 65536},
		{"node-create-dk", dk.srv.URL, "docker", 8192},
	} {
		if err := upsert(f.s.reg, n.id, n.url, "nodekey", "", nil, "v1",
			cluster.Capacity{Cores: 8, MemTotalMB: 65536, MemAvailMB: n.availMB}, time.Now()); err != nil {
			t.Fatal(err)
		}
		f.s.reg.SetNodeMeta(n.id, cluster.NodeMeta{Runtime: n.runtime, Isolation: true, CanFetch: true})
	}
	return &snapshotCreateFleet{snapshotFleet: f, fc: fc, docker: dk}
}

func TestCreateFromSnapshotFixesSizeAndPinsTheRuntime(t *testing.T) {
	f := newSnapshotCreateFleet(t)
	got := f.saveAndWait(t, "golden")
	if got["status"] != "ready" {
		t.Fatalf("save: %v", got)
	}

	// A size that disagrees with the capture is refused, with a code.
	status, out := f.do(t, "POST", "/v1/sandboxes", "apikey", `{"snapshot":"golden","vcpu":4}`)
	if status != http.StatusBadRequest || out["code"] != "snapshot_size_fixed" {
		t.Fatalf("a create asking for 4 vCPU from a 2 vCPU snapshot answered %d %v", status, out["_raw"])
	}
	status, out = f.do(t, "POST", "/v1/sandboxes", "apikey", `{"snapshot":"golden","memory_mb":4096}`)
	if status != http.StatusBadRequest || out["code"] != "snapshot_size_fixed" {
		t.Fatalf("a create asking for 4096 MB from a 2048 MB snapshot answered %d %v", status, out["_raw"])
	}
	// Naming both is a mistake, not a precedence question.
	status, out = f.do(t, "POST", "/v1/sandboxes", "apikey", `{"snapshot":"golden","template":"base"}`)
	if status != http.StatusBadRequest || out["code"] != "snapshot_and_template" {
		t.Fatalf("template+snapshot answered %d %v", status, out["_raw"])
	}
	// The wrong runtime is a refusal, not a silent reroute.
	status, out = f.do(t, "POST", "/v1/sandboxes", "apikey", `{"snapshot":"golden","runtime":"docker"}`)
	if status != http.StatusBadRequest || out["code"] != "snapshot_runtime_fixed" {
		t.Fatalf("a docker create from a firecracker snapshot answered %d %v", status, out["_raw"])
	}

	// The matching size is accepted, and so is silence.
	for _, body := range []string{`{"snapshot":"golden"}`, `{"snapshot":"golden","vcpu":2,"memory_mb":2048}`} {
		status, out := f.do(t, "POST", "/v1/sandboxes", "apikey", body)
		if status != http.StatusCreated {
			t.Fatalf("create %s answered %d %v", body, status, out["_raw"])
		}
		spec := f.fc.last(t)
		if spec.VCPU != 2 || spec.MemoryMB != 2048 {
			t.Errorf("the node was asked for %d vCPU / %d MB, want the snapshot's 2 / 2048",
				spec.VCPU, spec.MemoryMB)
		}
		if want := store.SnapshotTemplateID("default", "golden"); spec.Template != want {
			t.Errorf("the node was asked for template %q, want %q", spec.Template, want)
		}
		if spec.Snapshot != "golden" {
			t.Errorf("the node was not told which snapshot to wake (%q); it would boot the "+
				"template cold and run the start command", spec.Snapshot)
		}
	}
	// A memory snapshot never lands on a Docker node.
	f.docker.mu.Lock()
	onDocker := len(f.docker.got)
	f.docker.mu.Unlock()
	if onDocker != 0 {
		t.Errorf("%d creates from a Firecracker snapshot were placed on the Docker node", onDocker)
	}
}

func TestCreateFromAnUnreadySnapshotIs404(t *testing.T) {
	f := newSnapshotCreateFleet(t)

	status, out := f.do(t, "POST", "/v1/sandboxes", "apikey", `{"snapshot":"never-saved"}`)
	if status != http.StatusNotFound || out["code"] != "snapshot_not_ready" {
		t.Fatalf("a create from an unknown snapshot answered %d %v", status, out["_raw"])
	}
	// The rig created from a snapshot straight after saving it; the answer has
	// to say what to poll, not only that it is not ready.
	if msg, _ := out["error"].(string); !strings.Contains(msg, "GET /v1/snapshots/never-saved") {
		t.Errorf("snapshot_not_ready error %q does not say what to poll", msg)
	}
	// A name whose only version is still building is the same answer: not ready.
	if _, out := f.do(t, "POST", "/v1/sandboxes/sb-1/snapshot", "apikey", `{"name":"slow"}`); out["status"] != "building" {
		t.Fatalf("save did not report building: %v", out["_raw"])
	}
	// Positive control: once it is ready the same create succeeds.
	ready := f.saveAndWait(t, "ready-one")
	if ready["status"] != "ready" {
		t.Fatalf("save: %v", ready)
	}
	if status, out := f.do(t, "POST", "/v1/sandboxes", "apikey", `{"snapshot":"ready-one"}`); status != http.StatusCreated {
		t.Fatalf("create from a ready snapshot answered %d %v", status, out["_raw"])
	}
}
