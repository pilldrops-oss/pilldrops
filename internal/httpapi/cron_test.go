package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

func TestCronCreateAndList(t *testing.T) {
	srv, key, sb, _ := newCronSandbox(t)

	code, out := do(t, "POST", srv+"/v1/cron", key,
		`{"sandbox_id":"`+sb+`","schedule":"0 9 * * *","command":"npm test","name":"nightly","timezone":"Europe/London"}`)
	if code != http.StatusCreated {
		t.Fatalf("create job = %d %v", code, out)
	}
	if out["next_run_at"] == nil || out["next_run_at"].(float64) == 0 {
		t.Error("job created with no next run — it would never fire")
	}
	if out["enabled"] != true {
		t.Error("a new job must be enabled, or it silently does nothing")
	}

	code, out = do(t, "GET", srv+"/v1/cron", key, "")
	if code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	jobs, _ := out["jobs"].([]any)
	if len(jobs) != 1 {
		t.Fatalf("list returned %d jobs, want 1", len(jobs))
	}
}

func TestCronRejectsBadInputAtWriteTime(t *testing.T) {
	srv, key, sb, _ := newCronSandbox(t)

	for _, tc := range []struct{ name, body string }{
		{"bad schedule", `{"sandbox_id":"` + sb + `","schedule":"every tuesday-ish","command":"x"}`},
		{"empty command", `{"sandbox_id":"` + sb + `","schedule":"0 9 * * *","command":"  "}`},
		{"unknown timezone", `{"sandbox_id":"` + sb + `","schedule":"0 9 * * *","command":"x","timezone":"Mars/Base"}`},
		{"impossible date", `{"sandbox_id":"` + sb + `","schedule":"0 0 30 2 *","command":"x"}`},
	} {
		if code, out := do(t, "POST", srv+"/v1/cron", key, tc.body); code != http.StatusBadRequest {
			t.Errorf("%s = %d %v, want 400", tc.name, code, out)
		}
	}
}

// Cross-tenant scheduling must 404, not 403, so the id's existence is not confirmed.
func TestCronCannotTargetAnotherOrgsSandbox(t *testing.T) {
	srv, _, sb, st := newCronSandbox(t)

	_, otherKey, err := st.CreateKey(t.Context(), "other", "org_other")
	if err != nil {
		t.Fatal(err)
	}
	code, _ := do(t, "POST", srv+"/v1/cron", otherKey,
		`{"sandbox_id":"`+sb+`","schedule":"0 9 * * *","command":"whoami"}`)
	if code != http.StatusNotFound {
		t.Errorf("cross-tenant schedule = %d, want 404", code)
	}
}

func TestCronToggleAndDelete(t *testing.T) {
	srv, key, sb, _ := newCronSandbox(t)

	_, out := do(t, "POST", srv+"/v1/cron", key,
		`{"sandbox_id":"`+sb+`","schedule":"*/5 * * * *","command":"echo hi"}`)
	id, _ := out["id"].(string)

	if code, _ := do(t, "POST", srv+"/v1/cron/"+id+"/toggle", key, `{"enabled":false}`); code != http.StatusOK {
		t.Fatalf("disable = %d", code)
	}
	_, out = do(t, "GET", srv+"/v1/cron", key, "")
	jobs, _ := out["jobs"].([]any)
	j := jobs[0].(map[string]any)
	if j["enabled"] != false {
		t.Error("job still enabled after disable")
	}
	if j["next_run_at"].(float64) == 0 {
		t.Error("disabling cleared the schedule — re-enabling would lose it")
	}

	if code, _ := do(t, "DELETE", srv+"/v1/cron/"+id, key, ""); code != http.StatusOK {
		t.Fatalf("delete = %d", code)
	}
	if code, _ := do(t, "DELETE", srv+"/v1/cron/"+id, key, ""); code != http.StatusNotFound {
		t.Error("deleting twice should 404 the second time")
	}
}

func TestCronRunNowExecutes(t *testing.T) {
	srv, key, sb, _ := newCronSandbox(t)

	_, out := do(t, "POST", srv+"/v1/cron", key,
		`{"sandbox_id":"`+sb+`","schedule":"0 9 * * *","command":"echo scheduled-hello"}`)
	id, _ := out["id"].(string)

	code, out := do(t, "POST", srv+"/v1/cron/"+id+"/run", key, "")
	if code != http.StatusOK {
		t.Fatalf("run now = %d %v", code, out)
	}
	if out["status"] != "ok" {
		t.Errorf("status = %v, want ok (%v)", out["status"], out)
	}
	if s, _ := out["output"].(string); !strings.Contains(s, "scheduled-hello") {
		t.Errorf("output = %q, want the command's stdout", s)
	}
}

// A failing command is a failed run, not a 500.
func TestCronRunNowReportsFailure(t *testing.T) {
	srv, key, sb, _ := newCronSandbox(t)

	_, out := do(t, "POST", srv+"/v1/cron", key,
		`{"sandbox_id":"`+sb+`","schedule":"0 9 * * *","command":"exit 3"}`)
	id, _ := out["id"].(string)

	code, out := do(t, "POST", srv+"/v1/cron/"+id+"/run", key, "")
	if code != http.StatusOK {
		t.Fatalf("run now = %d, want 200 — a failing command is a result, not a server error", code)
	}
	if out["status"] != "failed" {
		t.Errorf("status = %v, want failed", out["status"])
	}
	if ec, _ := out["exit_code"].(float64); ec != 3 {
		t.Errorf("exit_code = %v, want 3", out["exit_code"])
	}
}

