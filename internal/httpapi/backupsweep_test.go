package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/artifact"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/runtimetest"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// newBackupNode is a node that can capture: a fake runtime that checkpoints and
// an object store, with one sandbox on it.
func newBackupNode(t *testing.T) (url, key, sandboxID string, st *store.Store, api *Server, fake *runtimetest.Fake) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	_, key, err = st.CreateKey(t.Context(), "backups", "default")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	fake = runtimetest.NewFake()
	cfg := daemon.Config{
		Runtime:     fake,
		Store:       daemon.StoreAdapter{S: st},
		SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	}
	withBackups(&cfg, st)
	api = New(daemon.New(cfg), st)
	srv := httptest.NewServer(api.Routes())
	t.Cleanup(srv.Close)
	return srv.URL, key, createSandbox(t, srv.URL, key), st, api, fake
}

func backupJobOf(t *testing.T, st *store.Store, sandboxID string) (store.CronJob, bool) {
	t.Helper()
	jobs, err := st.ListCronJobs(t.Context(), "default")
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	for _, j := range jobs {
		if j.SandboxID == sandboxID && j.Kind == store.CronKindBackup {
			return j, true
		}
	}
	return store.CronJob{}, false
}

// Drives the sweeper with a controlled clock rather than waiting on the real ticker.
func TestScheduledBackupFiresAndCaptures(t *testing.T) {
	url, key, sb, st, api, _ := newBackupNode(t)
	if code, out := do(t, "POST", url+"/v1/sandboxes/"+sb+"/config", key,
		`{"backups":{"period":"1h"}}`); code != http.StatusOK {
		t.Fatalf("set schedule = %d (%v)", code, out)
	}
	job, ok := backupJobOf(t, st, sb)
	if !ok {
		t.Fatal("setting a schedule wrote no backup job")
	}

	api.cronSweeper().SweepOnce(t.Context(), time.Unix(job.NextRunAt, 0))

	after, _ := backupJobOf(t, st, sb)
	if after.LastStatus != "ok" || !strings.HasPrefix(after.LastOutput, "backup ") {
		t.Fatalf("last run = %q %q, want ok naming the backup", after.LastStatus, after.LastOutput)
	}
	if after.NextRunAt <= job.NextRunAt {
		t.Errorf("next_run_at = %d, want past %d: the schedule did not move on", after.NextRunAt, job.NextRunAt)
	}
	backups, err := st.ListSandboxBackups(t.Context(), "default", sb)
	if err != nil {
		t.Fatalf("list backups: %v", err)
	}
	if len(backups) != 1 || !strings.Contains(after.LastOutput, backups[0].ID) {
		t.Errorf("backups = %+v, want the one the run names (%q)", backups, after.LastOutput)
	}
}

// A shell row written by the /v1/cron API before it moved out stays in the
// table, since migrations are untouched. The engine must neither claim it
// (which would move its next_run_at) nor run its command.
func TestLegacyShellJobIsNeverClaimedOrRun(t *testing.T) {
	_, _, sb, st, api, fake := newBackupNode(t)
	due := time.Now().Add(-time.Minute).Unix()
	legacy, err := st.CreateCronJob(t.Context(), store.CronJob{
		OrgID: "default", SandboxID: sb, Schedule: "* * * * *", Command: "echo legacy-fired",
	}, due)
	if err != nil {
		t.Fatalf("create legacy job: %v", err)
	}

	api.cronSweeper().SweepOnce(t.Context(), time.Now())

	got, ok := st.CronJobByID(t.Context(), "default", legacy.ID)
	if !ok {
		t.Fatal("the legacy row vanished")
	}
	if got.NextRunAt != due || got.LastRunAt != 0 || got.LastStatus != "" {
		t.Errorf("legacy row = next %d, last run %d, status %q; want it untouched (next %d)",
			got.NextRunAt, got.LastRunAt, got.LastStatus, due)
	}
	for _, cmd := range fake.ExecLog(sb) {
		if strings.Contains(cmd, "legacy-fired") {
			t.Fatalf("the legacy shell command ran: %q", cmd)
		}
	}
}

func TestDestroyingASandboxDeletesItsBackupSchedule(t *testing.T) {
	url, key, sb, st, _, _ := newBackupNode(t)
	if code, out := do(t, "POST", url+"/v1/sandboxes/"+sb+"/config", key,
		`{"backups":{"period":"24h"}}`); code != http.StatusOK {
		t.Fatalf("set schedule = %d (%v)", code, out)
	}
	if _, ok := backupJobOf(t, st, sb); !ok {
		t.Fatal("setting a schedule wrote no backup job")
	}
	if code, _ := do(t, "DELETE", url+"/v1/sandboxes/"+sb, key, ""); code != http.StatusOK {
		t.Fatalf("destroy sandbox = %d", code)
	}
	if j, ok := backupJobOf(t, st, sb); ok {
		t.Errorf("backup job %s survived the sandbox it targets", j.ID)
	}
}

// A scheduled backup fired for a down node's sandbox recorded "sandbox not
// found", which reads as the sandbox being gone.
func TestScheduledBackupOfADownNodeSaysOwnerDown(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	blobs, err := artifact.NewFS(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	d := daemon.New(daemon.Config{
		Runtime: runtimetest.NewFake(), Store: daemon.StoreAdapter{S: st, NodeID: "node-a"}, NodeID: "node-a",
		Backups: st, Blobs: blobs, SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	api := New(d, st)
	api.SetNodeID("node-a")
	if err := st.SaveSandbox(t.Context(), store.SandboxRecord{ID: "sb-theirs", OrgID: "default", NodeID: "node-b", State: "lost"}); err != nil {
		t.Fatal(err)
	}
	for id, wantDown := range map[string]bool{"sb-unknown": false, "sb-theirs": true} {
		_, err := daemonBackupRunner{api}.RunBackup(t.Context(), "default", id)
		if err == nil || errors.Is(err, errOwnerDown) != wantDown {
			t.Errorf("%s: %v, want owner-down = %v", id, err, wantDown)
		}
	}
}
