//go:build !core

package httpapi

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/extras/agent/agentd"
)

// An OpenAI-Responses-shaped adapter over the native turns API. Every frame
// carries the journal seq as its id so Last-Event-ID resumes mid-run. It is
// deliberately namespaced under the sandbox, never at /v1/responses.

// responseObject is the Responses-shaped view of a turn.
type responseObject struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	CreatedAt int64  `json:"created_at"`
	// Status uses the Responses vocabulary: queued | in_progress | completed | failed | cancelled.
	Status     string          `json:"status"`
	SessionID  string          `json:"session_id,omitempty"`
	OutputText string          `json:"output_text,omitempty"`
	Error      *responseError  `json:"error,omitempty"`
	Sandbox    string          `json:"sandbox_id"`
	Native     *agentd.Turn    `json:"native,omitempty"`
	Stream     json.RawMessage `json:"-"`
}

type responseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// responseStatusFor maps turn states onto the Responses vocabulary.
// An interrupted turn is "cancelled", not "failed": the user asked for it.
func responseStatusFor(state agentd.TurnState) string {
	switch state {
	case agentd.TurnQueued:
		return "queued"
	case agentd.TurnRunning, agentd.TurnAwaiting:
		return "in_progress"
	case agentd.TurnDone:
		return "completed"
	case agentd.TurnInterrupted:
		return "cancelled"
	case agentd.TurnFailed:
		return "failed"
	default:
		return "in_progress"
	}
}

// toResponse renders a turn in the Responses shape, with the full native turn alongside.
func toResponse(sandboxID string, t *agentd.Turn) responseObject {
	out := responseObject{
		ID: t.ID, Object: "response", CreatedAt: t.QueuedAt.Unix(),
		Status: responseStatusFor(t.State), SessionID: t.SessionID,
		OutputText: t.Response, Sandbox: sandboxID, Native: t,
	}
	if t.State == agentd.TurnFailed {
		msg := t.Error
		if msg == "" && t.ExitCode != nil {
			msg = fmt.Sprintf("the agent exited %d", *t.ExitCode)
		}
		if msg == "" {
			msg = "the turn failed"
		}
		out.Error = &responseError{Code: "agent_error", Message: msg}
	}
	return out
}

// defaultResponseWait bounds a non-streaming request; after it the object is
// returned as in_progress so the caller polls or streams instead of hanging in a proxy.
const defaultResponseWait = 60 * time.Second

// maxResponseWait caps what a caller may ask for.
const maxResponseWait = 10 * time.Minute

// handleAgentResponseCreate is the Responses-shaped submit.
//
//	POST /v1/sandboxes/{id}/agent/responses
func (s *Server) handleAgentResponseCreate(w http.ResponseWriter, r *http.Request) {
	id, _, ok := s.agentGate(r)
	if !ok {
		writeErrCode(w, http.StatusNotFound, codeAgentNotSupported, "sandbox not found")
		return
	}
	var body struct {
		// Message is accepted as an alias of Input for native-shape callers.
		Input     string  `json:"input"`
		Message   string  `json:"message"`
		SessionID string  `json:"session_id"`
		Stream    bool    `json:"stream"`
		Mode      string  `json:"mode"`
		Wait      float64 `json:"max_wait_seconds"`
		// Background returns immediately, like the Responses async flag.
		Background bool `json:"background"`
	}
	if !decodeBody(r, &body) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	input := strings.TrimSpace(cmp.Or(body.Input, body.Message))
	if input == "" {
		writeErr(w, http.StatusBadRequest, "input is required")
		return
	}

	c, err := s.agentClient(id)
	if err != nil {
		writeAgentErr(w, err)
		return
	}
	// An empty session id continues the latest conversation.
	thread := body.SessionID
	turn, err := c.Submit(r.Context(), agentd.SubmitRequest{
		Message: input, Mode: body.Mode, Thread: thread, Source: "responses",
	})
	if err != nil {
		writeAgentErr(w, err)
		return
	}
	s.auditDetail(r, "agent.turn", id, map[string]any{"turn": turn.ID, "via": "responses"})

	if body.Stream {
		s.streamResponse(w, r, id, c, turn)
		return
	}
	if body.Background {
		writeJSON(w, http.StatusAccepted, toResponse(id, turn))
		return
	}

	wait := time.Duration(body.Wait * float64(time.Second))
	if wait <= 0 {
		wait = defaultResponseWait
	}
	if wait > maxResponseWait {
		wait = maxResponseWait
	}
	final := s.awaitTurn(r, c, turn, wait)
	writeJSON(w, http.StatusOK, toResponse(id, final))
}

