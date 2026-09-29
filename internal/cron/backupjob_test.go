package cron

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// backupRunner is a fakeRunner that can also capture.
type backupRunner struct {
	fakeRunner
	mu      sync.Mutex
	backed  []string
	id      string
	backErr error
}

func (r *backupRunner) RunBackup(_ context.Context, orgID, sandboxID string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.backed = append(r.backed, orgID+"/"+sandboxID)
	return r.id, r.backErr
}

func (r *backupRunner) captures() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.backed...)
}

func backupJob() store.CronJob {
	j := job("cron_bkp", "0 3 * * *", store.CronKindBackup, 100)
	j.Kind = store.CronKindBackup
	return j
}

// A backup job's Command is the word "backup", not something to run in the
// sandbox. Handing it to a shell would run `backup` in the guest, which either
// fails or — worse — hits something that happens to be on the PATH.
func TestSweepDispatchesABackupJobToTheCapture(t *testing.T) {
	st := &fakeStore{due: []store.CronJob{backupJob()}}
	rn := &backupRunner{id: "bkp_123"}
	NewSweeper(st, rn, nil).SweepOnce(context.Background(), at("2026-08-02 03:00"))

	if got := rn.captures(); len(got) != 1 || got[0] != "org1/sb1" {
		t.Fatalf("captures = %v, want one for org1/sb1", got)
	}
	if cmds := rn.commands(); len(cmds) != 0 {
		t.Errorf("a backup job was handed to a shell: %v", cmds)
	}
	runs := st.runs()
	if len(runs) != 1 || runs[0].status != "ok" || !strings.Contains(runs[0].output, "bkp_123") {
		t.Fatalf("recorded run = %+v, want ok naming the backup id", runs)
	}
}

// A shell job must still go to the shell: the dispatch is on Kind, and a test
// that only checks the backup path would pass with every job captured.
func TestSweepStillRunsShellJobs(t *testing.T) {
	st := &fakeStore{due: []store.CronJob{job("cron_sh", "0 9 * * *", "npm test", 100)}}
	rn := &backupRunner{}
	NewSweeper(st, rn, nil).SweepOnce(context.Background(), at("2026-08-02 09:00"))

	if got := rn.captures(); len(got) != 0 {
		t.Errorf("a shell job triggered a capture: %v", got)
	}
	if cmds := rn.commands(); len(cmds) != 1 || !strings.Contains(cmds[0], "npm test") {
		t.Fatalf("commands = %v, want the shell command", cmds)
	}
}

func TestSweepRecordsABackupFailure(t *testing.T) {
	st := &fakeStore{due: []store.CronJob{backupJob()}}
	rn := &backupRunner{backErr: errors.New("object store is down")}
	NewSweeper(st, rn, nil).SweepOnce(context.Background(), at("2026-08-02 03:00"))

	runs := st.runs()
	if len(runs) != 1 || runs[0].status != "error" || !strings.Contains(runs[0].output, "object store") {
		t.Fatalf("recorded run = %+v, want an error naming the cause", runs)
	}
}

// A runner with no RunBackup must say so, not fall through to the shell.
func TestSweepRefusesABackupJobOnARunnerThatCannot(t *testing.T) {
	st := &fakeStore{due: []store.CronJob{backupJob()}}
	rn := &fakeRunner{}
	NewSweeper(st, rn, nil).SweepOnce(context.Background(), at("2026-08-02 03:00"))

	if cmds := rn.commands(); len(cmds) != 0 {
		t.Errorf("the backup job fell through to the shell: %v", cmds)
	}
	runs := st.runs()
	if len(runs) != 1 || runs[0].status != "error" {
		t.Fatalf("recorded run = %+v, want an error", runs)
	}
}
