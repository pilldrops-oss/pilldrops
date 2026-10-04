package store

import (
	"testing"
	"time"
)

// A node sweeps only jobs on sandboxes it owns, plus unstamped ones.
func TestDueCronJobsIsScopedToTheOwningNode(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	for id, node := range map[string]string{"sb-a": "node-a", "sb-b": "node-b", "sb-old": "", "sb-lost": "node-c", "sb-gone": "-"} {
		if node != "-" {
			if err := s.SaveSandbox(ctx, SandboxRecord{ID: id, OrgID: "org", NodeID: node, State: "running"}); err != nil {
				t.Fatalf("save %s: %v", id, err)
			}
		}
		if _, err := s.CreateCronJob(ctx, CronJob{
			OrgID: "org", SandboxID: id, Schedule: "* * * * *", Kind: CronKindBackup,
		}, time.Now().Unix()-1); err != nil {
			t.Fatalf("create job: %v", err)
		}
	}
	if _, err := s.MarkNodeSandboxesLost(ctx, "node-c"); err != nil {
		t.Fatal(err)
	}
	got := func(node string) map[string]bool {
		due, err := s.DueCronJobs(ctx, node, time.Now().Unix(), 10)
		if err != nil {
			t.Fatalf("due for %q: %v", node, err)
		}
		m := map[string]bool{}
		for _, j := range due {
			m[j.SandboxID] = true
		}
		return m
	}
	// Jobs with no live owner are every node's to record as missed.
	if a := got("node-a"); len(a) != 4 || !a["sb-a"] || !a["sb-old"] || !a["sb-lost"] || !a["sb-gone"] {
		t.Errorf("node-a sees %v, want sb-a, the unstamped sb-old and the ownerless sb-lost and sb-gone", a)
	}
	if b := got("node-b"); b["sb-a"] {
		t.Errorf("node-b sees node-a's live sandbox: %v", b)
	}
	if all := got(""); len(all) != 5 {
		t.Errorf("unclustered sees %v, want every job", all)
	}
}

// The engine runs backup schedules only. A shell row left from before the
// /v1/cron API moved out must never be due, or a sweeper would claim it.
func TestDueCronJobsSelectsBackupsOnly(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	past := time.Now().Unix() - 1
	shell, err := s.CreateCronJob(ctx, CronJob{
		OrgID: "org", SandboxID: "sb-1", Schedule: "* * * * *", Command: "rm -rf /data",
	}, past)
	if err != nil {
		t.Fatalf("create shell job: %v", err)
	}
	backup, err := s.CreateCronJob(ctx, CronJob{
		OrgID: "org", SandboxID: "sb-1", Schedule: "* * * * *", Kind: CronKindBackup,
	}, past)
	if err != nil {
		t.Fatalf("create backup job: %v", err)
	}
	for _, node := range []string{"", "node-a"} {
		due, err := s.DueCronJobs(ctx, node, time.Now().Unix(), 10)
		if err != nil {
			t.Fatalf("due for %q: %v", node, err)
		}
		if len(due) != 1 || due[0].ID != backup.ID {
			t.Errorf("node %q: due = %v, want only the backup job (not shell job %s)", node, due, shell.ID)
		}
	}
}
