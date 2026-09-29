package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
)

// agentd listens on the guest's shared-bridge address (VMIP:6002, beside envd's
// 6001), so authentication reuses internal/sbtoken.

// LoopbackAddr is the default listen address.
const LoopbackAddr = "127.0.0.1:6002"

// DefaultPort is agentd's port, mirroring envd's 6001.
const DefaultPort = 6002

// ErrExposedWithoutAuth refuses a routable listen address with no token. Unlike
// envd, agentd can start processes and read the workspace, so a missing token
// would publish that to every sandbox on the shared bridge.
var ErrExposedWithoutAuth = errors.New(
	"refusing to listen on a routable address without -token-hash-file: " +
		"every sandbox on the shared network could otherwise drive this agent")

// Server exposes the supervisor over HTTP.
type Server struct {
	Sup     *Supervisor
	Journal *Journal
	Queue   *Queue
	// TokenHash is the SHA-256 of the node's access token. Empty disables auth, for tests only.
	TokenHash string
	// KeepAlive is how often an idle event stream emits a comment. Zero uses the default.
	KeepAlive time.Duration
	// Addr is the listen address. Empty means loopback; routable requires TokenHash.
	Addr string
	// Models lists the agent's models. Nil means listing is unsupported, which is
	// reported as such rather than as an empty list.
	Models func(ctx context.Context) ([]string, error)
	// CurrentModel is the model the agent is configured with, when known.
	CurrentModel func() string
	// Gate is the approval gate. Nil means no verified permission-tool flag, which
	// the approval endpoints report as 501 rather than an empty list.
	Gate *Gate
}

// isLoopbackAddr reports whether addr binds only this host. An empty or wildcard
// host publishes the port on every interface and is not loopback.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

const defaultKeepAlive = 25 * time.Second

func (s *Server) keepAlive() time.Duration {
	if s.KeepAlive > 0 {
		return s.KeepAlive
	}
	return defaultKeepAlive
}

// Handler returns the mux with authentication applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Health is unauthenticated: the boot probe polls it before the token is available.
	mux.HandleFunc("GET /health", s.handleHealth)

	mux.Handle("GET /status", s.auth(s.handleStatus))
	mux.Handle("GET /snapshot-safe", s.auth(s.handleSnapshotSafe))
	mux.Handle("POST /turns", s.auth(s.handleSubmit))
	mux.Handle("GET /turns", s.auth(s.handleListTurns))
	mux.Handle("GET /turns/{id}", s.auth(s.handleGetTurn))
	mux.Handle("POST /turns/{id}/interrupt", s.auth(s.handleInterrupt))
	mux.Handle("DELETE /turns/{id}", s.auth(s.handleRemoveTurn))
	mux.Handle("POST /queue/reorder", s.auth(s.handleReorder))
	mux.Handle("GET /events", s.auth(s.handleEvents))
	mux.Handle("GET /models", s.auth(s.handleModels))
	mux.Handle("GET /threads", s.auth(s.handleThreads))
	mux.Handle("GET /threads/{id}", s.auth(s.handleThread))
	mux.Handle("GET /budget", s.auth(s.handleBudgetGet))
	mux.Handle("PATCH /budget", s.auth(s.handleBudgetSet))
	// Registered even with a nil Gate so the endpoints answer 501 with a reason, not 404.
	mux.Handle("GET /approvals", s.auth(s.handleApprovalsList))
	mux.Handle("POST /approvals/{id}", s.auth(s.handleApprovalDecide))
	mux.Handle("GET /policy", s.auth(s.handlePolicyGet))
	mux.Handle("DELETE /policy/remembered", s.auth(s.handlePolicyForget))
	return mux
}

// Listen binds the server, refusing a routable address without a token hash.
func (s *Server) Listen() (net.Listener, *http.Server, error) {
	addr := s.Addr
	if addr == "" {
		addr = LoopbackAddr
	}
	if s.TokenHash == "" && !isLoopbackAddr(addr) {
		return nil, nil, fmt.Errorf("%w (address %s)", ErrExposedWithoutAuth, addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler: s.Handler(),
		// No write timeout: it would sever the long-lived event stream.
		ReadHeaderTimeout: 10 * time.Second,
	}
	return ln, srv, nil
}

// auth verifies the node's access token in constant time.
func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.TokenHash != "" && !sbtoken.Verify(r.Header.Get(sbtoken.MetadataKey), s.TokenHash) {
			writeJSONErr(w, http.StatusUnauthorized, "invalid access token")
			return
		}
		next(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "last_seq": s.Journal.LastSeq()})
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Sup.Status())
}

// handleSnapshotSafe answers the node's pre-snapshot check.
func (s *Server) handleSnapshotSafe(w http.ResponseWriter, _ *http.Request) {
	ok, reason := s.Sup.SafeToSnapshot()
	writeJSON(w, http.StatusOK, map[string]any{"safe": ok, "reason": reason})
}

