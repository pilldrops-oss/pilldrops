//go:build !core

package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cron"
	"github.com/pilldrops-oss/pilldrops/internal/extras/agent/agentd"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// Scheduled runs, API half. internal/cron owns the schedule math and the sweeper.

// maxCronCommand keeps the jobs table from becoming blob storage.
const maxCronCommand = 8192

// handleCronCreate schedules a command against a workstation.
//
//	POST /v1/cron  {"sandbox_id","schedule","command","name","timezone"}
func (s *Server) handleCronCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SandboxID string `json:"sandbox_id"`
		Schedule  string `json:"schedule"`
		Command   string `json:"command"`
		Name      string `json:"name"`
		Timezone  string `json:"timezone"`
		// AgentMode "guided" or "autonomous" runs command as an agent prompt; empty runs it in a shell.
		AgentMode string `json:"agent_mode"`
	}
	if !decodeBody(r, &body) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// Ownership before validation, so error codes cannot be used to probe sandbox ids.
	if !s.owns(r, body.SandboxID) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	if strings.TrimSpace(body.Command) == "" {
		writeErr(w, http.StatusBadRequest, "command is required")
		return
	}
	if len(body.Command) > maxCronCommand {
		writeErr(w, http.StatusBadRequest, "command is too long")
		return
	}
	// An unknown mode is refused, not defaulted to shell: that would execute prose.
	switch body.AgentMode {
	case "", "guided", "autonomous":
	default:
		writeErr(w, http.StatusBadRequest,
			"agent_mode must be omitted (a shell command), \"guided\" or \"autonomous\"")
		return
	}
	if body.AgentMode != "" {
		if _, ok := s.agentTemplateOf(body.SandboxID); !ok {
			writeErr(w, http.StatusBadRequest,
				"agent_mode needs an AI-agent workstation; this template has no agent, so "+
					"schedule a shell command instead")
			return
		}
	}
	// Validate at write time; a stored bad schedule silently never fires.
	sched, err := cron.Parse(body.Schedule)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "schedule: "+err.Error())
		return
	}
	if !cron.ValidTimezone(body.Timezone) {
		writeErr(w, http.StatusBadRequest, "timezone: unknown zone "+body.Timezone+" — use an IANA name like Europe/London")
		return
	}
	loc := cron.Location(body.Timezone)
	next := sched.Next(time.Now().In(loc))
	if next.IsZero() {
		writeErr(w, http.StatusBadRequest, "schedule: that date never occurs")
		return
	}

	job, err := s.st.CreateCronJob(r.Context(), store.CronJob{
		OrgID: orgID(r), SandboxID: body.SandboxID, Name: body.Name,
		Schedule: body.Schedule, Timezone: body.Timezone, Command: body.Command,
		AgentMode: body.AgentMode,
	}, next.Unix())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not create the job")
		return
	}
	s.auditDetail(r, "cron.create", body.SandboxID, map[string]any{
		"job": job.ID, "schedule": body.Schedule, "timezone": body.Timezone,
	})
	writeJSON(w, http.StatusCreated, job)
}

// handleCronList returns the caller's jobs.
//
//	GET /v1/cron
func (s *Server) handleCronList(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.st.ListCronJobs(r.Context(), orgID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not list jobs")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

// handleCronDelete removes a job.
//
//	DELETE /v1/cron/{id}
func (s *Server) handleCronDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ok, err := s.st.DeleteCronJob(r.Context(), orgID(r), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not delete the job")
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	s.auditDetail(r, "cron.delete", id, nil)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// handleCronToggle enables or disables a job without losing its schedule.
//
//	POST /v1/cron/{id}/toggle  {"enabled":false}
func (s *Server) handleCronToggle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if !decodeBody(r, &body) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	ok, err := s.st.SetCronEnabled(r.Context(), orgID(r), id, body.Enabled)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not update the job")
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	s.auditDetail(r, "cron.toggle", id, map[string]any{"enabled": body.Enabled})
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "enabled": body.Enabled})
}

// handleCronRunNow fires a job immediately without disturbing its schedule.
//
//	POST /v1/cron/{id}/run
func (s *Server) handleCronRunNow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok := s.st.CronJobByID(r.Context(), orgID(r), id)
	if !ok {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	out, code, err := s.cronRunner().RunInSandbox(r.Context(), job.OrgID, job.SandboxID, job.Command)
	status := "ok"
	switch {
	case err != nil:
		status, out = "error", err.Error()
	case code != 0:
		status = "failed"
	}
	// Recorded like a scheduled run so the dashboard shows one history.
	_ = s.st.RecordCronRun(r.Context(), job.ID, status, out, time.Now().Unix())
	s.auditDetail(r, "cron.run_now", job.SandboxID, map[string]any{"job": job.ID, "status": status})
	writeJSON(w, http.StatusOK, map[string]any{
		"id": job.ID, "status": status, "exit_code": code, "output": out,
	})
}

// daemonCronRunner runs a job's command inside its sandbox.
type daemonCronRunner struct{ s *Server }

func (s *Server) cronRunner() cron.Runner { return daemonCronRunner{s} }

// RunAgentTurn submits a scheduled prompt as an agent turn. A guided job that
// parks for approval does not fail; the run records what it is waiting for.
func (d daemonCronRunner) RunAgentTurn(ctx context.Context, orgIDStr, sandboxID, prompt, mode string) (string, error) {
	c, err := d.s.agentClient(sandboxID)
	if err != nil {
		return "", err
	}
	turn, err := c.Submit(ctx, agentd.SubmitRequest{
		Message: prompt, Mode: mode, Thread: "latest", Source: "cron",
	})
	if err != nil {
		return "", err
	}
	// Scheduled tasks are expected to be slow; the turn survives this wait either way.
	ctx, cancel := context.WithTimeout(ctx, cronExecTimeout)
	defer cancel()
	return d.s.waitForChatTurn(ctx, c, turn.ID)
}

// RunInSandbox executes cmd in the sandbox and returns its combined output.
// Uses cronExecTimeout, not the 30s interactive default, since scheduled tasks take minutes.
func (d daemonCronRunner) RunInSandbox(ctx context.Context, orgIDStr, sandboxID, cmd string) (string, int, error) {
	res, err := d.s.d.Exec(ctx, sandboxID, rt.ExecSpec{
		Cmd: cmd, Env: d.s.configEnvFor(ctx, sandboxID), Timeout: cronExecTimeout})
	if err != nil {
		return "", 0, err
	}
	out := res.Stdout
	if res.Stderr != "" {
		out += res.Stderr
	}
	return out, res.ExitCode, nil
}

// Asserted, not assumed: the sweeper reaches these through type assertions, so
// a signature that drifts does not fail to compile — it silently becomes "this
// deployment cannot take scheduled backups", which reads like a configuration
// problem and is a build problem.
var (
	_ cron.Runner       = daemonCronRunner{}
	_ cron.AgentRunner  = daemonCronRunner{}
	_ cron.BackupRunner = daemonCronRunner{}
)

// RunBackup implements cron.BackupRunner: a scheduled capture. It returns as
// soon as the sandbox is running again, so a sweep slot is held for the pause
// and not for the upload.
func (d daemonCronRunner) RunBackup(ctx context.Context, _, sandboxID string) (string, error) {
	started, err := d.s.d.Backup(ctx, sandboxID)
	if err != nil {
		return "", err
	}
	return started.ID, nil
}

// cronExecTimeout bounds one scheduled run so a hung command cannot hold a sweep slot.
const cronExecTimeout = 10 * time.Minute
