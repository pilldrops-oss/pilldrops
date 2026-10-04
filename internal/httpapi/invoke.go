package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/entitlements"
	"github.com/pilldrops-oss/pilldrops/internal/extras/functions"
	"github.com/pilldrops-oss/pilldrops/internal/proxy"
	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// coldStartReadyTimeout is how long an invoke waits for the handler to bind
// its port after a cold start. A var so tests can shorten it.
var coldStartReadyTimeout = 5 * time.Second

// waitForTarget polls addr with TCP dials until it accepts a connection or
// the deadline elapses; the proxy's error page is the fallback.
func waitForTarget(ctx context.Context, addr string, timeout time.Duration) {
	poll := time.NewTicker(targetProbeInterval)
	defer poll.Stop()
	deadline := time.Now().Add(timeout)
	for {
		if ctx.Err() != nil {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		remaining := time.Until(deadline)
		dialTimeout := 200 * time.Millisecond
		if remaining < dialTimeout {
			dialTimeout = remaining
		}
		c, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = c.Close()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
		}
	}
}

// handleFuncInvoke brings the function's sandbox up if needed and
// reverse-proxies the request to its handler. The path after ".../invoke" is
// forwarded ("/" for a bare invoke).
func (s *Server) handleFuncInvoke(w http.ResponseWriter, r *http.Request) {
	if s.functions == nil {
		writeErr(w, http.StatusNotFound, "function not found")
		return
	}
	name := r.PathValue("name")
	org := orgID(r)

	fn, ok, err := s.functions.Get(r.Context(), org, name)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "function not found")
		return
	}

	// AcquireReplica reaches daemon.Create without passing the create handler's
	// cap, so apply the plan's concurrency cap here.
	if plan, full := s.invokeWouldExceedPlan(r.Context(), org, name); full {
		w.Header().Set("Retry-After", "5")
		writeErr(w, http.StatusTooManyRequests, fmt.Sprintf(
			"plan '%s' allows %d concurrent sandboxes and this invoke needs another to start — pause one or upgrade",
			plan.ID, plan.MaxConcurrent))
		return
	}

	start := time.Now()
	id, cold, err := s.functions.AcquireReplica(r.Context(), org, name)
	if errors.Is(err, functions.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "function not found")
		return
	}
	if err != nil {
		w.Header().Set("Retry-After", "2")
		writeErr(w, http.StatusServiceUnavailable, "cold start failed: "+err.Error())
		return
	}
	defer s.functions.ReleaseReplica(org, name, id)

	target, err := s.functions.PreviewTarget(r.Context(), id, fn.Port, org)
	if err != nil {
		// ErrForeignOrg is the same 404 so another org's sandbox id is not leaked.
		if errors.Is(err, rt.ErrNotFound) || errors.Is(err, rt.ErrForeignOrg) {
			writeErr(w, http.StatusNotFound, "function not found")
			return
		}
		w.Header().Set("Retry-After", "2")
		writeErr(w, http.StatusServiceUnavailable, "sandbox unavailable: "+err.Error())
		return
	}

	if cold && target != "" {
		waitForTarget(r.Context(), target, coldStartReadyTimeout)
	}

	s.metrics.incFunctionInvoke(name)
	if cold {
		s.metrics.incFunctionColdStart(name)
	}
	s.functions.RecordInvoke(r.Context(), org, name, cold)

	rest := "/" + r.PathValue("path")
	w.Header().Set("X-Sandboxd-Cold-Start", strconv.FormatBool(cold))
	w.Header().Set("X-Sandboxd-Duration-Ms", strconv.FormatInt(time.Since(start).Milliseconds(), 10))

	errBody := fmt.Sprintf("function %q handler is not responding on port %d\n", name, fn.Port)
	proxy.Forward(w, r, target, rest, errBody)
}

// invokeWouldExceedPlan reports whether this invoke would need a new sandbox
// the plan has no room for. Only the cold path is gated: the cap rations
// machines, not requests.
func (s *Server) invokeWouldExceedPlan(ctx context.Context, org, name string) (entitlements.Plan, bool) {
	plan := s.orgPlan(ctx, org)
	if plan.MaxConcurrent <= 0 {
		return plan, false
	}
	if s.functions.ReplicaCount(org, name) > 0 {
		return plan, false
	}
	return plan, len(s.d.List(org)) >= plan.MaxConcurrent
}

// handleFuncLogs serves the logs of the function's current sandbox.
func (s *Server) handleFuncLogs(w http.ResponseWriter, r *http.Request) {
	fn, ok := s.lookupFunc(w, r)
	if !ok {
		return
	}
	if fn.SandboxID == "" {
		writeJSON(w, http.StatusOK, map[string]any{"lines": []string{}, "note": "function has no running sandbox yet — invoke it first"})
		return
	}
	s.serveSandboxLogs(w, r, fn.SandboxID)
}

const targetProbeInterval = 50 * time.Millisecond
