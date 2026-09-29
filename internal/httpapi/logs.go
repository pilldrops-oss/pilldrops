package httpapi

import (
	"net/http"
	"strings"

	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// defaultLogLines is the tail length returned when ?limit=N is absent.
const defaultLogLines = 200

// handleSandboxLogs serves the tail of the start command's captured output.
// A paused sandbox or one with no app started gets an empty list, not an error, so UIs can poll.
func (s *Server) handleSandboxLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	s.serveSandboxLogs(w, r, id)
}

// serveSandboxLogs tails rt.AppLogPath. The caller must check ownership.
func (s *Server) serveSandboxLogs(w http.ResponseWriter, r *http.Request, id string) {
	fs, err := s.d.FS(id)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"lines": []string{}, "note": "sandbox is paused — logs return when it wakes",
		})
		return
	}
	// The log only exists once a server has been started; say so rather than return a bare empty list.
	noAppLogNote := "no app log yet — it captures the server started via `--start`/the start API; " +
		"ad-hoc `exec` output is returned by exec itself, not here"
	data, err := fs.Read(r.Context(), rt.AppLogPath)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"lines": []string{}, "note": noAppLogNote})
		return
	}

	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = []string{}
	}
	if len(lines) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"lines": []string{}, "note": noAppLogNote})
		return
	}
	limit := httpx.QueryInt(r, "limit", defaultLogLines)
	truncated := false
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
		truncated = true
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines, "truncated": truncated})
}
