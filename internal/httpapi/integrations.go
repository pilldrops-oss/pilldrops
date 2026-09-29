//go:build !core

package httpapi

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"

	"github.com/pilldrops-oss/pilldrops/internal/extras/mcp/catalog"
)

// Integrations switch MCP servers on for a workstation by writing the agent's
// config into the machine.

// handleIntegrationsCatalog lists what can be switched on.
//
//	GET /v1/integrations
func (s *Server) handleIntegrationsCatalog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"servers": catalog.Catalog(),
		"agents":  catalog.Agents(),
	})
}

// handleIntegrationsSet enables a set of servers on a workstation. The whole
// set replaces the previous one, so "off" means gone.
//
//	POST /v1/sandboxes/{id}/integrations  {"servers":["github"],"env":{"GITHUB_TOKEN":"..."}}
func (s *Server) handleIntegrationsSet(w http.ResponseWriter, r *http.Request) {
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
		Servers []string          `json:"servers"`
		Env     map[string]string `json:"env"`
	}
	if !decodeBody(r, &body) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Claude's config is shared with its install state, so that one is merged.
	path, mergeInto := catalog.Target(sb.Template)
	var content string
	var err error
	if mergeInto {
		content, err = s.mergeAgentConfig(r, id, path, sb.Template, body.Servers, body.Env)
	} else {
		path, content, err = catalog.Render(sb.Template, body.Servers, body.Env)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := s.writeAgentConfig(r, id, path, content); err != nil {
		writeErr(w, statusForErr(err), "could not write the agent config: "+err.Error())
		return
	}

	// Record the selection, not the rendered file; the machine can be rebuilt.
	if err := s.st.SetSandboxIntegrations(r.Context(), id, orgID(r), body.Servers); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not record the selection")
		return
	}
	// Audit env keys, never values.
	keys := make([]string, 0, len(body.Env))
	for k := range body.Env {
		keys = append(keys, k)
	}
	s.auditDetail(r, "sandbox.integrations", id, map[string]any{
		"servers": body.Servers, "env_keys": keys, "path": path,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"servers": body.Servers,
		"path":    path,
		"note":    "restart the agent (or send a new chat message) to pick up the change",
	})
}

// handleIntegrationsGet reports which servers are enabled.
//
//	GET /v1/sandboxes/{id}/integrations
func (s *Server) handleIntegrationsGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	ids, err := s.st.SandboxIntegrations(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not read the selection")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": ids})
}

// dirOf returns the directory part of a config path, or "." when it is a bare
// filename in the working directory.
func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "."
}

// mergeAgentConfig folds the selection into the agent's existing config, which
// also holds state a plain write would reset.
func (s *Server) mergeAgentConfig(r *http.Request, id, path, agent string, ids []string, env map[string]string) (string, error) {
	// A missing file is the normal first run.
	return catalog.Merge(agent, s.readAgentConfig(r, id, path), ids, env)
}

// writeAgentConfig writes the rendered config. A "~/" path lies outside the
// filesystem API's workdir root, so it goes through the shell; the content is
// base64'd so credentials never become shell syntax.
func (s *Server) writeAgentConfig(r *http.Request, id, path, content string) error {
	if !strings.HasPrefix(path, "~/") {
		fs, err := s.d.FS(id)
		if err != nil {
			return err
		}
		if d := dirOf(path); d != "." {
			_ = fs.Mkdir(r.Context(), d)
		}
		return fs.Write(r.Context(), path, []byte(content))
	}
	rel := strings.TrimPrefix(path, "~/")
	b64 := base64.StdEncoding.EncodeToString([]byte(content))
	cmd := fmt.Sprintf(`mkdir -p "$HOME/%s" && printf %%s %s | base64 -d > "$HOME/%s"`,
		dirOf(rel), b64, rel)
	if dirOf(rel) == "." {
		cmd = fmt.Sprintf(`printf %%s %s | base64 -d > "$HOME/%s"`, b64, rel)
	}
	res, err := s.d.Exec(r.Context(), id, rt.ExecSpec{Cmd: cmd, Timeout: 30 * time.Second})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("writing %s failed: %s", path, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// readAgentConfig returns the agent's current config, empty when absent.
func (s *Server) readAgentConfig(r *http.Request, id, path string) []byte {
	if !strings.HasPrefix(path, "~/") {
		fs, err := s.d.FS(id)
		if err != nil {
			return nil
		}
		b, _ := fs.Read(r.Context(), path)
		return b
	}
	rel := strings.TrimPrefix(path, "~/")
	res, err := s.d.Exec(r.Context(), id,
		rt.ExecSpec{Cmd: fmt.Sprintf(`cat "$HOME/%s" 2>/dev/null || true`, rel), Timeout: 20 * time.Second})
	if err != nil {
		return nil
	}
	return []byte(res.Stdout)
}
