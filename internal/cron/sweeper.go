package cron

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// Store is the persistence the sweeper needs.
type Store interface {
	DueCronJobs(ctx context.Context, now int64, limit int) ([]store.CronJob, error)
	ClaimCronJob(ctx context.Context, id string, expected, next int64) (bool, error)
	RecordCronRun(ctx context.Context, id, status, output string, ranAt int64) error
}

// Runner executes a command inside a customer's workstation.
type Runner interface {
	RunInSandbox(ctx context.Context, orgID, sandboxID, cmd string) (stdout string, exitCode int, err error)
}

// AgentRunner submits a scheduled prompt as an agent turn. A separate interface
// so a runner without it refuses agent jobs instead of falling back to exec.
type AgentRunner interface {
	RunAgentTurn(ctx context.Context, orgID, sandboxID, prompt, mode string) (output string, err error)
}

// BackupRunner captures a scheduled backup. Separate for the same reason as
// AgentRunner: a runner without it refuses backup jobs rather than handing
// j.Command — which is the word "backup" — to a shell.
type BackupRunner interface {
	RunBackup(ctx context.Context, orgID, sandboxID string) (backupID string, err error)
}

// SweepInterval matches the minute granularity of a cron expression.
const SweepInterval = time.Minute

// maxPerSweep bounds one tick so a backlog drains over several ticks.
const maxPerSweep = 100

// sweepConcurrency bounds how many due jobs one sweep executes at once. Jobs
// run in parallel so a minutes-long agent turn cannot stall the schedule; the
// claim is already an atomic compare-and-swap, so no further coordination is needed.
const sweepConcurrency = 8

// Sweeper fires scheduled jobs.
type Sweeper struct {
	st  Store
	run Runner
	// onClaim is a test seam: it observes the next_run_at written for a job.
	onClaim func(id string, next int64)
}

func NewSweeper(st Store, run Runner, onClaim func(id string, next int64)) *Sweeper {
	return &Sweeper{st: st, run: run, onClaim: onClaim}
}

// Run sweeps every SweepInterval until ctx is cancelled. A sweep still in
// flight suppresses the next tick rather than overlapping it.
func (s *Sweeper) Run(ctx context.Context) {
	t := time.NewTicker(SweepInterval)
	defer t.Stop()
	done := make(chan struct{}, 1)
	inFlight := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			inFlight = false
		case now := <-t.C:
			if inFlight {
				continue // previous sweep still draining
			}
			inFlight = true
			go func() {
				s.SweepOnce(ctx, now)
				done <- struct{}{}
			}()
		}
	}
}

// SweepOnce runs every job due at now. Errors are recorded against the job
// rather than returned so one bad job cannot starve the rest.
func (s *Sweeper) SweepOnce(ctx context.Context, now time.Time) {
	due, err := s.st.DueCronJobs(ctx, now.Unix(), maxPerSweep)
	if err != nil {
		return
	}
	sem := make(chan struct{}, sweepConcurrency)
	var wg sync.WaitGroup
	for _, j := range due {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			// Unstarted jobs keep their next_run_at, so the next sweep re-selects them.
			wg.Wait()
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s.runJob(ctx, j, now)
		}()
	}
	// Stay synchronous: Run relies on the return to know the sweep drained.
	wg.Wait()
}

