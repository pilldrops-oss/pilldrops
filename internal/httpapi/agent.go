//go:build !core

package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/extras/agent/agentd"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
)

// The agent API under /v1/sandboxes/{id}/agent/* exists only for sandboxes
// whose template Kind is KindAgent. A sandbox that fails the gate gets 404,
// never 403, so its existence is not confirmed to a caller without access.

// Stable error codes clients can branch on.
const (
	codeAgentNotSupported = "agent_not_supported"
	codeAgentUnreachable  = "agent_unreachable"
	codeAgentPaused       = "agent_paused"
	// Distinct from codeAgentNotSupported: the caller already owns an agent
	// sandbox, so naming the transport gap leaks nothing.
	codeAgentTransportUnsupported = "agent_transport_unsupported"
)

// agentGate resolves the sandbox and confirms it runs an agent. Ownership is
// checked before kind so a caller cannot probe templates on sandboxes they do not own.
func (s *Server) agentGate(r *http.Request) (id string, tmpl templates.Template, ok bool) {
	id = r.PathValue("id")
	if !s.owns(r, id) {
		return "", templates.Template{}, false
	}
	t, ok := s.agentTemplateOf(id)
	if !ok {
		return "", templates.Template{}, false
	}
	return id, t, true
}

// agentTemplateOf resolves a sandbox's agent template without checking
// ownership. The caller must have authorised the id already, or this is a cross-tenant read.
func (s *Server) agentTemplateOf(id string) (templates.Template, bool) {
	sb, found := s.d.Get(id)
	if !found {
		return templates.Template{}, false
	}
	t, resolved := templates.Resolve(sb.Template)
	if !resolved || t.Kind != templates.KindAgent || t.Agent == nil {
		return templates.Template{}, false
	}
	return t, true
}

// agentClient returns a client for this sandbox's agentd.
func (s *Server) agentClient(id string) (*agentd.Client, error) {
	addr, token, err := s.d.AgentEndpoint(id)
	if err != nil {
		return nil, err
	}
	return &agentd.Client{Addr: addr, Token: token}, nil
}

// writeAgentErr maps a transport or agentd error onto a status and code.
// agentd's own refusals keep their status and message, which are written for a person.
func writeAgentErr(w http.ResponseWriter, err error) {
	var se *agentd.StatusError
	switch {
	case errors.Is(err, rt.ErrNotFound):
		writeErrCode(w, http.StatusNotFound, codeAgentNotSupported, "sandbox not found")
	case errors.Is(err, rt.ErrPaused):
		// 409, not 404: the sandbox exists and the caller can wake it.
		writeErrCode(w, http.StatusConflict, codeAgentPaused,
			"the workstation is paused; send a task to wake it")
	case errors.Is(err, rt.ErrUnsupported):
		writeErrCode(w, http.StatusNotImplemented, codeAgentTransportUnsupported,
			"this runtime cannot reach an agent supervisor inside the sandbox")
	case errors.As(err, &se):
		writeErrCode(w, se.Code, "", se.Message)
	case errors.Is(err, agentd.ErrAgentUnreachable):
		writeErrCode(w, http.StatusServiceUnavailable, codeAgentUnreachable,
			"the agent supervisor is not answering; it may still be starting")
	default:
		writeErrCode(w, http.StatusInternalServerError, "", err.Error())
	}
}

// countAgentFailure records why an agent call failed. An unreachable
// supervisor is alert-worthy; a spend stop or refused turn is user-fixable.
func (s *Server) countAgentFailure(agent string, err error) {
	switch {
	case errors.Is(err, agentd.ErrAgentUnreachable):
		s.metrics.incAgentUnreachable(agent)
	case errors.Is(err, agentd.ErrSpendCapReached):
		s.metrics.incAgentSpendStop(agent)
	default:
		var se *agentd.StatusError
		if errors.As(err, &se) && se.Code == http.StatusPaymentRequired {
			s.metrics.incAgentSpendStop(agent)
			return
		}
		s.metrics.incAgentTurn(agent, "refused")
	}
}

// handleAgentStatus reports the session.
//
//	GET /v1/sandboxes/{id}/agent/status
func (s *Server) handleAgentStatus(w http.ResponseWriter, r *http.Request) {
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
	st, err := c.Status(r.Context())
	if err != nil {
		// Live first, since a stale "idle" mid-turn is wrong. When the sandbox
		// cannot answer at all, last-known state beats a 409.
		if agentUnavailableLive(err) && s.serveMirroredStatus(w, r, id, tmpl) {
			return
		}
		writeAgentErr(w, err)
		return
	}
	// Identity and limits come from the manifest, not the guest. Some CLIs
	// have no resume command, so the UI is told rather than left to guess.
	remembers := tmpl.Agent.ContinueCmd != "" || tmpl.Agent.ResumeCmd != ""
	agent := map[string]any{"id": tmpl.ID, "name": tmpl.Name, "remembers": remembers}
	if !remembers {
		agent["memory_note"] = "This agent's CLI has no resume command, so each message starts a " +
			"new conversation and it will not recall earlier messages."
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sandbox_id": id,
		"agent":      agent,
		"session":    st,
		// Labelled on the live path too, so clients never infer the source.
		"served_from": servedFromNode,
	})
}

