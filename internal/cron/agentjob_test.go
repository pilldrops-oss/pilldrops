package cron

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// recordingStore is the narrow Store the sweeper needs, plus a record of what it wrote.
type recordingStore struct {
	due     []store.CronJob
	claimed bool
	status  string
	output  string
}

func (r *recordingStore) DueCronJobs(ctx context.Context, now int64, limit int) ([]store.CronJob, error) {
	return r.due, nil
}
func (r *recordingStore) ClaimCronJob(ctx context.Context, id string, expected, next int64) (bool, error) {
	r.claimed = true
	return true, nil
}
func (r *recordingStore) RecordCronRun(ctx context.Context, id, status, output string, ranAt int64) error {
	r.status, r.output = status, output
	return nil
}

// shellOnlyRunner implements Runner but not AgentRunner.
type shellOnlyRunner struct {
	ran []string
}

func (s *shellOnlyRunner) RunInSandbox(ctx context.Context, org, sb, cmd string) (string, int, error) {
	s.ran = append(s.ran, cmd)
	return "shell output", 0, nil
}

// bothRunner can do shell and turns.
type bothRunner struct {
	shellOnlyRunner
	prompt string
	mode   string
	reply  string
	err    error
}

func (b *bothRunner) RunAgentTurn(ctx context.Context, org, sb, prompt, mode string) (string, error) {
	b.prompt, b.mode = prompt, mode
	return b.reply, b.err
}

func agentJob(mode string) store.CronJob {
	return store.CronJob{
		ID: "cj1", OrgID: "org1", SandboxID: "sb1", Schedule: "* * * * *",
		Command: "check the overnight CI and open a PR if it broke", AgentMode: mode,
		Enabled: true, NextRunAt: 1,
	}
}

// A scheduled job on the exec path would skip the spend cap, journal and approval gate.
func TestAgentJobBecomesATurnNotAnExec(t *testing.T) {
	st := &recordingStore{due: []store.CronJob{agentJob("guided")}}
	run := &bothRunner{reply: "CI was green"}
	NewSweeper(st, run, nil).SweepOnce(context.Background(), time.Unix(100, 0))

	if run.prompt != "check the overnight CI and open a PR if it broke" {
		t.Errorf("prompt = %q", run.prompt)
	}
	if run.mode != "guided" {
		t.Errorf("mode = %q, want the job's declared mode", run.mode)
	}
	if len(run.ran) != 0 {
		t.Errorf("the prompt was ALSO handed to a shell: %v", run.ran)
	}
	if st.status != "ok" || st.output != "CI was green" {
		t.Errorf("recorded status=%q output=%q", st.status, st.output)
	}
}

// Jobs with an empty agent_mode keep running on the shell path.
func TestShellJobIsUnchanged(t *testing.T) {
	j := agentJob("")
	j.Command = "pg_dump mydb | gzip > /backups/nightly.sql.gz"
	st := &recordingStore{due: []store.CronJob{j}}
	run := &bothRunner{reply: "should not be used"}
	NewSweeper(st, run, nil).SweepOnce(context.Background(), time.Unix(100, 0))

	if run.prompt != "" {
		t.Errorf("a shell command was submitted to the agent as a prompt: %q", run.prompt)
	}
	if len(run.ran) != 1 || run.ran[0] != j.Command {
		t.Errorf("shell path did not run the command: %v", run.ran)
	}
}

// An agent job's Command is an English prompt; a shell could execute a fragment of it.
func TestAgentJobOnARunnerThatCannotDoTurnsIsNotShelledOut(t *testing.T) {
	st := &recordingStore{due: []store.CronJob{agentJob("autonomous")}}
	run := &shellOnlyRunner{}
	NewSweeper(st, run, nil).SweepOnce(context.Background(), time.Unix(100, 0))

	if len(run.ran) != 0 {
		t.Fatalf("an English prompt was executed as a shell command: %v", run.ran)
	}
	if st.status != "error" {
		t.Errorf("status = %q, want error so the user can see it never ran", st.status)
	}
	if !strings.Contains(st.output, "not executed") {
		t.Errorf("the recorded reason does not say the prompt did not run: %q", st.output)
	}
}

func TestAgentJobFailureIsRecordedAsError(t *testing.T) {
	st := &recordingStore{due: []store.CronJob{agentJob("guided")}}
	run := &bothRunner{err: context.DeadlineExceeded}
	NewSweeper(st, run, nil).SweepOnce(context.Background(), time.Unix(100, 0))
	if st.status != "error" {
		t.Errorf("status = %q, want error", st.status)
	}
}

// The claim stops two replicas submitting the same turn.
func TestAgentJobStillClaimsBeforeRunning(t *testing.T) {
	st := &recordingStore{due: []store.CronJob{agentJob("guided")}}
	run := &bothRunner{reply: "ok"}
	NewSweeper(st, run, nil).SweepOnce(context.Background(), time.Unix(100, 0))
	if !st.claimed {
		t.Error("ran without claiming, so every replica would submit the same turn")
	}
}
