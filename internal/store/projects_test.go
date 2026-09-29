package store

import (
	"testing"
	"time"
)

func TestProjectOutlivesItsSandboxes(t *testing.T) {
	st := openTemp(t)
	now := time.Now().Unix()

	p := Project{ID: "pr-1", OrgID: "acme", Owner: "u:1", Name: "checkout-api", Plan: `{"services":[{"id":"api"}]}`, CreatedAt: now}
	if err := st.SaveProject(t.Context(), p); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}

	run := Run{
		ID: "run-1", ProjectID: "pr-1", OrgID: "acme",
		PlanSnapshot: `{"services":[{"id":"api"}]}`,
		StartedAt:    now,
		Services:     []RunService{{ServiceID: "api", SandboxID: "sb-1", Verdict: "serving"}},
	}
	if err := st.RecordRun(t.Context(), run); err != nil {
		t.Fatalf("RecordRun: %v", err)
	}

	if err := st.EndRun(t.Context(), "run-1", now+900); err != nil {
		t.Fatalf("EndRun: %v", err)
	}

	got, err := st.GetProject(t.Context(), "acme", "pr-1")
	if err != nil || got == nil {
		t.Fatalf("GetProject after sandboxes gone: %v %+v", err, got)
	}
	if got.Name != "checkout-api" {
		t.Fatalf("name = %q", got.Name)
	}

	runs, err := st.ListRuns(t.Context(), "acme", "pr-1", 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns: %v n=%d", err, len(runs))
	}
	if runs[0].EndedAt != now+900 {
		t.Fatalf("run not ended: %+v", runs[0])
	}
	if len(runs[0].Services) != 1 || runs[0].Services[0].SandboxID != "sb-1" {
		t.Fatalf("services not round-tripped: %+v", runs[0].Services)
	}
}

func TestRunSnapshotIsFrozen(t *testing.T) {
	st := openTemp(t)
	now := time.Now().Unix()
	_ = st.SaveProject(t.Context(), Project{ID: "pr-1", OrgID: "acme", Name: "x", Plan: `{"v":1}`, CreatedAt: now})
	_ = st.RecordRun(t.Context(), Run{ID: "run-1", ProjectID: "pr-1", OrgID: "acme", PlanSnapshot: `{"v":1}`, StartedAt: now})

	if err := st.SaveProject(t.Context(), Project{ID: "pr-1", OrgID: "acme", Name: "x", Plan: `{"v":2}`, CreatedAt: now}); err != nil {
		t.Fatalf("SaveProject update: %v", err)
	}

	runs, _ := st.ListRuns(t.Context(), "acme", "pr-1", 10)
	if len(runs) != 1 || runs[0].PlanSnapshot != `{"v":1}` {
		t.Fatalf("snapshot was rewritten: %+v", runs)
	}
	p, _ := st.GetProject(t.Context(), "acme", "pr-1")
	if p.Plan != `{"v":2}` {
		t.Fatalf("project plan did not update: %q", p.Plan)
	}
}

func TestProjectsAreOrgScoped(t *testing.T) {
	st := openTemp(t)
	now := time.Now().Unix()
	_ = st.SaveProject(t.Context(), Project{ID: "pr-1", OrgID: "acme", Name: "mine", CreatedAt: now})
	_ = st.SaveProject(t.Context(), Project{ID: "pr-2", OrgID: "other", Name: "theirs", CreatedAt: now})

	list, err := st.ListProjects(t.Context(), "acme", 10)
	if err != nil || len(list) != 1 || list[0].ID != "pr-1" {
		t.Fatalf("ListProjects leaked across orgs: %v %+v", err, list)
	}
	if got, _ := st.GetProject(t.Context(), "acme", "pr-2"); got != nil {
		t.Fatalf("GetProject reached another org's project")
	}
	if runs, _ := st.ListRuns(t.Context(), "acme", "pr-2", 10); len(runs) != 0 {
		t.Fatalf("ListRuns reached another org's runs")
	}
}

func TestProjectListIsNewestActivityFirst(t *testing.T) {
	st := openTemp(t)
	now := time.Now().Unix()
	_ = st.SaveProject(t.Context(), Project{ID: "old", OrgID: "acme", Name: "old", CreatedAt: now - 900, LastRunAt: now - 900})
	_ = st.SaveProject(t.Context(), Project{ID: "fresh", OrgID: "acme", Name: "fresh", CreatedAt: now - 100, LastRunAt: now})

	list, _ := st.ListProjects(t.Context(), "acme", 10)
	if len(list) != 2 || list[0].ID != "fresh" {
		t.Fatalf("not ordered by recent activity: %+v", list)
	}
}

func TestArchivedProjectsAreHiddenNotDeleted(t *testing.T) {
	st := openTemp(t)
	now := time.Now().Unix()
	_ = st.SaveProject(t.Context(), Project{ID: "pr-1", OrgID: "acme", Name: "x", CreatedAt: now})
	_ = st.RecordRun(t.Context(), Run{ID: "run-1", ProjectID: "pr-1", OrgID: "acme", StartedAt: now})

	if err := st.ArchiveProject(t.Context(), "acme", "pr-1"); err != nil {
		t.Fatalf("ArchiveProject: %v", err)
	}
	if list, _ := st.ListProjects(t.Context(), "acme", 10); len(list) != 0 {
		t.Fatalf("archived project still listed")
	}
	if got, _ := st.GetProject(t.Context(), "acme", "pr-1"); got == nil {
		t.Fatalf("archived project was deleted")
	}
	if runs, _ := st.ListRuns(t.Context(), "acme", "pr-1", 10); len(runs) != 1 {
		t.Fatalf("archiving destroyed run history")
	}
}

func TestRunsAccumulate(t *testing.T) {
	st := openTemp(t)
	now := time.Now().Unix()
	_ = st.SaveProject(t.Context(), Project{ID: "pr-1", OrgID: "acme", Name: "x", CreatedAt: now})
	_ = st.RecordRun(t.Context(), Run{ID: "run-1", ProjectID: "pr-1", OrgID: "acme", PlanSnapshot: `{"v":1}`, StartedAt: now - 60})
	_ = st.EndRun(t.Context(), "run-1", now-30)
	_ = st.RecordRun(t.Context(), Run{ID: "run-2", ProjectID: "pr-1", OrgID: "acme", PlanSnapshot: `{"v":2}`, StartedAt: now})

	runs, _ := st.ListRuns(t.Context(), "acme", "pr-1", 10)
	if len(runs) != 2 || runs[0].ID != "run-2" {
		t.Fatalf("runs not accumulated newest-first: %+v", runs)
	}
	if runs[1].EndedAt == 0 {
		t.Fatalf("earlier run was mutated: %+v", runs[1])
	}
}

func TestSaveProjectIsIdempotent(t *testing.T) {
	st := openTemp(t)
	now := time.Now().Unix()
	p := Project{ID: "pr-1", OrgID: "acme", Name: "first", CreatedAt: now}
	if err := st.SaveProject(t.Context(), p); err != nil {
		t.Fatalf("first: %v", err)
	}
	p.Name = "renamed"
	if err := st.SaveProject(t.Context(), p); err != nil {
		t.Fatalf("second: %v", err)
	}
	list, _ := st.ListProjects(t.Context(), "acme", 10)
	if len(list) != 1 || list[0].Name != "renamed" {
		t.Fatalf("not upserted: %+v", list)
	}
}