// handleAgentSubmit accepts a task and returns 202; the turn is accepted, not finished.
//
//	POST /v1/sandboxes/{id}/agent/turns
func (s *Server) handleAgentSubmit(w http.ResponseWriter, r *http.Request) {
	id, tmpl, ok := s.agentGate(r)
	tmplID := tmpl.ID
	if !ok {
		writeErrCode(w, http.StatusNotFound, codeAgentNotSupported, "sandbox not found")
		return
	}
	var body struct {
		Message  string `json:"message"`
		Mode     string `json:"mode"`
		Thread   string `json:"thread"`
		Priority bool   `json:"priority"`
		Wake     *bool  `json:"wake"`
	}
	if !decodeBody(r, &body) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// A whitespace-only message would bill a turn to produce nothing.
	msg := strings.TrimSpace(body.Message)
	if msg == "" {
		writeErr(w, http.StatusBadRequest, "message is required")
		return
	}

	// Wake on task by default; a caller can opt out of the resume.
	wake := body.Wake == nil || *body.Wake
	woke := false
	if sb, found := s.d.Get(id); found && sb.State == daemon.StatePaused {
		if !wake {
			writeErrCode(w, http.StatusConflict, codeAgentPaused,
				"the workstation is paused and wake was declined")
			return
		}
		if err := s.d.Resume(r.Context(), id); err != nil {
			writeErr(w, http.StatusInternalServerError, "could not wake the workstation: "+err.Error())
			return
		}
		woke = true
	}

	c, err := s.agentClient(id)
	if err != nil {
		writeAgentErr(w, err)
		return
	}
	turn, err := c.Submit(r.Context(), agentd.SubmitRequest{
		Message: msg, Mode: body.Mode, Thread: body.Thread,
		Source: agentSource(r), Priority: body.Priority,
	})
	if err != nil {
		s.countAgentFailure(tmplID, err)
		writeAgentErr(w, err)
		return
	}
	s.metrics.incAgentTurn(tmplID, "accepted")
	s.auditDetail(r, "agent.turn", id, map[string]any{
		"turn": turn.ID, "mode": turn.Mode, "woke": woke, "actor": actorOf(r),
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"turn_id":      turn.ID,
		"state":        turn.State,
		"seq":          turn.SeqStart,
		"woke_sandbox": woke,
		"stream": map[string]string{
			"sse": "/v1/sandboxes/" + id + "/agent/events?since_seq=" + strconv.FormatUint(turn.SeqStart, 10),
		},
	})
}

// handleAgentTurns lists turns.
//
//	GET /v1/sandboxes/{id}/agent/turns
func (s *Server) handleAgentTurns(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.agentGate(r)
	if !ok {
		writeErrCode(w, http.StatusNotFound, codeAgentNotSupported, "sandbox not found")
		return
	}
	// The mirror is the only source that survives a paused or destroyed sandbox.
	c, err := s.agentClient(id)
	if err != nil {
		if agentUnavailableLive(err) && s.serveMirroredTurns(w, r, id,
			"this workstation is not running; these turns come from the control plane's "+
				"copy and reflect what it did while it was up") {
			return
		}
		writeAgentErr(w, err)
		return
	}
	turns, err := c.Turns(r.Context())
	if err != nil {
		if agentUnavailableLive(err) && s.serveMirroredTurns(w, r, id,
			"the agent supervisor did not answer; these turns come from the control "+
				"plane's copy") {
			return
		}
		writeAgentErr(w, err)
		return
	}
	if turns == nil {
		turns = []*agentd.Turn{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"turns": turns, "served_from": servedFromNode,
	})
}

// handleAgentTurn fetches one turn.
//
//	GET /v1/sandboxes/{id}/agent/turns/{turn}
func (s *Server) handleAgentTurn(w http.ResponseWriter, r *http.Request) {
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
	turn, err := c.Turn(r.Context(), r.PathValue("turn"))
	if err != nil {
		writeAgentErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, turn)
}

// handleAgentInterrupt stops a running turn.
//
//	POST /v1/sandboxes/{id}/agent/turns/{turn}/interrupt
func (s *Server) handleAgentInterrupt(w http.ResponseWriter, r *http.Request) {
	id, tmpl, ok := s.agentGate(r)
	if !ok {
		writeErrCode(w, http.StatusNotFound, codeAgentNotSupported, "sandbox not found")
		return
	}
	var body struct {
		GraceSeconds float64 `json:"grace_seconds"`
	}
	_ = decodeBody(r, &body)

	c, err := s.agentClient(id)
	if err != nil {
		writeAgentErr(w, err)
		return
	}
	turnID := r.PathValue("turn")
	grace := time.Duration(body.GraceSeconds * float64(time.Second))
	if err := c.Interrupt(r.Context(), turnID, grace); err != nil {
		s.countAgentFailure(tmpl.ID, err)
		writeAgentErr(w, err)
		return
	}
	s.metrics.incAgentInterrupt(tmpl.ID)
	s.auditDetail(r, "agent.interrupt", id, map[string]any{"turn": turnID})
	writeJSON(w, http.StatusOK, map[string]any{"state": "interrupting"})
}

