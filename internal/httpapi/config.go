package httpapi

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"strings"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox"
	"github.com/pilldrops-oss/pilldrops/internal/store"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// Post-launch config is applied per exec, so a change lands on the next
// command without restarting the sandbox.

// secretish decides whether a value is masked on read-back. Deliberately generous.
func secretish(name string) bool {
	n := strings.ToUpper(name)
	for _, marker := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "AUTH", "DSN", "DATABASE_URL"} {
		if strings.Contains(n, marker) {
			return true
		}
	}
	return false
}

// handleConfigSet updates a workstation's environment, model or base URL.
//
//	POST /v1/sandboxes/{id}/config  {"env":{...},"model":"...","base_url":"..."}
func (s *Server) handleConfigSet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	sb, ok := s.d.Get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	var body struct {
		Env     map[string]string `json:"env"`
		Model   string            `json:"model"`
		BaseURL string            `json:"base_url"`
		Backups *BackupSchedule   `json:"backups"`
	}
	if !decodeBody(r, &body) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// Before the env merge: a refused schedule must change nothing at all,
	// not leave the environment written and the schedule rejected.
	if body.Backups != nil {
		if err := s.applyBackupSchedule(r.Context(), orgID(r), id, *body.Backups); err != nil {
			backupScheduleErr(w, err)
			return
		}
		s.auditDetail(r, "sandbox.backup.schedule", id, map[string]any{
			"period": body.Backups.Period, "cron": body.Backups.Cron,
			"retention": body.Backups.Retention,
		})
	}
	for name := range body.Env {
		if err := sandbox.ValidEnvName(name); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// Model and base URL map to per-agent env names from the manifest, so the
	// UI never needs to know them.
	env := body.Env
	if env == nil {
		env = map[string]string{}
	}
	if tmpl, ok := templates.Resolve(sb.Template); ok && tmpl.Agent != nil {
		if body.Model != "" && tmpl.Agent.ModelEnv != "" {
			env[tmpl.Agent.ModelEnv] = body.Model
		}
		if body.BaseURL != "" && tmpl.Agent.BaseURLEnv != "" {
			env[tmpl.Agent.BaseURLEnv] = body.BaseURL
		}
	}

	cfg, err := s.st.MergeSandboxConfig(r.Context(), id, orgID(r), env, body.Model, body.BaseURL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not save the configuration")
		return
	}
	// A runtime with rt.EnvSetter (Firecracker) gets the change into shells
	// too; Docker's container env is immutable and keeps the per-exec merge.
	if len(env) > 0 {
		if err := s.d.SetEnv(r.Context(), id, env); err != nil && !errors.Is(err, rt.ErrUnsupported) {
			writeErr(w, statusForErr(err), "saved, but the sandbox did not take the new environment: "+err.Error())
			return
		}
	}
	// Audit names only; values may be secrets.
	s.auditDetail(r, "sandbox.config", id, map[string]any{
		"env_keys": sortedKeys(body.Env), "model": body.Model, "base_url_set": body.BaseURL != "",
	})
	writeJSON(w, http.StatusOK, configView(cfg))
}

// handleConfigGet reports a workstation's config. Secret values never come
// back, only the name and whether it is set.
//
//	GET /v1/sandboxes/{id}/config
func (s *Server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	view := configView(s.st.SandboxConfigFor(r.Context(), id))
	if sched := s.backupScheduleView(r.Context(), orgID(r), id); sched != nil {
		view["backups"] = sched
	}
	writeJSON(w, http.StatusOK, view)
}

// envEntry is one variable as the UI sees it; Value is omitted for secrets.
type envEntry struct {
	Name   string `json:"name"`
	Value  string `json:"value,omitempty"`
	Secret bool   `json:"secret"`
	Set    bool   `json:"set"`
}

func configView(c store.SandboxConfig) map[string]any {
	entries := make([]envEntry, 0, len(c.Env))
	for _, name := range sortedKeys(c.Env) {
		e := envEntry{Name: name, Secret: secretish(name), Set: true}
		if !e.Secret {
			e.Value = c.Env[name]
		}
		entries = append(entries, e)
	}
	return map[string]any{"env": entries, "model": c.Model, "base_url": c.BaseURL}
}

// configEnvFor returns the stored config as exec environment. Every exec path
// (terminal, chat, cron) goes through this.
func (s *Server) configEnvFor(ctx context.Context, id string) map[string]string {
	return s.st.SandboxConfigFor(ctx, id).Env
}

// mergeExecEnv folds stored config under the per-request env; the request wins.
func mergeExecEnv(cfg, req map[string]string) map[string]string {
	if len(cfg) == 0 {
		return req
	}
	out := maps.Clone(cfg)
	maps.Copy(out, req)
	return out
}
