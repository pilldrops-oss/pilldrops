package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
)

// Preview readiness states returned by GET /v1/sandboxes/{id}/preview-status.
// They report what the preview URL would serve, not the sandbox lifecycle state.
const (
	previewReady    = "ready"     // app answered
	previewStarting = "starting"  // route live, app not listening yet
	previewNoPage   = "no_page"   // app is serving but has nothing at /
	previewAppError = "app_error" // app answered with a 5xx of its own
	// previewTerminal: no app is registered, so the preview serves the web terminal.
	previewTerminal = "terminal"
	// previewPaused is reported without resolving a target, since resolution
	// would wake (and bill) the sandbox.
	previewPaused = "paused"
	// previewUnpublished: the runtime cannot serve this port at all.
	previewUnpublished = "unpublished"
)

// classifyPreview maps an upstream response to a readiness state. A transport
// error is indistinguishable from booting, so it is "starting". A 401/403 is
// an app answering, so ready; a 500 is up and broken, not starting.
func classifyPreview(status int, transportErr bool) string {
	if transportErr {
		return previewStarting
	}
	switch {
	case status == http.StatusNotFound:
		return previewNoPage
	case status == http.StatusBadGateway,
		status == http.StatusServiceUnavailable,
		status == http.StatusGatewayTimeout:
		return previewStarting
	case status >= 500:
		return previewAppError
	default:
		return previewReady
	}
}

// previewProbeClient never follows redirects; a 3xx is already an answer and
// following it could leave the sandbox.
var previewProbeClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// probePreviewHTTP performs one GET against the preview target. Bounded
// tightly because the dashboard polls it.
func probePreviewHTTP(ctx context.Context, target string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+target+"/", nil)
	if err != nil {
		return 0, err
	}
	resp, err := previewProbeClient.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	return resp.StatusCode, nil
}

// handlePreviewStatus probes the same target the /preview proxy dials.
//
//	GET /v1/sandboxes/{id}/preview-status?port=8000
//	200 {"state":"starting","sandbox":"sb1","port":8000,"upstream_status":503}
//
// The HTTP code says whether the probe could run; callers branch on state.
func (s *Server) handlePreviewStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sb, ok := s.d.Get(id)
	// owns, not an org comparison: same rule as every other read of a sandbox,
	// so a private one is not observable by a key that may not see it.
	if !ok || !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}

	port := sb.PublicPort
	if q := r.URL.Query().Get("port"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 && n < 65536 {
			port = n
		}
	}
	if port == 0 {
		port = 8000
	}

	out := map[string]any{"sandbox": id, "port": port}
	respond := func(state string) {
		out["state"] = state
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, out)
	}

	// PreviewTarget wakes paused sandboxes; a status poll must never resume one.
	if sb.State == daemon.StatePaused || sb.State == daemon.StatePausing || sb.State == daemon.StateResuming {
		respond(previewPaused)
		return
	}

	if sb.StartCmd == "" && sb.PublicPort == 0 {
		respond(previewTerminal)
		return
	}

	// orgID(r), not sb.OrgID, so the daemon's own org check is not tautological.
	target, err := s.d.PreviewTarget(r.Context(), id, port, orgID(r))
	if err != nil {
		switch {
		case errors.Is(err, rt.ErrNotFound), errors.Is(err, rt.ErrForeignOrg):
			writeErr(w, http.StatusNotFound, "sandbox not found")
		case errors.Is(err, rt.ErrNoAppOnPort):
			respond(previewUnpublished)
		default:
			// Transient resolution failure reads the same as not up yet.
			respond(previewStarting)
		}
		return
	}

	status, perr := s.probePreview(r.Context(), target)
	if perr == nil {
		out["upstream_status"] = status
	}
	respond(classifyPreview(status, perr != nil))
}
