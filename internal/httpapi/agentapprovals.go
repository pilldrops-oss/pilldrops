//go:build !core

package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/aat"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// The approval surface proxies agentd and adds the one check only this layer
// can make: who is allowed to approve, since agentd never sees the caller's credential.

// handleAgentApprovals lists parked approvals: GET /v1/sandboxes/{id}/agent/approvals
func (s *Server) handleAgentApprovals(w http.ResponseWriter, r *http.Request) {
	id, tmpl, ok := s.agentGate(r)
	if !ok {
		writeErrCode(w, http.StatusNotFound, codeAgentNotSupported, "sandbox not found")
		return
	}
	c, err := s.agentClient(id)
	if err != nil {
		writeAgentErr(w, err)
		return
	}
	out, err := c.Approvals(r.Context())
	if err != nil {
		writeAgentErr(w, err)
		return
	}
	// Persist both lists so the trail outlives the sandbox; resolved entries
	// include policy allows and timeouts nobody here caused.
	s.recordApprovals(r.Context(), id, orgID(r), tmpl.ID, out)

	// Tell the caller whether they may act, so the UI does not show buttons that 403.
	out["can_decide"] = mayApprove(r)
	if !out["can_decide"].(bool) {
		out["cannot_decide_reason"] = approvalRefusalReason(r)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAgentApprovalDecide answers one:
// POST /v1/sandboxes/{id}/agent/approvals/{approvalID}
func (s *Server) handleAgentApprovalDecide(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.agentGate(r)
	if !ok {
		writeErrCode(w, http.StatusNotFound, codeAgentNotSupported, "sandbox not found")
		return
	}
	if !mayApprove(r) {
		writeErrCode(w, http.StatusForbidden, codeApprovalNotPermitted, approvalRefusalReason(r))
		return
	}

	var body struct {
		Decision string `json:"decision"`
		Remember string `json:"remember"`
	}
	if !decodeBody(r, &body) {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return
	}
	switch body.Decision {
	case "allow", "deny":
	default:
		writeErr(w, http.StatusBadRequest, `decision must be "allow" or "deny"`)
		return
	}

	c, err := s.agentClient(id)
	if err != nil {
		writeAgentErr(w, err)
		return
	}
	// The actor comes from the credential, never the request body.
	out, err := c.Decide(r.Context(), r.PathValue("approvalID"), body.Decision, body.Remember, actorOf(r))
	if err != nil {
		writeAgentErr(w, err)
		return
	}
	s.auditDetail(r, "agent.approval."+body.Decision, id, map[string]any{
		"approval_id": r.PathValue("approvalID"), "remember": body.Remember,
	})
	writeJSON(w, http.StatusOK, out)
}

// handleAgentPolicy returns the active policy: GET /v1/sandboxes/{id}/agent/policy
func (s *Server) handleAgentPolicy(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.agentGate(r)
	if !ok {
		writeErrCode(w, http.StatusNotFound, codeAgentNotSupported, "sandbox not found")
		return
	}
	c, err := s.agentClient(id)
	if err != nil {
		writeAgentErr(w, err)
		return
	}
	if r.Method == http.MethodDelete {
		var body struct {
			Signature string `json:"signature"`
		}
		if !decodeBody(r, &body) || body.Signature == "" {
			writeErr(w, http.StatusBadRequest, "signature is required")
			return
		}
		out, err := c.ForgetApproval(r.Context(), body.Signature)
		if err != nil {
			writeAgentErr(w, err)
			return
		}
		s.auditDetail(r, "agent.approval.forget", id, map[string]any{"signature": body.Signature})
		writeJSON(w, http.StatusOK, out)
		return
	}
	out, err := c.Policy(r.Context())
	if err != nil {
		writeAgentErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

const codeApprovalNotPermitted = "approval_not_permitted"

// mayApprove reports whether this caller may answer an approval. An org key
// always may; a delegated token needs agent:approve and must not look like
// automation, since a pipeline approving its own gate defeats Guided mode.
func mayApprove(r *http.Request) bool {
	tok, delegated := agentTokenOf(r)
	if !delegated {
		return true
	}
	if !tok.Scopes.Has(aat.ScopeApprove) {
		return false
	}
	return !looksAutomated(tok.Label)
}

func approvalRefusalReason(r *http.Request) string {
	tok, delegated := agentTokenOf(r)
	if !delegated {
		return ""
	}
	if !tok.Scopes.Has(aat.ScopeApprove) {
		return "this token does not have the agent:approve scope, which answering an " +
			"approval needs"
	}
	if looksAutomated(tok.Label) {
		return "this token is marked as automation, and automation cannot approve its " +
			"own agent's actions — the point of Guided mode is that a person sees them"
	}
	return ""
}

// automationLabels mark a token as a robot. Used only to refuse, never to grant.
var automationLabels = []string{"ci", "cron", "bot", "automation", "pipeline", "scheduled", "webhook"}

func looksAutomated(label string) bool {
	l := strings.ToLower(label)
	for _, w := range automationLabels {
		// Word boundaries, so "circuit" does not match "ci" while "ci-runner" does.
		for _, f := range strings.FieldsFunc(l, func(r rune) bool {
			return r == ' ' || r == '-' || r == '_' || r == '.' || r == ':' || r == '/'
		}) {
			if f == w {
				return true
			}
		}
	}
	return false
}

// recordApprovals persists an agentd approvals response into the durable
// trail. An approval that ages out of agentd's ring before anyone polls is
// never recorded. Failures never reach the caller.
func (s *Server) recordApprovals(ctx context.Context, sandboxID, org, agent string, out map[string]any) {
	perTurn := map[string]int{}
	for _, key := range []string{"approvals", "resolved"} {
		list, _ := out[key].([]any)
		for _, item := range list {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			row := approvalRowFrom(sandboxID, org, m)
			if row.ID == "" {
				continue
			}
			_ = s.st.UpsertAgentApproval(ctx, row)
			// Count only resolved outcomes, so repeated polls do not inflate pending ones.
			if key == "resolved" && row.Decided() {
				s.metrics.incAgentApproval(agent, string(row.State))
				perTurn[row.TurnID]++
			}
		}
	}
	s.noteApprovalFriction(agent, perTurn)
}

// approvalRowFrom converts agentd's JSON into a store row. It reads the map
// rather than a shared struct because agentd is upgraded on its own schedule.
func approvalRowFrom(sandboxID, org string, m map[string]any) store.AgentApproval {
	str := func(k string) string { v, _ := m[k].(string); return v }
	row := store.AgentApproval{
		ID: str("id"), SandboxID: sandboxID, OrgID: org,
		TurnID: str("turn_id"), Tool: str("tool"), ToolUseID: str("tool_use_id"),
		Signature: str("signature"), ArgsDigest: str("args_digest"),
		ArgsPreview: str("args_preview"), DecidedBy: str("decided_by"),
		Remember: str("remember"), Reason: str("reason"),
		State: store.ApprovalPending,
	}
	if t, err := time.Parse(time.RFC3339, str("requested_at")); err == nil {
		row.RequestedAt = t
	}
	if t, err := time.Parse(time.RFC3339, str("decided_at")); err == nil {
		row.DecidedAt = t
	}
	// In agentd's vocabulary, "ask" on a finished approval means refused.
	switch str("decision") {
	case "allow":
		row.State = store.ApprovalAllowed
	case "ask", "deny":
		row.State = store.ApprovalDenied
		// A timeout is recorded distinctly from a refusal.
		if strings.Contains(row.Reason, "no answer within") ||
			strings.Contains(row.Reason, "turn ended before") {
			row.State = store.ApprovalTimedOut
		}
	}
	return row
}

// noteApprovalFriction records how many approvals each turn needed; many per
// turn means the policy is too narrow.
func (s *Server) noteApprovalFriction(agent string, perTurn map[string]int) {
	for turnID, n := range perTurn {
		if turnID == "" {
			// Skip rather than bucket under an empty key that looks like one busy turn.
			continue
		}
		s.metrics.noteAgentApprovalsPerTurn(agent, n)
	}
}
