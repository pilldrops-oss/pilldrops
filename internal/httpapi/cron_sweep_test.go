package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cron"
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

	sw := cron.NewSweeper(api.st, api.cronRunner(), nil)
	sw.SweepOnce(context.Background(), time.Now().Add(2*time.Minute))

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
