package cron

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// fakeStore records what the sweeper did.
type fakeStore struct {
	mu       sync.Mutex
	due      []store.CronJob
	claims   map[string]bool // id -> whether ClaimCronJob returned true
	claimed  []string
	recorded []recordedRun
	claimErr error
}

type recordedRun struct{ id, status, output string }

func (f *fakeStore) DueCronJobs(ctx context.Context, now int64, limit int) ([]store.CronJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.CronJob(nil), f.due...), nil
}

func (f *fakeStore) ClaimCronJob(ctx context.Context, id string, expected, next int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return false, f.claimErr
	}
	f.claimed = append(f.claimed, id)
	won, ok := f.claims[id]
	return !ok || won, nil // default: this sweeper wins the claim
}

func (f *fakeStore) RecordCronRun(ctx context.Context, id, status, output string, ranAt int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded = append(f.recorded, recordedRun{id, status, output})
	return nil
}

func (f *fakeStore) runs() []recordedRun {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRun(nil), f.recorded...)
}

// fakeRunner records the commands the sweeper asked a sandbox to run.
type fakeRunner struct {
	mu   sync.Mutex
	got  []string
	out  string
	code int
	err  error
}

func (r *fakeRunner) RunInSandbox(ctx context.Context, orgID, sandboxID, cmd string) (string, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, orgID+"/"+sandboxID+": "+cmd)
	return r.out, r.code, r.err
}

func (r *fakeRunner) commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

func job(id, sched, cmd string, next int64) store.CronJob {
	return store.CronJob{ID: id, OrgID: "org1", SandboxID: "sb1", Schedule: sched, Command: cmd,
		Enabled: true, NextRunAt: next, Timezone: "UTC"}
}

func TestSweepRunsDueJob(t *testing.T) {
	st := &fakeStore{due: []store.CronJob{job("cron_a", "0 9 * * *", "npm test", 100)}}
	rn := &fakeRunner{out: "3 passed", code: 0}
	s := NewSweeper(st, rn, nil)

	s.SweepOnce(context.Background(), at("2026-08-02 09:00"))

	if got := rn.commands(); len(got) != 1 || !strings.Contains(got[0], "org1/sb1: npm test") {
		t.Fatalf("commands = %v, want the job's command in its sandbox", got)
	}
	runs := st.runs()
	if len(runs) != 1 || runs[0].status != "ok" || runs[0].output != "3 passed" {
		t.Errorf("recorded = %+v, want one ok run carrying the output", runs)
	}
}

func TestLosingTheClaimDoesNotRun(t *testing.T) {
	st := &fakeStore{
		due:    []store.CronJob{job("cron_a", "0 9 * * *", "send-invoices", 100)},
		claims: map[string]bool{"cron_a": false},
	}
	rn := &fakeRunner{}
	NewSweeper(st, rn, nil).SweepOnce(context.Background(), at("2026-08-02 09:00"))

	if got := rn.commands(); len(got) != 0 {
		t.Errorf("ran %v after losing the claim — the job fires twice across replicas", got)
	}
	if runs := st.runs(); len(runs) != 0 {
		t.Errorf("recorded %+v after losing the claim", runs)
	}
}

func TestNonZeroExitIsRecordedAndKeepsSchedule(t *testing.T) {
	st := &fakeStore{due: []store.CronJob{job("cron_a", "*/5 * * * *", "false", 100)}}
	rn := &fakeRunner{out: "boom", code: 1}
	NewSweeper(st, rn, nil).SweepOnce(context.Background(), at("2026-08-02 09:00"))

	runs := st.runs()
	if len(runs) != 1 || runs[0].status != "failed" {
		t.Errorf("recorded = %+v, want status failed", runs)
	}
	if len(st.claimed) != 1 {
		t.Errorf("job was not rescheduled after a failing command")
	}
}

func TestUnreachableSandboxRecordsError(t *testing.T) {
	st := &fakeStore{due: []store.CronJob{job("cron_a", "*/5 * * * *", "echo hi", 100)}}
	rn := &fakeRunner{err: fmt.Errorf("sandbox not found")}
	NewSweeper(st, rn, nil).SweepOnce(context.Background(), at("2026-08-02 09:00"))

	runs := st.runs()
	if len(runs) != 1 || runs[0].status != "error" {
		t.Errorf("recorded = %+v, want status error (not 'failed')", runs)
	}
	if !strings.Contains(runs[0].output, "sandbox not found") {
		t.Errorf("output %q does not say why it could not run", runs[0].output)
	}
}

