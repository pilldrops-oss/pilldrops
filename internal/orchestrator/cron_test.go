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

// On the rig, cron create and run-now went to the first healthy node, which
// answers 404 for a sandbox that lives on another. Each sandbox here lives on a
// different node, so one of them is always not the first.
func TestCronCreateAndRunReachTheNodeHostingTheSandbox(t *testing.T) {
	var mu sync.Mutex
	hits := map[string][]string{}
	nodeServer := func(id string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits[id] = append(hits[id], r.Method+" "+r.URL.Path)
			mu.Unlock()
			_, _ = w.Write([]byte(`{}`))
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}

	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "cron.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := NewServer("fleet-key", "nodetoken")
	s.SetStore(st)
	for _, n := range []string{"n1", "n2"} {
		if err := upsert(s.reg, n, nodeServer(n), "kk", "", nil, "",
			cluster.Capacity{Cores: 8, MemTotalMB: 64000, MemAvailMB: 64000}, s.now()); err != nil {
			t.Fatalf("upsert %s: %v", n, err)
		}
	}
	s.reg.Place("sb-on-n1", "n1")
	s.reg.Place("sb-on-n2", "n2")
	s.reg.Place("sb-private", "n1")
	for id, vis := range map[string]string{"sb-on-n1": "", "sb-on-n2": "", "sb-private": "private"} {
		if err := st.SaveSandbox(t.Context(), store.SandboxRecord{
			ID: id, Template: "base", OrgID: "u:tenant", State: "running",
			CreatedBy: "key_someone_else", Visibility: vis,
		}); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}
	tenant := orgKey(t, s, "u:tenant")

	call := func(method, path, body string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tenant)
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "203.0.113.9:1234"
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, req)
		return w.Code
	}

	for _, c := range []struct{ node, sandbox string }{{"n1", "sb-on-n1"}, {"n2", "sb-on-n2"}} {
		mu.Lock()
		hits = map[string][]string{}
		mu.Unlock()
		if code := call("POST", "/v1/cron", `{"sandbox_id":"`+c.sandbox+`","schedule":"* * * * *","command":"true"}`); code != http.StatusOK {
			t.Fatalf("create for %s = %d", c.sandbox, code)
		}
		job, err := st.CreateCronJob(t.Context(), store.CronJob{
			OrgID: "u:tenant", SandboxID: c.sandbox, Schedule: "* * * * *", Command: "true",
		}, 0)
		if err != nil {
			t.Fatalf("seed job: %v", err)
		}
		if code := call("POST", "/v1/cron/"+job.ID+"/run", ""); code != http.StatusOK {
			t.Fatalf("run-now for %s = %d", c.sandbox, code)
		}
		mu.Lock()
		want := []string{"POST /v1/cron", "POST /v1/cron/" + job.ID + "/run"}
		if got := hits[c.node]; strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s got %v, want %v (all hits %v)", c.node, got, want, hits)
		}
		mu.Unlock()
	}

	// Another org's job is not found here, so its sandbox's node is never told.
	other, err := st.CreateCronJob(t.Context(), store.CronJob{
		OrgID: "u:other", SandboxID: "sb-on-n1", Schedule: "* * * * *", Command: "true",
	}, 0)
	if err != nil {
		t.Fatalf("seed other job: %v", err)
	}
	if code := call("POST", "/v1/cron/"+other.ID+"/run", ""); code != http.StatusNotFound {
		t.Errorf("run-now on another org's job = %d, want 404", code)
	}
	// Another member's private sandbox is not the caller's to schedule on.
	mu.Lock()
	hits = map[string][]string{}
	mu.Unlock()
	if code := call("POST", "/v1/cron", `{"sandbox_id":"sb-private","schedule":"* * * * *","command":"true"}`); code != http.StatusNotFound {
		t.Errorf("create on another member's private sandbox = %d, want 404", code)
	}
	mu.Lock()
	if len(hits) != 0 {
		t.Errorf("a refused create still reached a node: %v", hits)
	}
	mu.Unlock()
}