func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Message  string `json:"message"`
		Mode     string `json:"mode"`
		Thread   string `json:"thread"`
		Source   string `json:"source"`
		Priority bool   `json:"priority"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// agentd is reachable directly, so validate here too.
	msg := strings.TrimSpace(body.Message)
	if msg == "" {
		writeJSONErr(w, http.StatusBadRequest, "message is required")
		return
	}
	turn, err := s.Sup.Submit(msg, body.Mode, body.Thread, body.Source, body.Priority)
	if err != nil {
		if errors.Is(err, ErrSpendCapReached) {
			writeJSONErr(w, http.StatusPaymentRequired, err.Error())
			return
		}
		if errors.Is(err, ErrQueueFull) {
			w.Header().Set("Retry-After", "30")
			writeJSONErr(w, http.StatusTooManyRequests, err.Error())
			return
		}
		if errors.Is(err, ErrModeUnavailable) {
			writeJSONErr(w, http.StatusConflict, err.Error())
			return
		}
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, turn)
}

// handleModels serves GET /models. Most agents cannot list models, so the answer
// says listing is unsupported and still reports the configured model.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"supported": s.Models != nil, "models": []string{}}
	if s.CurrentModel != nil {
		if m := s.CurrentModel(); m != "" {
			out["current"] = m
		}
	}
	if s.Models == nil {
		out["reason"] = "this agent cannot list its models; set one in its config or with its model flag"
		writeJSON(w, http.StatusOK, out)
		return
	}
	models, err := s.Models(r.Context())
	if err != nil {
		out["supported"] = false
		out["reason"] = "could not list models: " + err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	if models == nil {
		models = []string{}
	}
	out["models"] = models
	writeJSON(w, http.StatusOK, out)
}

// handleBudgetGet serves GET /budget.
func (s *Server) handleBudgetGet(w http.ResponseWriter, _ *http.Request) {
	if s.Sup.Budget == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false,
			"reason":  "no spend cap is configured for this workstation",
		})
		return
	}
	snap := s.Sup.Budget.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "budget": snap})
}

// handleBudgetSet serves PATCH /budget {"cap_usd": N}. Zero clears the cap.
func (s *Server) handleBudgetSet(w http.ResponseWriter, r *http.Request) {
	if s.Sup.Budget == nil {
		writeJSONErr(w, http.StatusNotImplemented,
			"this workstation has no budget store; restart the supervisor with a state directory")
		return
	}
	var body struct {
		CapUSD *float64 `json:"cap_usd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.CapUSD == nil {
		writeJSONErr(w, http.StatusBadRequest, "cap_usd is required (0 clears the cap)")
		return
	}
	snap := s.Sup.Budget.SetCap(*body.CapUSD)
	// Raising the cap un-freezes the queue.
	if !snap.Exceeded {
		s.Sup.Recovered()
		s.Sup.Notify()
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "budget": snap})
}

// handleThreads serves GET /threads.
func (s *Server) handleThreads(w http.ResponseWriter, _ *http.Request) {
	threads := s.Queue.Threads()
	if threads == nil {
		threads = []Thread{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"threads": threads})
}

// handleThread serves GET /threads/{id}. The id "unknown" addresses turns with
// no reported session id.
func (s *Server) handleThread(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "unknown" {
		id = ""
	}
	turns := s.Queue.TurnsForThread(id)
	if len(turns) == 0 {
		writeJSONErr(w, http.StatusNotFound, "no such thread")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "turns": turns})
}

func (s *Server) handleListTurns(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"turns": s.Queue.List()})
}

func (s *Server) handleGetTurn(w http.ResponseWriter, r *http.Request) {
	turn, err := s.Queue.Get(r.PathValue("id"))
	if err != nil {
		writeJSONErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, turn)
}

func (s *Server) handleInterrupt(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GraceSeconds float64 `json:"grace_seconds"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // body is optional

	grace := time.Duration(body.GraceSeconds * float64(time.Second))
	err := s.Sup.Interrupt(r.PathValue("id"), grace)
	switch {
	case errors.Is(err, ErrNoTurnRunning):
		// 409, not 404: the turn may exist and simply be finished.
		writeJSONErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrNotFound):
		writeJSONErr(w, http.StatusConflict, err.Error())
	case err != nil:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"interrupting": true})
	}
}

func (s *Server) handleRemoveTurn(w http.ResponseWriter, r *http.Request) {
	err := s.Queue.Remove(r.PathValue("id"))
	switch {
	case errors.Is(err, ErrNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	case err != nil:
		writeJSONErr(w, http.StatusConflict, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]any{"removed": true})
	}
}

func (s *Server) handleReorder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Order []string `json:"order"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := s.Queue.Reorder(body.Order); err != nil {
		writeJSONErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pending": s.Queue.Pending()})
}

// handleEvents streams the journal as SSE from ?since_seq=N (Last-Event-ID wins).
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	cursor := parseCursor(r)

	// Subscribe before the first replay so an event appended mid-replay is not missed.
	nudge, release := s.Journal.Subscribe()
	defer release()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	keepAlive := time.NewTicker(s.keepAlive())
	defer keepAlive.Stop()

	for {
		sent, err := s.drain(w, cursor)
		if err != nil {
			return
		}
		if sent > cursor {
			cursor = sent
			flusher.Flush()
		}

		select {
		case <-ctx.Done():
			return
		case <-nudge:
		case <-keepAlive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// drain writes every event after cursor and returns the new cursor.
func (s *Server) drain(w http.ResponseWriter, cursor uint64) (uint64, error) {
	newCursor := cursor
	var writeErr error
	err := s.Journal.Replay(cursor, func(seq uint64, line []byte) error {
		if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", seq, line); err != nil {
			writeErr = err
			return err
		}
		newCursor = seq
		return nil
	})
	if writeErr != nil {
		return newCursor, writeErr
	}
	return newCursor, err
}

// parseCursor resolves the resume position. Last-Event-ID wins over since_seq
// because browsers set it on reconnect with what they actually received.
func parseCursor(r *http.Request) uint64 {
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	if n, err := strconv.ParseUint(r.URL.Query().Get("since_seq"), 10, 64); err == nil {
		return n
	}
	return 0
}
