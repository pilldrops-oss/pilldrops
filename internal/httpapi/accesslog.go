package httpapi

import (
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// accessLogLevel is Debug by default; set SANDBOXD_ACCESS_LOG=1 to see every request.
var accessLogLevel = func() slog.Level {
	if envBool("SANDBOXD_ACCESS_LOG", false) {
		return slog.LevelInfo
	}
	return slog.LevelDebug
}()

// accessLog records one completed request. It logs the route pattern, not the
// raw path, so lines group and customer ids stay out of the log.
func (s *Server) accessLog(r *http.Request, status int, dur time.Duration, requestID string) {
	level := accessLogLevel
	if status >= http.StatusInternalServerError {
		// Always log server errors, even with the access log off.
		level = slog.LevelWarn
	}
	ctx := r.Context()
	if !slog.Default().Enabled(ctx, level) {
		return
	}
	attrs := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("route", routePattern(r)),
		slog.Int("status", status),
		slog.Int64("duration_ms", dur.Milliseconds()),
	}
	if requestID != "" {
		attrs = append(attrs, slog.String("request_id", requestID))
	}
	if org := orgID(r); org != "" {
		attrs = append(attrs, slog.String("org_id", org))
	}
	slog.Default().LogAttrs(ctx, level, "http_request", attrs...)
}

// routePattern returns the matched route pattern, or the raw path on a 404.
func routePattern(r *http.Request) string {
	if p := r.Pattern; p != "" {
		// Pattern is "METHOD /path"; the method is already its own field.
		if _, path, ok := strings.Cut(p, " "); ok {
			return path
		}
		return p
	}
	return r.URL.Path
}