// awaitTurn polls until the turn reaches a terminal state or the budget runs
// out. Returns whatever it last saw rather than an error on timeout: the turn
// is still running and its id is still valid.
func (s *Server) awaitTurn(r *http.Request, c *agentd.Client, turn *agentd.Turn, budget time.Duration) *agentd.Turn {
	// One ticker, not time.After per iteration, so timers do not pile up over a long budget.
	poll := time.NewTicker(awaitTurnPollInterval)
	defer poll.Stop()
	deadline := time.Now().Add(budget)
	latest := turn
	for time.Now().Before(deadline) {
		if r.Context().Err() != nil {
			return latest
		}
		got, err := c.Turn(r.Context(), turn.ID)
		if err == nil {
			latest = got
			if got.State.Terminal() {
				return got
			}
		}
		select {
		case <-r.Context().Done():
			return latest
		case <-poll.C:
		}
	}
	return latest
}

// handleAgentResponseGet fetches a response.
//
//	GET /v1/sandboxes/{id}/agent/responses/{response}
func (s *Server) handleAgentResponseGet(w http.ResponseWriter, r *http.Request) {
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
	turn, err := c.Turn(r.Context(), r.PathValue("response"))
	if err != nil {
		writeAgentErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toResponse(id, turn))
}

// handleAgentResponseCancel cancels a running response.
//
//	POST /v1/sandboxes/{id}/agent/responses/{response}/cancel
func (s *Server) handleAgentResponseCancel(w http.ResponseWriter, r *http.Request) {
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
	rid := r.PathValue("response")
	if err := c.Interrupt(r.Context(), rid, 0); err != nil {
		var se *agentd.StatusError
		// Already finished is fine: the caller wanted it stopped and it is.
		if !errors.As(err, &se) || se.Code != http.StatusConflict {
			writeAgentErr(w, err)
			return
		}
	}
	turn, err := c.Turn(r.Context(), rid)
	if err != nil {
		writeAgentErr(w, err)
		return
	}
	s.auditDetail(r, "agent.interrupt", id, map[string]any{"turn": rid, "via": "responses"})
	writeJSON(w, http.StatusOK, toResponse(id, turn))
}

// handleAgentResponseStream reattaches to a response's event stream.
//
//	GET /v1/sandboxes/{id}/agent/responses/{response}/stream
func (s *Server) handleAgentResponseStream(w http.ResponseWriter, r *http.Request) {
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
	turn, err := c.Turn(r.Context(), r.PathValue("response"))
	if err != nil {
		writeAgentErr(w, err)
		return
	}
	s.streamResponse(w, r, id, c, turn)
}

