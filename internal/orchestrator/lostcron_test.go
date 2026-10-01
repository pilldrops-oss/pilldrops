package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// Destroying a lost sandbox used the bare row delete, which leaves its cron
// jobs behind; on a private sandbox their command text was then listed to the
// whole org. Runs on Postgres when SANDBOXD_TEST_POSTGRES_URL is set.
func TestDestroyingALostSandboxRemovesItsCronJobs(t *testing.T) {
	st := store.OpenForTest(t)
	s := NewServer("k", "t")
	s.SetStore(st)
	const org = "u:lostcron"
	if err := st.SaveSandbox(t.Context(), store.SandboxRecord{
		ID: "sb-dead", OrgID: org, NodeID: "node-dead", Template: "base", State: "running",
		CreatedAt: 1, UpdatedAt: 1, ExpiresAt: 9e9,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateCronJob(t.Context(), store.CronJob{
		OrgID: org, SandboxID: "sb-dead", Name: "j", Schedule: "0 9 * * *", Command: "echo secret", Enabled: true,
	}, 9e9); err != nil {
		t.Fatal(err)
	}
	s.markLost([]string{"node-dead"})

	r := httptest.NewRequest(http.MethodDelete, "/v1/sandboxes/sb-dead", nil)
	r.SetPathValue("id", "sb-dead")
	r.Header.Set(orgOverride, org)
	r = r.WithContext(withOperator(r.Context()))
	w := httptest.NewRecorder()
	s.handleDestroy(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE lost sandbox = %d %s", w.Code, w.Body)
	}
	jobs, err := st.ListCronJobs(t.Context(), org)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Errorf("%d cron jobs outlived their destroyed sandbox: %+v", len(jobs), jobs)
	}
}