func (s *Sweeper) runJob(ctx context.Context, j store.CronJob, now time.Time) {
	sched, err := Parse(j.Schedule)
	if err != nil {
		// Park it; leaving next_run_at in the past would re-select it every tick.
		s.park(ctx, j, fmt.Sprintf("schedule %q is not valid: %v", j.Schedule, err))
		return
	}

	// Compute the next fire from the scheduled time, not now, or a late sweep
	// drifts the job a little every day. Read it in the job's zone, since
	// time.Unix returns local time.
	base := now.In(Location(j.Timezone))
	if j.NextRunAt != 0 {
		base = time.Unix(j.NextRunAt, 0).In(Location(j.Timezone))
	}
	next := sched.Next(base)
	// An unreachable schedule (e.g. Feb 30) parks rather than becoming permanently due.
	nextUnix := int64(0)
	if !next.IsZero() {
		nextUnix = next.Unix()
	}

	// The claim is a conditional UPDATE on next_run_at, so exactly one replica wins.
	won, err := s.st.ClaimCronJob(ctx, j.ID, j.NextRunAt, nextUnix)
	if err != nil || !won {
		return
	}
	if s.onClaim != nil {
		s.onClaim(j.ID, nextUnix)
	}

	// Kind is checked before AgentMode: a backup job's Command is the word
	// "backup", not something to run anywhere.
	if j.Kind == store.CronKindBackup {
		s.runBackupJob(ctx, j, now)
		return
	}
	if j.AgentMode != "" {
		s.runAgentJob(ctx, j, now)
		return
	}

	out, code, err := s.run.RunInSandbox(ctx, j.OrgID, j.SandboxID, j.Command)
	switch {
	case err != nil:
		// Could not reach the machine, distinct from the command failing.
		if rErr := s.st.RecordCronRun(ctx, j.ID, "error", err.Error(), now.Unix()); rErr != nil {
			slog.ErrorContext(ctx, "could not record the cron run outcome",
				"component", "cron", "job", j.ID, "outcome", "error", "err", rErr)
		}
	case code != 0:
		if rErr := s.st.RecordCronRun(ctx, j.ID, "failed", out, now.Unix()); rErr != nil {
			slog.ErrorContext(ctx, "could not record the cron run outcome",
				"component", "cron", "job", j.ID, "outcome", "failed", "err", rErr)
		}
	default:
		if rErr := s.st.RecordCronRun(ctx, j.ID, "ok", out, now.Unix()); rErr != nil {
			slog.ErrorContext(ctx, "could not record the cron run outcome",
				"component", "cron", "job", j.ID, "outcome", "ok", "err", rErr)
		}
	}
}

// runBackupJob captures the sandbox. The row it creates is the record of what
// happened; the job's last_output carries the backup id so an operator reading
// the schedule can find it.
func (s *Sweeper) runBackupJob(ctx context.Context, j store.CronJob, now time.Time) {
	br, ok := s.run.(BackupRunner)
	if !ok {
		if rErr := s.st.RecordCronRun(ctx, j.ID, "error",
			"this deployment cannot take scheduled backups", now.Unix()); rErr != nil {
			slog.ErrorContext(ctx, "could not record the cron run outcome",
				"component", "cron", "job", j.ID, "outcome", "error", "err", rErr)
		}
		return
	}
	id, err := br.RunBackup(ctx, j.OrgID, j.SandboxID)
	status, out := "ok", "backup "+id+" started"
	if err != nil {
		status, out = "error", err.Error()
	}
	if rErr := s.st.RecordCronRun(ctx, j.ID, status, out, now.Unix()); rErr != nil {
		slog.ErrorContext(ctx, "could not record the cron run outcome",
			"component", "cron", "job", j.ID, "outcome", status, "err", rErr)
	}
}

// runAgentJob submits the job's prompt as a turn.
func (s *Sweeper) runAgentJob(ctx context.Context, j store.CronJob, now time.Time) {
	ar, ok := s.run.(AgentRunner)
	if !ok {
		// Not run on the shell path: j.Command is an English prompt.
		if rErr := s.st.RecordCronRun(ctx, j.ID, "error",
			"this scheduler cannot run agent jobs, so the prompt was not executed", now.Unix()); rErr != nil {
			slog.ErrorContext(ctx, "could not record the cron run outcome",
				"component", "cron", "job", j.ID, "outcome", "error", "err", rErr)
		}
		return
	}
	out, err := ar.RunAgentTurn(ctx, j.OrgID, j.SandboxID, j.Command, j.AgentMode)
	if err != nil {
		if rErr := s.st.RecordCronRun(ctx, j.ID, "error", err.Error(), now.Unix()); rErr != nil {
			slog.ErrorContext(ctx, "could not record the cron run outcome",
				"component", "cron", "job", j.ID, "outcome", "error", "err", rErr)
		}
		return
	}
	// A turn has no exit code; it either answered or failed above.
	if rErr := s.st.RecordCronRun(ctx, j.ID, "ok", out, now.Unix()); rErr != nil {
		slog.ErrorContext(ctx, "could not record the cron run outcome",
			"component", "cron", "job", j.ID, "outcome", "ok", "err", rErr)
	}
}

// park stops a job from being selected again (next_run_at 0) and records why.
func (s *Sweeper) park(ctx context.Context, j store.CronJob, reason string) {
	if won, err := s.st.ClaimCronJob(ctx, j.ID, j.NextRunAt, 0); err != nil || !won {
		return
	}
	if rErr := s.st.RecordCronRun(ctx, j.ID, "error", reason, time.Now().Unix()); rErr != nil {
		slog.ErrorContext(ctx, "could not record the cron run outcome",
			"component", "cron", "job", j.ID, "outcome", "error", "err", rErr)
	}
}
