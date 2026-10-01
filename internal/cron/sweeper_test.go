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

// fakeRunner records the captures the sweeper asked for.
type fakeRunner struct {
	mu     sync.Mutex
	backed []string
	id     string
	err    error
}

func (r *fakeRunner) RunBackup(_ context.Context, orgID, sandboxID string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.backed = append(r.backed, orgID+"/"+sandboxID)
	return r.id, r.err
}

func (r *fakeRunner) captures() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.backed...)
}

func job(id, sched string, next int64) store.CronJob {
	return store.CronJob{ID: id, OrgID: "org1", SandboxID: "sb1", Schedule: sched, Command: store.CronKindBackup,
		Kind: store.CronKindBackup, Enabled: true, NextRunAt: next, Timezone: "UTC"}
}

func TestSweepCapturesADueBackup(t *testing.T) {
	st := &fakeStore{due: []store.CronJob{job("cron_a", "0 3 * * *", 100)}}
	rn := &fakeRunner{id: "bkp_123"}
	NewSweeper(st, rn, nil).SweepOnce(context.Background(), at("2026-08-02 03:00"))

	if got := rn.captures(); len(got) != 1 || got[0] != "org1/sb1" {
		t.Fatalf("captures = %v, want one for org1/sb1", got)
	}
	runs := st.runs()
	if len(runs) != 1 || runs[0].status != "ok" || !strings.Contains(runs[0].output, "bkp_123") {
		t.Errorf("recorded = %+v, want one ok run naming the backup id", runs)
	}
}

func TestANonBackupRowIsNeitherClaimedNorRun(t *testing.T) {
	shell := job("cron_shell", "* * * * *", 100)
	shell.Kind, shell.Command = "shell", "rm -rf /data"
	st := &fakeStore{due: []store.CronJob{shell}}
	rn := &fakeRunner{}
	NewSweeper(st, rn, nil).SweepOnce(context.Background(), at("2026-08-02 09:00"))

	if got := rn.captures(); len(got) != 0 {
		t.Errorf("captured %v for a shell row", got)
	}
	if runs := st.runs(); len(runs) != 0 {
		t.Errorf("recorded %+v for a shell row", runs)
	}
}

func TestLosingTheClaimDoesNotRun(t *testing.T) {
	st := &fakeStore{
		due:    []store.CronJob{job("cron_a", "0 9 * * *", 100)},
		claims: map[string]bool{"cron_a": false},
	}
	rn := &fakeRunner{}
	NewSweeper(st, rn, nil).SweepOnce(context.Background(), at("2026-08-02 09:00"))

	if got := rn.captures(); len(got) != 0 {
		t.Errorf("captured %v after losing the claim — the job fires twice across replicas", got)
	}
	if runs := st.runs(); len(runs) != 0 {
		t.Errorf("recorded %+v after losing the claim", runs)
	}
}

func TestBackupFailureIsRecordedAndKeepsSchedule(t *testing.T) {
	st := &fakeStore{due: []store.CronJob{job("cron_a", "0 3 * * *", 100)}}
	rn := &fakeRunner{err: fmt.Errorf("object store is down")}
	NewSweeper(st, rn, nil).SweepOnce(context.Background(), at("2026-08-02 03:00"))

	runs := st.runs()
	if len(runs) != 1 || runs[0].status != "error" || !strings.Contains(runs[0].output, "object store") {
		t.Errorf("recorded = %+v, want an error naming the cause", runs)
	}
	if len(st.claimed) != 1 {
		t.Errorf("job was not rescheduled after a failed capture")
	}
}

// next_run_at 0 is the dormant marker DueCronJobs filters on.
func TestUnparseableScheduleIsParkedNotRetried(t *testing.T) {
	st := &fakeStore{due: []store.CronJob{job("cron_bad", "not a schedule", 100)}}
	rn := &fakeRunner{}
	NewSweeper(st, rn, nil).SweepOnce(context.Background(), at("2026-08-02 09:00"))

	if got := rn.captures(); len(got) != 0 {
		t.Errorf("captured %v for an unparseable schedule", got)
	}
	runs := st.runs()
	if len(runs) != 1 || runs[0].status != "error" {
		t.Errorf("recorded = %+v, want an error explaining the bad schedule", runs)
	}
}

func TestOneBadJobDoesNotBlockTheRest(t *testing.T) {
	st := &fakeStore{due: []store.CronJob{
		job("cron_bad", "nonsense", 100),
		job("cron_ok", "0 9 * * *", 100),
	}}
	rn := &fakeRunner{}
	NewSweeper(st, rn, nil).SweepOnce(context.Background(), at("2026-08-02 09:00"))

	if got := rn.captures(); len(got) != 1 {
		t.Errorf("captures = %v, want the healthy job to still run", got)
	}
}

func TestNextRunDoesNotDrift(t *testing.T) {
	var claimedNext int64
	st := &fakeStore{due: []store.CronJob{job("cron_a", "0 9 * * *", at("2026-08-02 09:00").Unix())}}
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
		j := job("cron_tz", "0 9 * * *", at("2026-08-02 09:00").Unix())
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
	j := job("cron_tz", "0 9 * * *", at("2026-08-02 09:00").Unix())
	j.Timezone = "Mars/Olympus_Mons"
	st := &fakeStore{due: []store.CronJob{j}, claims: map[string]bool{}}
	NewSweeper(st, &fakeRunner{}, func(id string, next int64) { claimedNext = next }).
		SweepOnce(context.Background(), at("2026-08-02 09:00"))

	if got, want := time.Unix(claimedNext, 0).UTC().Format("2006-01-02 15:04"), "2026-08-03 09:00"; got != want {
		t.Errorf("unknown tz = %sZ, want %sZ (UTC fallback)", got, want)
	}
}
