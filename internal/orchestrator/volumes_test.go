package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// volumeFleet is two nodes that can hold volumes and one that cannot, each
// recording the requests it was sent.
type nodeLog struct {
	mu   sync.Mutex
	seen []string
}

func (l *nodeLog) add(s string) { l.mu.Lock(); l.seen = append(l.seen, s); l.mu.Unlock() }
func (l *nodeLog) has(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, x := range l.seen {
		if x == s {
			return true
		}
	}
	return false
}

func volumeFleet(t *testing.T) (*Server, *store.Store, map[string]*nodeLog) {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "vol.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := NewServer("fleet-key", "nodetoken")
	s.SetStore(st)
	logs := map[string]*nodeLog{}
	for _, id := range []string{"n1", "n2", "novolumes"} {
		l := &nodeLog{}
		logs[id] = l
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			l.add(r.Method + " " + r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"sb-x","status":"running"}`))
		}))
		t.Cleanup(srv.Close)
		if err := upsert(s.reg, id, srv.URL, "kk", "", nil, "", cluster.Capacity{Cores: 8, MemTotalMB: 64000, MemAvailMB: 64000}, s.now()); err != nil {
			t.Fatal(err)
		}
		s.reg.SetNodeMeta(id, cluster.NodeMeta{Runtime: "firecracker", Isolation: true, Volumes: id != "novolumes"})
	}
	for _, v := range []store.Volume{
		{ID: "vol-on-n2", OrgID: "u:one", SizeMB: 1024, NodeID: "n2"},
		{ID: "vol-on-n1", OrgID: "u:one", SizeMB: 1024, NodeID: "n1"},
		{ID: "vol-theirs", OrgID: "u:two", SizeMB: 1024, NodeID: "n2"},
	} {
		if err := st.CreateVolume(t.Context(), v); err != nil {
			t.Fatal(err)
		}
	}
	return s, st, logs
}

func TestACreateAttachingAVolumeRunsOnTheVolumesNode(t *testing.T) {
	s, _, logs := volumeFleet(t)
	w := apiReq(t, s, "POST", "u:one", "/v1/sandboxes", `{"template":"base","volumes":[{"id":"vol-on-n2","path":"/data"}]}`)
	if w.Code/100 != 2 || !logs["n2"].has("POST /v1/sandboxes") || logs["n1"].has("POST /v1/sandboxes") {
		t.Fatalf("create = %d %q; n1 %v n2 %v: want it on n2 only", w.Code, w.Body.String(), logs["n1"].seen, logs["n2"].seen)
	}

	for body, want := range map[string]int{
		`{"template":"base","volumes":[{"id":"vol-on-n1","path":"/a"},{"id":"vol-on-n2","path":"/b"}]}`: http.StatusConflict,
		`{"template":"base","volumes":[{"id":"vol-theirs","path":"/data"}]}`:                            http.StatusNotFound,
	} {
		if w := apiReq(t, s, "POST", "u:one", "/v1/sandboxes", body); w.Code != want {
			t.Errorf("create %s = %d %q, want %d", body, w.Code, w.Body.String(), want)
		}
	}

	s.reg.SetCordon("n2", true)
	if w := apiReq(t, s, "POST", "u:one", "/v1/sandboxes", `{"template":"base","volumes":[{"id":"vol-on-n2","path":"/data"}]}`); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "vol-on-n2") {
		t.Errorf("volume's node unavailable = %d %q, want a 503 naming the volume", w.Code, w.Body.String())
	}
}

func TestVolumesArePlacedReadAndDeletedWhereTheyLive(t *testing.T) {
	s, _, logs := volumeFleet(t)
	if w := apiReq(t, s, "POST", "u:one", "/v1/volumes", `{"name":"x","size_mb":1024}`); w.Code/100 != 2 {
		t.Fatalf("create volume = %d %q", w.Code, w.Body.String())
	}
	if logs["novolumes"].has("POST /v1/volumes") {
		t.Error("a volume was placed on a node that cannot hold one")
	}

	w := apiReq(t, s, "GET", "u:one", "/v1/volumes", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "vol-on-n2") || strings.Contains(w.Body.String(), "vol-theirs") {
		t.Errorf("list = %d %q: want this org's volumes from the store only", w.Code, w.Body.String())
	}
	if w := apiReq(t, s, "GET", "u:one", "/v1/volumes/vol-theirs", ""); w.Code != http.StatusNotFound {
		t.Errorf("another org's volume = %d, want 404", w.Code)
	}
	if w := apiReq(t, s, "DELETE", "u:one", "/v1/volumes/vol-on-n2", ""); w.Code/100 != 2 || !logs["n2"].has("DELETE /v1/volumes/vol-on-n2") {
		t.Errorf("delete = %d; n2 saw %v: want it sent to the volume's node", w.Code, logs["n2"].seen)
	}
}