// streamResponse translates the native journal stream into Responses events.
// Resume position is Last-Event-ID when present, else the turn's start cursor,
// so reattaching to a finished response replays it in full.
func (s *Server) streamResponse(w http.ResponseWriter, r *http.Request, sandboxID string,
	c *agentd.Client, turn *agentd.Turn) {

	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	since := turn.SeqStart
	if since > 0 {
		since-- // inclusive of the turn's own first event
	}
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			since = n
		}
	}

	body, err := c.Events(r.Context(), since)
	if err != nil {
		writeAgentErr(w, err)
		return
	}
	defer body.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	emit := func(seq uint64, name string, payload any) bool {
		b, err := json.Marshal(payload)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", seq, name, b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// response.created first, always: an OpenAI-shaped client reads the id from it.
	if !emit(since, "response.created", map[string]any{
		"id": turn.ID, "session_id": turn.SessionID, "sandbox_id": sandboxID,
	}) {
		return
	}

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue // id/comment/blank lines carry no event
		}
		var ev struct {
			Seq     uint64          `json:"seq"`
			Type    string          `json:"type"`
			TurnID  string          `json:"turn_id"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
			continue
		}
		// Session-level events carry no turn id; a Responses client has nowhere to put them.
		if ev.TurnID != turn.ID {
			continue
		}

		name, payload, terminal := translateEvent(ev.Type, ev.Payload)
		if name == "" {
			continue
		}
		if !emit(ev.Seq, name, payload) {
			return
		}
		if terminal {
			return
		}
	}
}

// translateEvent maps one journal event onto a Responses event name and payload.
// Returns an empty name for events with no counterpart.
func translateEvent(typ string, raw json.RawMessage) (name string, payload any, terminal bool) {
	var p map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &p)
	}
	get := func(k string) any {
		if p == nil {
			return nil
		}
		return p[k]
	}
	str := func(k string) string {
		v, _ := get(k).(string)
		return v
	}

	switch typ {
	case agentd.EventTurnStarted:
		return "response.in_progress", map[string]any{}, false

	case agentd.EventTurnThinking:
		return "response.reasoning.delta", map[string]any{"text": str("text")}, false

	case agentd.EventTurnOutput:
		return "response.output_text.delta", map[string]any{"text": str("text")}, false

	case agentd.EventTurnOutputTruncated:
		// Non-standard event; staying silent would present a truncated stream as complete.
		return "response.output_text.truncated", map[string]any{"reason": str("reason")}, false

	case agentd.EventToolStarted:
		return "response.tool_call.started",
			map[string]any{"tool": str("tool"), "id": str("tool_id"), "label": str("detail")}, false

	case agentd.EventToolFinished:
		out := map[string]any{"tool": str("tool"), "id": str("tool_id")}
		if ms := get("ms"); ms != nil {
			out["duration_ms"] = ms
		}
		return "response.tool_call.completed", out, false

	case agentd.EventToolFailed:
		return "response.tool_call.failed",
			map[string]any{"tool": str("tool"), "id": str("tool_id"), "error": str("result")}, false

	case agentd.EventApprovalRequested:
		// Non-standard; a client that ignores it just sees a slow tool call.
		return "response.approval.requested", p, false

	case agentd.EventTurnFinished:
		out := map[string]any{"status": "completed"}
		if u := usageFrom(p); u != nil {
			out["usage"] = u
		}
		return "response.completed", out, true

	case agentd.EventTurnFailed:
		msg := "the turn failed"
		if code := get("exit_code"); code != nil {
			msg = fmt.Sprintf("the agent exited %v", code)
		}
		if e := str("error"); e != "" {
			msg = e
		}
		return "response.failed", map[string]any{
			"error": map[string]any{"code": "agent_error", "message": msg},
		}, true

	case agentd.EventTurnInterrupted:
		// A deliberate stop is not a failure.
		return "response.cancelled", map[string]any{"status": "cancelled"}, true
	}
	return "", nil, false
}

// usageFrom builds a usage block from a terminal event's payload.
// Returns nil rather than fabricated zeros when the agent reported nothing.
func usageFrom(p map[string]any) map[string]any {
	if p == nil {
		return nil
	}
	out := map[string]any{}
	for src, dst := range map[string]string{
		"input_tokens":      "input_tokens",
		"output_tokens":     "output_tokens",
		"cache_read_tokens": "cache_read_tokens",
		"model_cost_usd":    "cost_usd",
	} {
		if v, ok := p[src]; ok {
			out[dst] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	if in, ok := out["input_tokens"].(float64); ok {
		if o, ok := out["output_tokens"].(float64); ok {
			out["total_tokens"] = in + o
		}
	}
	return out
}

// awaitTurnPollInterval is how often awaitTurn re-probes.
const awaitTurnPollInterval = 400 * time.Millisecond
