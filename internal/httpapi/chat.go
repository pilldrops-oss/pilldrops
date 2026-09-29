//go:build !core

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/pilldrops-oss/pilldrops/internal/extras/bridge/telegram"
	"github.com/pilldrops-oss/pilldrops/internal/httpx"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// chatBoxes adapts the daemon to the Telegram bridge runner, so web chat and
// Telegram share one "send the agent a message" path.
type chatBoxes struct{ s *Server }

func (c chatBoxes) Get(id string) (telegram.SandboxState, bool) {
	sb, ok := c.s.d.Get(id)
	if !ok {
		return telegram.SandboxState{}, false
	}
	return telegram.SandboxState{Template: sb.Template, Paused: sb.State == daemon.StatePaused}, true
}

func (c chatBoxes) Resume(ctx context.Context, id string) error { return c.s.d.Resume(ctx, id) }

func (c chatBoxes) Exec(ctx context.Context, id string, spec rt.ExecSpec) (rt.ExecResult, error) {
	// The agent must see the same env the terminal does.
	spec.Env = mergeExecEnv(c.s.configEnvFor(ctx, id), spec.Env)
	return c.s.d.Exec(ctx, id, spec)
}

// chatSunsetDate is when the blocking chat contract stops being supported.
const chatSunsetDate = "2027-02-10"

// handleChatSend runs a message on the workstation's agent and returns its reply.
//
//	POST /v1/sandboxes/{id}/chat  {"message":"...", "autonomous":false}
func (s *Server) handleChatSend(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	var body struct {
		Message string `json:"message"`
		// Autonomous grants the agent its --dangerously-* flags for this turn. Default off.
		Autonomous bool `json:"autonomous"`
	}
	if !decodeBody(r, &body) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	msg := strings.TrimSpace(body.Message)
	if msg == "" {
		writeErr(w, http.StatusBadRequest, "message is required")
		return
	}
	if len(msg) > 32*1024 {
		writeErr(w, http.StatusBadRequest, "message is too long")
		return
	}

	org := orgID(r)
	followUp := s.st.HasChatMessages(r.Context(), id)

	// Take the turn slot before recording the message, so a busy workstation
	// does not leave an unanswered message in the transcript.
	release, free := telegram.AcquireTurn(id)
	if !free {
		writeErr(w, http.StatusConflict, telegram.ErrBusy.Error())
		return
	}
	defer release()

	// Record the user's turn before running, so a reload mid-run still shows it.
	_ = s.st.AppendChatMessage(r.Context(), id, org, "user", msg)

	// Prefer agentd (queue, journal, spend cap, approvals); fall back to direct exec.
	reply, err := s.runChatViaAgentd(r.Context(), id, msg, body.Autonomous)
	if errors.Is(err, errChatAgentdUnavailable) {
		// NoGate: the turn slot is already held above.
		runner := &telegram.DaemonRunner{Boxes: chatBoxes{s}, Gate: telegram.NoGate}
		reply, err = runner.Run(r.Context(), id, msg, telegram.RunOpts{
			FollowUp: followUp, Autonomous: body.Autonomous,
		})
	}
	if err != nil {
		// The failure is part of the conversation, not a 500.
		_ = s.st.AppendChatMessage(r.Context(), id, org, "agent", err.Error())
		s.auditDetail(r, "sandbox.chat", id, map[string]any{"ok": false})
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(reply) == "" {
		reply = "(the agent finished without printing anything)"
	}
	_ = s.st.AppendChatMessage(r.Context(), id, org, "agent", reply)
	s.auditDetail(r, "sandbox.chat", id, map[string]any{"ok": true, "autonomous": body.Autonomous})
	writeJSON(w, http.StatusOK, map[string]any{
		"reply": reply,
		"deprecation": map[string]any{
			"blocking_contract": "deprecated",
			"sunset":            chatSunsetDate,
			"replacement":       "POST /v1/sandboxes/{id}/agent/turns then GET …/agent/events",
			"why": "This call holds the connection open for the whole turn, so a dropped " +
				"connection loses the reply. A submitted turn survives it.",
		},
	})
}

// handleChatHistory returns the transcript.
//
//	GET /v1/sandboxes/{id}/chat?limit=200
func (s *Server) handleChatHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	limit := httpx.QueryInt(r, "limit", 0)
	msgs, err := s.st.ListChatMessages(r.Context(), id, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not read the transcript")
		return
	}
	if msgs == nil {
		msgs = []store.ChatMessage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
}
