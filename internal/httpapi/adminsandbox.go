package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// maxAdminLogBytes matches the retained-history log cap.
const maxAdminLogBytes = 200 * 1024

// adminActivityRow is one timeline entry; invalid or empty detail becomes null.
type adminActivityRow struct {
	At     int64           `json:"at"`
	OrgID  string          `json:"org_id"`
	KeyID  string          `json:"key_id"`
	Action string          `json:"action"`
	Detail json.RawMessage `json:"detail,omitempty"`
}

// adminSandboxResponse is the drill-down payload. State is the lifecycle
// (active, destroyed, unknown); Record is nil when unknown.
type adminSandboxResponse struct {
	Record   any                `json:"record"`
	State    string             `json:"state"`
	Launch   json.RawMessage    `json:"launch,omitempty"`
	Activity []adminActivityRow `json:"activity"`
	Logs     string             `json:"logs"`
	LogsNote string             `json:"logs_note,omitempty"`
}

// handleAdminSandbox returns the cross-org operator drill-down for one sandbox.
func (s *Server) handleAdminSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	entries, err := s.st.ListAuditByTarget(r.Context(), id, 0)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	activity := make([]adminActivityRow, 0, len(entries))
	var launch json.RawMessage
	for _, e := range entries {
		var d json.RawMessage
		if e.Detail != "" && json.Valid([]byte(e.Detail)) {
			d = json.RawMessage(e.Detail)
		}
		// "{}" is auditDetail's overflow marker, not a launch config.
		if launch == nil && e.Action == "sandbox.create" && d != nil && string(d) != "{}" {
			launch = d
		}
		activity = append(activity, adminActivityRow{At: e.At, OrgID: e.OrgID, KeyID: e.KeyID, Action: e.Action, Detail: d})
	}

	// History first, then the live row, then a timeline-only "unknown" response
	// when only audit rows survived pruning.
	rec, ok, err := s.st.GetHistory(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ok {
		// Serve the log once, at top level.
		logs := rec.Logs
		rec.Logs = ""
		note := ""
		if logs == "" {
			note = "no logs were retained"
		}
		writeJSON(w, http.StatusOK, adminSandboxResponse{
			Record: rec, State: "destroyed", Launch: launch, Activity: activity,
			Logs: logs, LogsNote: note,
		})
		return
	}

	row, ok, err := s.st.GetActiveSandboxRow(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ok {
		logs, note := s.liveLogTail(r.Context(), id)
		writeJSON(w, http.StatusOK, adminSandboxResponse{
			Record: row, State: "active", Launch: launch, Activity: activity,
			Logs: logs, LogsNote: note,
		})
		return
	}

	if len(activity) > 0 {
		writeJSON(w, http.StatusOK, adminSandboxResponse{
			Record: nil, State: "unknown", Launch: launch, Activity: activity,
			Logs: "", LogsNote: "history record expired — timeline reconstructed from the audit log",
		})
		return
	}
	writeErr(w, http.StatusNotFound, "unknown sandbox")
}

// liveLogTail returns the local app log tail, or a note explaining why it is empty.
func (s *Server) liveLogTail(ctx context.Context, id string) (string, string) {
	fs, err := s.d.FS(id)
	if err != nil {
		return "", "logs unavailable — sandbox is paused or not local to the serving node"
	}
	data, err := fs.Read(ctx, rt.AppLogPath)
	if err != nil || len(data) == 0 {
		return "", "no app log yet — logs capture the server started via start"
	}
	if len(data) > maxAdminLogBytes {
		data = data[len(data)-maxAdminLogBytes:]
	}
	return string(data), ""
}
