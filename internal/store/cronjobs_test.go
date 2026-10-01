package store

import (
	"testing"
	"time"
)

// A node sweeps only jobs on sandboxes it owns, plus unstamped ones.
func TestDueCronJobsIsScopedToTheOwningNode(t *testing.T) {
	s := openTemp(t)
	ctx := t.Context()
	for id, node := range map[string]string{"sb-a": "node-a", "sb-b": "node-b", "sb-old": ""} {
		if err := s.SaveSandbox(ctx, SandboxRecord{ID: id, OrgID: "org", NodeID: node, State: "running"}); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
		if _, err := s.CreateCronJob(ctx, CronJob{
			OrgID: "org", SandboxID: id, Schedule: "* * * * *", Command: "true",
		}, time.Now().Unix()-1); err != nil {
			t.Fatalf("create job: %v", err)
		}
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
	if a := got("node-a"); len(a) != 2 || !a["sb-a"] || !a["sb-old"] {
		t.Errorf("node-a sees %v, want sb-a and the unstamped sb-old", a)
	}
	if all := got(""); len(all) != 3 {
		t.Errorf("unclustered sees %v, want every job", all)
	}
}