func TestCronShellJobStillHasNoAgentMode(t *testing.T) {
	srv, key, sb, st := newCronSandbox(t)

	code, _ := do(t, "POST", srv+"/v1/cron", key,
		`{"sandbox_id":"`+sb+`","schedule":"0 9 * * *","command":"npm test"}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d", code)
	}
	jobs, err := st.ListCronJobs(t.Context(), "default")
	if err != nil || len(jobs) != 1 {
		t.Fatalf("list: %v %d", err, len(jobs))
	}
	if jobs[0].AgentMode != "" {
		t.Errorf("agent_mode = %q on a plain shell job — every existing job would be "+
			"rerouted through an agent", jobs[0].AgentMode)
	}
}

// The engine schedules shell commands; an agent prompt has nothing to run it.
func TestCronRefusesAgentMode(t *testing.T) {
	srv, key := newTestServer(t)
	id := createSandbox(t, srv.URL, key)
	for _, mode := range []string{"guided", "autonomous"} {
		code, out := do(t, "POST", srv.URL+"/v1/cron", key,
			`{"sandbox_id":"`+id+`","schedule":"*/5 * * * *","command":"summarise","agent_mode":"`+mode+`"}`)
		if code != 400 {
			t.Fatalf("agent_mode %q = %d %v, want 400", mode, code, out)
		}
		if msg, _ := out["error"].(string); !strings.Contains(msg, "agent_mode is not supported") {
			t.Errorf("agent_mode %q error = %q", mode, msg)
		}
	}
}

// A job acts on its sandbox, so on a private sandbox only its creator and an
// operator may run, disable or delete it, as with the sandbox itself.
func TestCronJobOnAPrivateSandboxIsTheCreators(t *testing.T) {
	srv, st, _ := newTestServerWithStore(t)
	k1, k2, kop := twoMembersAndAnOperator(t, st)
	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", k1, `{"template":"base","visibility":"private"}`)
	if code != http.StatusCreated {
		t.Fatalf("private create = %d (%v)", code, out)
	}
	code, out = do(t, "POST", srv.URL+"/v1/cron", k1,
		`{"sandbox_id":"`+str(out, "id")+`","schedule":"0 9 * * *","command":"true"}`)
	if code != http.StatusCreated {
		t.Fatalf("cron create = %d (%v)", code, out)
	}
	job := srv.URL + "/v1/cron/" + str(out, "id")

	for _, probe := range []struct{ method, path, body string }{
		{"POST", job + "/run", ""},
		{"POST", job + "/toggle", `{"enabled":false}`},
		{"DELETE", job, ""},
	} {
		if code, _ := do(t, probe.method, probe.path, k2, probe.body); code != http.StatusNotFound {
			t.Errorf("another member %s %s = %d, want 404", probe.method, probe.path, code)
		}
	}
	if code, _ := do(t, "POST", job+"/toggle", kop, `{"enabled":false}`); code != http.StatusOK {
		t.Errorf("operator toggle = %d, want 200", code)
	}
	if code, _ := do(t, "POST", job+"/run", k1, ""); code != http.StatusOK {
		t.Errorf("creator run = %d, want 200", code)
	}
	if code, _ := do(t, "DELETE", job, k1, ""); code != http.StatusOK {
		t.Errorf("creator delete = %d, want 200", code)
	}
}

// The list carries each job's command, so it follows the same rule.
func TestCronListHidesJobsOnAnotherMembersPrivateSandbox(t *testing.T) {
	srv, st, _ := newTestServerWithStore(t)
	k1, k2, kop := twoMembersAndAnOperator(t, st)
	_, priv := do(t, "POST", srv.URL+"/v1/sandboxes", k1, `{"template":"base","visibility":"private"}`)
	_, shared := do(t, "POST", srv.URL+"/v1/sandboxes", k1, `{"template":"base"}`)
	for _, sb := range []string{str(priv, "id"), str(shared, "id")} {
		if code, out := do(t, "POST", srv.URL+"/v1/cron", k1,
			`{"sandbox_id":"`+sb+`","schedule":"0 9 * * *","command":"echo secret-`+sb+`"}`); code != http.StatusCreated {
			t.Fatalf("cron create = %d (%v)", code, out)
		}
	}
	listed := func(key string) int {
		code, out := do(t, "GET", srv.URL+"/v1/cron", key, "")
		if code != http.StatusOK {
			t.Fatalf("list = %d (%v)", code, out)
		}
		jobs, _ := out["jobs"].([]any)
		return len(jobs)
	}
	if n := listed(k2); n != 1 {
		t.Errorf("another member sees %d jobs, want 1: the private sandbox's job leaked", n)
	}
	if n := listed(k1); n != 2 {
		t.Errorf("the creator sees %d jobs, want 2", n)
	}
	if n := listed(kop); n != 2 {
		t.Errorf("an operator sees %d jobs, want 2", n)
	}
}
