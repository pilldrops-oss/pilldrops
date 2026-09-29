package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// idemGuard holds a claimed Idempotency-Key for the life of one request. A
// guard for a request that sent no key is inert.
type idemGuard struct {
	s    *Server
	org  string
	key  string
	ctx  context.Context
	held bool
}

// beginIdempotent claims key for org before any work happens. done=true means
// the response (a replay or a 409) is already written. The caller must defer
// release so a failed attempt hands the key back.
func (s *Server) beginIdempotent(ctx context.Context, w http.ResponseWriter, org, key string, replayStatus int) (*idemGuard, bool) {
	g := &idemGuard{s: s, org: org, key: key, ctx: ctx}
	if key == "" {
		return g, false
	}
	claimed, err := s.st.ReserveIdempotent(ctx, org, key, []byte(idemInProgress))
	if err != nil {
		// Never proceed unguarded — but say why, at the tier that decided. This
		// refusal used to be a bare 503 with no log line at either tier, which
		// is how an intermittent store fault read as "the rig is flaky" instead
		// of as one named error anyone could go and look at.
		slog.Error("refused a request because its Idempotency-Key could not be claimed",
			"component", "httpapi", "org", org, "idem_key", key, "err", err)
		writeErr(w, http.StatusServiceUnavailable,
			"the Idempotency-Key on this request could not be claimed, so it was refused rather "+
				"than run unguarded: "+err.Error())
		return g, true
	}
	if claimed {
		g.held = true
		return g, false
	}
	if body, ok, _ := s.st.IdempotentResult(ctx, org, key); ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(replayStatus)
		_, _ = w.Write(body)
		return g, true
	}
	// Also logged: a caller seeing this needs to know it is their own earlier
	// request holding the key, not a fault.
	slog.Info("a request was refused because an earlier one still holds its Idempotency-Key",
		"component", "httpapi", "org", org, "idem_key", key)
	w.Header().Set("Retry-After", "2")
	writeErr(w, http.StatusConflict, "a request with this Idempotency-Key is already in progress")
	return g, true
}

// finish commits resp as the stored response for the key. A failed write is
// not fatal: the claim stays in progress until stale, so a retry gets 409
// rather than a duplicate.
func (g *idemGuard) finish(resp any) {
	if g == nil || !g.held {
		return
	}
	b, err := json.Marshal(resp)
	if err != nil {
		return
	}
	if err := g.s.st.FinishIdempotent(g.ctx, g.org, g.key, b); err == nil {
		g.held = false
	}
}

// release hands the key back when the guarded work did not complete. No-op after finish.
func (g *idemGuard) release() {
	if g == nil || !g.held {
		return
	}
	// g.ctx is usually already cancelled here (client disconnect), which would
	// skip the release and strand the key.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(g.ctx), 5*time.Second)
	defer cancel()
	_ = g.s.st.ReleaseIdempotent(ctx, g.org, g.key)
	g.held = false
}

// idemInProgress mirrors store's claim placeholder.
const idemInProgress = "processing"

// scopedIdemKey namespaces an Idempotency-Key by action and sandbox id, so a
// key reused across actions or a batch does not replay the wrong response.
// NUL separators because no caller-supplied id or key can contain one.
func scopedIdemKey(action, id, key string) string {
	if key == "" {
		return ""
	}
	return action + "\x00" + id + "\x00" + key
}