// handleAgentTurnDelete dequeues a turn that has not started.
//
//	DELETE /v1/sandboxes/{id}/agent/turns/{turn}
func (s *Server) handleAgentTurnDelete(w http.ResponseWriter, r *http.Request) {
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
	turnID := r.PathValue("turn")
	if err := c.RemoveTurn(r.Context(), turnID); err != nil {
		writeAgentErr(w, err)
		return
	}
	s.auditDetail(r, "agent.dequeue", id, map[string]any{"turn": turnID})
	writeJSON(w, http.StatusOK, map[string]any{"removed": true})
}

// handleAgentReorder sets the pending queue order.
//
//	POST /v1/sandboxes/{id}/agent/queue/reorder
func (s *Server) handleAgentReorder(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.agentGate(r)
	if !ok {
		writeErrCode(w, http.StatusNotFound, codeAgentNotSupported, "sandbox not found")
		return
	}
	var body struct {
		Order []string `json:"order"`
	}
	if !decodeBody(r, &body) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	c, err := s.agentClient(id)
	if err != nil {
		writeAgentErr(w, err)
		return
	}
	if err := c.Reorder(r.Context(), body.Order); err != nil {
		writeAgentErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reordered": true})
}

// handleAgentModels reports the models this agent can use.
//
//	GET /v1/sandboxes/{id}/agent/models
func (s *Server) handleAgentModels(w http.ResponseWriter, r *http.Request) {
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
	out, err := c.Models(r.Context())
	if err != nil {
		writeAgentErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAgentThreads lists the workstation's conversations.
//
//	GET /v1/sandboxes/{id}/agent/threads
func (s *Server) handleAgentThreads(w http.ResponseWriter, r *http.Request) {
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
	threads, err := c.Threads(r.Context())
	if err != nil {
		writeAgentErr(w, err)
		return
	}
	if threads == nil {
		threads = []agentd.Thread{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"threads": threads})
}

// handleAgentThread returns one conversation's turns.
//
//	GET /v1/sandboxes/{id}/agent/threads/{thread}
func (s *Server) handleAgentThread(w http.ResponseWriter, r *http.Request) {
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
	turns, err := c.Thread(r.Context(), r.PathValue("thread"))
	if err != nil {
		writeAgentErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("thread"), "turns": turns})
}

// handleAgentBudget reports or sets the workstation's spend cap.
//
//	GET   /v1/sandboxes/{id}/agent/budget
//	PATCH /v1/sandboxes/{id}/agent/budget  {"cap_usd": 5.00}
func (s *Server) handleAgentBudget(w http.ResponseWriter, r *http.Request) {
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
	if r.Method == http.MethodGet {
		out, err := c.Budget(r.Context())
		if err != nil {
			writeAgentErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	var body struct {
		CapUSD *float64 `json:"cap_usd"`
	}
	if !decodeBody(r, &body) || body.CapUSD == nil {
		writeErr(w, http.StatusBadRequest, "cap_usd is required (0 clears the cap)")
		return
	}
	out, err := c.SetBudget(r.Context(), *body.CapUSD)
	if err != nil {
		writeAgentErr(w, err)
		return
	}
	s.auditDetail(r, "agent.budget", id, map[string]any{"cap_usd": *body.CapUSD})
	writeJSON(w, http.StatusOK, out)
}

// handleAgentEvents streams the journal through byte for byte. Last-Event-ID
// is honoured as the cursor so a browser's automatic reconnect resumes.
//
//	GET /v1/sandboxes/{id}/agent/events?since_seq=N
func (s *Server) handleAgentEvents(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.agentGate(r)
	if !ok {
		writeErrCode(w, http.StatusNotFound, codeAgentNotSupported, "sandbox not found")
		return
	}
	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	since := uint64(0)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			since = n
		}
	} else if n, err := strconv.ParseUint(r.URL.Query().Get("since_seq"), 10, 64); err == nil {
		since = n
	}

	c, err := s.agentClient(id)
	if err != nil {
		// No live journal for a paused or destroyed sandbox; replay the mirror.
		if agentUnavailableLive(err) && s.replayMirroredEvents(w, r, flusher, id, since) {
			return
		}
		writeAgentErr(w, err)
		return
	}
	body, err := c.Events(r.Context(), since)
	if err != nil {
		if agentUnavailableLive(err) && s.replayMirroredEvents(w, r, flusher, id, since) {
			return
		}
		writeAgentErr(w, err)
		return
	}
	defer body.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Flush each chunk; io.Copy would buffer into bursts.
	buf := make([]byte, 4<<10)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			flusher.Flush()
		}
		if readErr != nil {
			return
		}
	}
}