// next_run_at 0 is the dormant marker DueCronJobs filters on.
func TestUnparseableScheduleIsParkedNotRetried(t *testing.T) {
	st := &fakeStore{due: []store.CronJob{job("cron_bad", "not a schedule", "echo hi", 100)}}
	rn := &fakeRunner{}
	NewSweeper(st, rn, nil).SweepOnce(context.Background(), at("2026-08-02 09:00"))

	if got := rn.commands(); len(got) != 0 {
		t.Errorf("ran %v for an unparseable schedule", got)
	}
	runs := st.runs()
	if len(runs) != 1 || runs[0].status != "error" {
		t.Errorf("recorded = %+v, want an error explaining the bad schedule", runs)
	}
}

func TestOneBadJobDoesNotBlockTheRest(t *testing.T) {
	st := &fakeStore{due: []store.CronJob{
		job("cron_bad", "nonsense", "no", 100),
		job("cron_ok", "0 9 * * *", "yes", 100),
	}}
	rn := &fakeRunner{out: "done"}
	NewSweeper(st, rn, nil).SweepOnce(context.Background(), at("2026-08-02 09:00"))

	if got := rn.commands(); len(got) != 1 || !strings.Contains(got[0], "yes") {
		t.Errorf("commands = %v, want the healthy job to still run", got)
	}
}

func TestNextRunDoesNotDrift(t *testing.T) {
	var claimedNext int64
	st := &fakeStore{due: []store.CronJob{job("cron_a", "0 9 * * *", "x", at("2026-08-02 09:00").Unix())}}
	st.claims = map[string]bool{}
	rn := &fakeRunner{}
	s := NewSweeper(st, rn, func(id string, next int64) { claimedNext = next })

	// Sweep notices 40 seconds late.
	s.SweepOnce(context.Background(), at("2026-08-02 09:00").Add(40*time.Second))

	if want := at("2026-08-03 09:00").Unix(); claimedNext != want {
		t.Errorf("next run = %s, want %s — the schedule drifted with sweep latency",
			time.Unix(claimedNext, 0).UTC().Format("2006-01-02 15:04:05"),
			time.Unix(want, 0).UTC().Format("2006-01-02 15:04:05"))
	}
}

func TestScheduleIsEvaluatedInTheJobsTimezone(t *testing.T) {
	for _, tc := range []struct {
		tz   string
		want string // the instant, in UTC
	}{
		// Base is 09:00Z on Aug 2; Dubai is already past 9am local, New York is not.
		{"UTC", "2026-08-03 09:00"},              // 09:00Z is exactly 9am → next is tomorrow
		{"Asia/Dubai", "2026-08-03 05:00"},       // 09:00Z = 13:00 +04, past 9am → tomorrow 09:00 +04
		{"America/New_York", "2026-08-02 13:00"}, // 09:00Z = 05:00 EDT, before 9am → today 09:00 EDT
	} {
		var claimedNext int64
		j := job("cron_tz", "0 9 * * *", "x", at("2026-08-02 09:00").Unix())
		j.Timezone = tc.tz
		st := &fakeStore{due: []store.CronJob{j}, claims: map[string]bool{}}
		s := NewSweeper(st, &fakeRunner{}, func(id string, next int64) { claimedNext = next })
		s.SweepOnce(context.Background(), at("2026-08-02 09:00"))

		if got := time.Unix(claimedNext, 0).UTC().Format("2006-01-02 15:04"); got != tc.want {
			t.Errorf("tz %s: next run = %sZ, want %sZ", tc.tz, got, tc.want)
		}
	}
}

func TestUnknownTimezoneFallsBackToUTCNotLocal(t *testing.T) {
	var claimedNext int64
	j := job("cron_tz", "0 9 * * *", "x", at("2026-08-02 09:00").Unix())
	j.Timezone = "Mars/Olympus_Mons"
	st := &fakeStore{due: []store.CronJob{j}, claims: map[string]bool{}}
	NewSweeper(st, &fakeRunner{}, func(id string, next int64) { claimedNext = next }).
		SweepOnce(context.Background(), at("2026-08-02 09:00"))

	if got, want := time.Unix(claimedNext, 0).UTC().Format("2006-01-02 15:04"), "2026-08-03 09:00"; got != want {
		t.Errorf("unknown tz = %sZ, want %sZ (UTC fallback)", got, want)
	}
}
