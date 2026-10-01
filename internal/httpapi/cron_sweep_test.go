package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/runtime/process"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// Drives the sweeper with a controlled clock rather than waiting on the real ticker.
func TestScheduledJobActuallyFires(t *testing.T) {
	srv, key, sb, _, api := newCronSandboxWithAPI(t)

	code, out := do(t, "POST", srv+"/v1/cron", key,
		`{"sandbox_id":"`+sb+`","schedule":"* * * * *","command":"echo fired-unattended","timezone":"UTC"}`)
	if code != http.StatusCreated {
		t.Fatalf("create job = %d %v", code, out)
	}
	id, _ := out["id"].(string)

	api.cronSweeper().SweepOnce(context.Background(), time.Now().Add(2*time.Minute))

	_, out = do(t, "GET", srv+"/v1/cron", key, "")
	jobs, _ := out["jobs"].([]any)
	if len(jobs) == 0 {
		t.Fatal("job vanished")
	}
	j := jobs[0].(map[string]any)
	if j["id"] != id {
		t.Fatalf("unexpected job %v", j["id"])
	}
	if j["last_status"] != "ok" {
		t.Errorf("last_status = %v, want ok — the scheduled run did not happen (%v)", j["last_status"], j["last_output"])
	}
	if s, _ := j["last_output"].(string); !strings.Contains(s, "fired-unattended") {
		t.Errorf("last_output = %q, want the command's stdout", s)
	}
	if j["last_run_at"].(float64) == 0 {
		t.Error("last_run_at not recorded")
	}
	if j["next_run_at"].(float64) == 0 {
		t.Error("next_run_at cleared — the job would never fire again")
	}
}

func TestDestroyingASandboxRemovesItsJobs(t *testing.T) {
	srv, key, sb, _ := newCronSandbox(t)

	if code, out := do(t, "POST", srv+"/v1/cron", key,
		`{"sandbox_id":"`+sb+`","schedule":"* * * * *","command":"echo hi"}`); code != http.StatusCreated {
		t.Fatalf("create job = %d %v", code, out)
	}
	if code, _ := do(t, "DELETE", srv+"/v1/sandboxes/"+sb, key, ""); code != http.StatusOK {
		t.Fatalf("destroy sandbox failed")
	}

	_, out := do(t, "GET", srv+"/v1/cron", key, "")
	if jobs, _ := out["jobs"].([]any); len(jobs) != 0 {
		t.Errorf("%d job(s) survived the sandbox they target", len(jobs))
	}
}

// Every node of a fleet sweeps the one shared table. A node that won the claim
// for a job whose sandbox lives elsewhere recorded "sandbox not found" and
// moved the schedule on, so on the rig the job never ran.
func TestScheduledJobRunsOnceOnTheNodeThatHostsItsSandbox(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	_, key, err := st.CreateKey(t.Context(), "cron", "default")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	node := func(id string) (*Server, string) {
		d := daemon.New(daemon.Config{
			Runtime:     process.New(filepath.Join(dir, id)),
			Store:       daemon.StoreAdapter{S: st, NodeID: id},
			NodeID:      id,
			SnapshotDir: filepath.Join(dir, id+"-snap"),
			IdleTimeout: time.Hour, HardTTL: time.Hour,
		})
		api := New(d, st)
		api.SetNodeID(id)
		srv := httptest.NewServer(api.Routes())
		t.Cleanup(srv.Close)
		return api, srv.URL
	}
	apiA, urlA := node("node-a")
	apiB, _ := node("node-b")

	code, out := do(t, "POST", urlA+"/v1/sandboxes", key, `{"template":"base"}`)
	if code != http.StatusCreated {
		t.Fatalf("create sandbox = %d %v", code, out)
	}
	sb, _ := out["id"].(string)
	code, out = do(t, "POST", urlA+"/v1/cron", key,
		`{"sandbox_id":"`+sb+`","schedule":"* * * * *","command":"echo fired >> runs; cat runs","timezone":"UTC"}`)
	if code != http.StatusCreated {
		t.Fatalf("create job = %d %v", code, out)
	}

	// The first due minute only: a later one would hand node A a second chance.
	now := time.Unix(int64(out["next_run_at"].(float64)), 0)
	apiB.cronSweeper().SweepOnce(t.Context(), now)
	apiA.cronSweeper().SweepOnce(t.Context(), now)

	_, out = do(t, "GET", urlA+"/v1/cron", key, "")
	jobs, _ := out["jobs"].([]any)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %v", out)
	}
	j := jobs[0].(map[string]any)
	if j["last_status"] != "ok" || j["last_output"] != "fired\n" {
		t.Errorf("last run = %v %q, want ok with exactly one run", j["last_status"], j["last_output"])
	}
}

// A job whose sandbox's node was reaped is recorded as missed, once, by any
// node, and stays on schedule; before, no node selected it and it went silent.
func TestScheduledJobOfALostNodeIsRecordedAsMissed(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	_, key, err := st.CreateKey(t.Context(), "cron", "default")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	node := func(id string) (*Server, string) {
		d := daemon.New(daemon.Config{
			Runtime:     process.New(filepath.Join(dir, id)),
			Store:       daemon.StoreAdapter{S: st, NodeID: id},
			NodeID:      id,
			SnapshotDir: filepath.Join(dir, id+"-snap"),
			IdleTimeout: time.Hour, HardTTL: time.Hour,
		})
		api := New(d, st)
		api.SetNodeID(id)
		srv := httptest.NewServer(api.Routes())
		t.Cleanup(srv.Close)
		return api, srv.URL
	}
	apiA, _ := node("node-a")
	_, urlB := node("node-b")

	code, out := do(t, "POST", urlB+"/v1/sandboxes", key, `{"template":"base"}`)
	if code != http.StatusCreated {
		t.Fatalf("create sandbox = %d %v", code, out)
	}
	sb, _ := out["id"].(string)
	code, out = do(t, "POST", urlB+"/v1/cron", key,
		`{"sandbox_id":"`+sb+`","schedule":"* * * * *","command":"echo hi","timezone":"UTC"}`)
	if code != http.StatusCreated {
		t.Fatalf("create job = %d %v", code, out)
	}
	due := int64(out["next_run_at"].(float64))
	if _, err := st.MarkNodeSandboxesLost(t.Context(), "node-b"); err != nil {
		t.Fatal(err)
	}

	now := time.Unix(due, 0)
	apiA.cronSweeper().SweepOnce(t.Context(), now)
	apiA.cronSweeper().SweepOnce(t.Context(), now)

	jobs, err := st.ListCronJobs(t.Context(), "default")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %v, %v", jobs, err)
	}
	j := jobs[0]
	if j.LastStatus != "error" || j.LastOutput != "unavailable: owning node is down" {
		t.Errorf("last run = %q %q, want the owner-down miss recorded", j.LastStatus, j.LastOutput)
	}
	if j.NextRunAt <= due {
		t.Errorf("next_run_at = %d, want past %d: the schedule did not move on", j.NextRunAt, due)
	}
}
