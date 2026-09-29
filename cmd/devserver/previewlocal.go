package main

// Local preview readiness, the single-box counterpart of the fleet probe.
// States mirror internal/httpapi/previewstatus.go; a reaped crash is
// app_error, and a clean exit or no app at all is terminal.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/httpx"
)

// localPreviewClient never follows redirects; a 301 already proves the app answered.
var localPreviewClient = &http.Client{
	// Not http.DefaultTransport: it is shared process-wide, and this probes
	// arbitrary local ports that may be filtered rather than closed.
	Transport:     httpx.NewFanoutTransport(),
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// localPreviewProbe performs one bounded GET against the app's local port; a
// hung app costs 2.5s, not a goroutine.
func localPreviewProbe(ctx context.Context, port int) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://localhost:%d/", port), nil)
	if err != nil {
		return 0, err
	}
	resp, err := localPreviewClient.Do(req) //nolint:bodyclose // closed by closeQuietly below
	if err != nil {
		return 0, err
	}
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)); err != nil {
		// Drain so the connection can be reused; only the status code matters.
		_ = err
	}
	closeQuietly(resp.Body, "localPreviewProbe")
	return resp.StatusCode, nil
}

// localPreviewState classifies preview readiness. probe runs only when an app
// is running; hasUpstream is true only when a probe completed.
func localPreviewState(status string, appStarted bool, probe func() (int, error)) (state string, upstream int, hasUpstream bool) {
	switch status {
	case "paused":
		return "paused", 0, false
	case "crashed":
		// The reaper saw the start command die; nothing will answer.
		return "app_error", 0, false
	case "exited":
		// A clean exit leaves no app; the preview serves the built-in surface.
		return "terminal", 0, false
	}
	if !appStarted {
		// No start command ever ran; a 2xx from the built-in terminal would be
		// a false green.
		return "terminal", 0, false
	}
	code, err := probe()
	if err != nil {
		// Indistinguishable from still booting; the caller's deadline decides.
		return "starting", 0, false
	}
	switch {
	case code == http.StatusNotFound:
		return "no_page", code, true
	case code == http.StatusBadGateway, code == http.StatusServiceUnavailable, code == http.StatusGatewayTimeout:
		return "starting", code, true
	case code >= 500:
		return "app_error", code, true
	default:
		// 4xx other than 404 counts as ready: an app returning 401/403 is answering.
		return "ready", code, true
	}
}
