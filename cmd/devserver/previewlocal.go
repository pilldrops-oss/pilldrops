package main

// Local preview readiness, the single-box counterpart of the fleet probe.
// States mirror internal/httpapi/previewstatus.go; a reaped crash is
// app_error, and a clean exit or no app at all is terminal.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/pilldrops-oss/pilldrops/internal/auth/sbtoken"
	"github.com/pilldrops-oss/pilldrops/internal/httpx"
)

// envdTerminalPort is the envd web terminal the dashboard's Terminal tab opens
// as a preview port; locally one envd serves every sandbox.
const envdTerminalPort = "7681"

// localPreview serves /preview/{id}/{port}/... on the local runtime. It only
// ever reaches the sandbox's own app port or, for its owner, the terminal: an
// unknown id or another port is not a way into arbitrary localhost services.
func localPreview(mgr *DevManager, ownerID func(*http.Request) string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sb, ok := mgr.GetSandbox(chi.URLParam(r, "id"))
		if !ok || sb.Port <= 0 {
			http.Error(w, "Sandbox not found", http.StatusNotFound)
			return
		}
		target := fmt.Sprintf("localhost:%d", sb.Port)
		terminal := chi.URLParam(r, "port") == envdTerminalPort
		if terminal {
			// A shell, so only its owner.
			if sb.UserID != ownerID(r) {
				http.Error(w, "Sandbox not found", http.StatusNotFound)
				return
			}
			target = "localhost:" + envdTerminalPort
		}
		rest := "/" + chi.URLParam(r, "*")
		rp := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.URL.Scheme, pr.Out.URL.Host = "http", target
				pr.Out.URL.Path, pr.Out.URL.RawPath = rest, ""
				// envd's websocket origin check compares Origin with Host.
				pr.Out.Host = pr.In.Host
				if terminal {
					pr.Out.Header.Set(sbtoken.MetadataKey, mgr.envdToken)
					q := pr.Out.URL.Query()
					if q.Get("cwd") == "" {
						q.Set("cwd", sb.WorkDir)
					}
					pr.Out.URL.RawQuery = q.Encode()
				}
			},
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
				msg := fmt.Sprintf("Preview not available — start a web server in the terminal first.\n\nTry: python3 -m http.server %d", sb.Port)
				if terminal {
					msg = "Terminal not available — the local envd is not running."
				}
				http.Error(w, msg, http.StatusBadGateway)
			},
		}
		rp.ServeHTTP(w, r)
	})
}

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
