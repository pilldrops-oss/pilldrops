package agentd

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Approval endpoints live here because the parked call is a goroutine blocked in
// this process. agentd sees only a bearer token, so the rule that automation must
// not approve its own calls is enforced on the node, where identity is known.

// handleApprovalsList returns the parked approvals, oldest first.
func (s *Server) handleApprovalsList(w http.ResponseWriter, r *http.Request) {
	if s.Gate == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error": "this agent has no approval gate, so Guided mode is not available",
			"code":  "gate_unavailable",
		})
		return
	}
	pending := s.Gate.Pending()
	if pending == nil {
		pending = []Approval{}
	}
	// Resolved approvals ride along so the node can persist outcomes that happened
	// entirely inside the sandbox (policy allows, timeouts).
	resolved := s.Gate.Resolved()
	if resolved == nil {
		resolved = []Approval{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"approvals": pending,
		"resolved":  resolved,
		"remember_scopes": []string{
			string(RememberOnce), string(RememberSession), string(RememberSandbox),
		},
		"timeout_seconds": int(s.Gate.timeout().Seconds()),
		// Stated so a reviewer does not assume silence means "it waits".
		"on_timeout": "deny",
	})
}

// handleApprovalDecide serves POST /approvals/{id}.
func (s *Server) handleApprovalDecide(w http.ResponseWriter, r *http.Request) {
	if s.Gate == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error": "this agent has no approval gate", "code": "gate_unavailable",
		})
		return
	}
	var req struct {
		Decision string `json:"decision"` // "allow" | "deny"
		Remember string `json:"remember"` // once | session | sandbox
		Actor    string `json:"actor"`    // who decided, for the record
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json body"})
		return
	}

	// The wire word is "deny", not "ask", so a client cannot read it as "keep waiting".
	var d Decision
	switch req.Decision {
	case "allow":
		d = DecisionAllow
	case "deny":
		d = DecisionAsk
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": `decision must be "allow" or "deny"`,
		})
		return
	}

	remember := RememberScope(req.Remember)
	if remember == "" {
		// Default to the narrowest scope.
		remember = RememberOnce
	}
	if !ValidRememberScope(remember) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "unknown remember scope " + req.Remember +
				"; there is deliberately no organisation-wide scope",
		})
		return
	}

	err := s.Gate.Decide(r.PathValue("id"), d, remember, req.Actor)
	switch {
	case errors.Is(err, ErrNoSuchApproval):
		// 409, not 404: the id was probably real and already answered or timed out.
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "this approval is no longer pending — it was already answered, " +
				"or it timed out and was denied",
			"code": "approval_not_pending",
		})
		return
	case err != nil:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"decided": r.PathValue("id"), "decision": req.Decision})
}

// handlePolicyGet returns the active policy and standing grants.
func (s *Server) handlePolicyGet(w http.ResponseWriter, r *http.Request) {
	if s.Gate == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error": "this agent has no approval gate", "code": "gate_unavailable",
		})
		return
	}
	rules := make([]map[string]any, 0, len(s.Gate.Policy.Rules))
	for _, r := range s.Gate.Policy.Rules {
		e := map[string]any{"tool": r.Tool, "decision": string(DecisionAllow), "why": r.Why}
		if len(r.Argv) > 0 {
			e["argv"] = r.Argv
		}
		if len(r.Forbid) > 0 {
			e["forbid"] = r.Forbid
		}
		if r.PathPrefix != "" {
			e["path_prefix"] = r.PathPrefix
		}
		rules = append(rules, e)
	}
	out := map[string]any{
		"rules": rules,
		// Explicit, so the list is safe to read as complete: anything absent asks.
		"default": string(DecisionAsk),
		"note": "Allowlist: any call not matched by a rule waits for a human. There are " +
			"no deny rules — a gap in this list produces a prompt, not an execution.",
	}
	// Both scopes are reported, each labelled, since session grants allow calls
	// too until restart.
	grants := make([]map[string]string, 0)
	persisted := map[string]bool{}
	if s.Gate.Remembered != nil {
		for _, sig := range s.Gate.Remembered.List() {
			grants = append(grants, map[string]string{
				"signature": sig, "scope": string(RememberSandbox),
			})
			persisted[sig] = true
		}
	}
	for _, sig := range s.Gate.SessionGrants() {
		if persisted[sig] {
			continue // a sandbox grant is also in the session map
		}
		grants = append(grants, map[string]string{
			"signature": sig, "scope": string(RememberSession),
		})
	}
	out["remembered"] = grants
	writeJSON(w, http.StatusOK, out)
}

// handlePolicyForget serves DELETE /policy/remembered.
func (s *Server) handlePolicyForget(w http.ResponseWriter, r *http.Request) {
	if s.Gate == nil || s.Gate.Remembered == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error": "no standing approvals are stored for this agent",
			"code":  "gate_unavailable",
		})
		return
	}
	var req struct {
		Signature string `json:"signature"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || req.Signature == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "signature required"})
		return
	}
	if err := s.Gate.Remembered.Forget(req.Signature); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// Drop the session copy too, or the grant keeps working until restart.
	s.Gate.forgetSession(req.Signature)
	writeJSON(w, http.StatusOK, map[string]any{"forgot": req.Signature})
}
