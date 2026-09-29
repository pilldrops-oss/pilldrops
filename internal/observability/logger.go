package observability

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
)

// NewLogger builds the process logger. format is "json" (default) or "text";
// level is debug|info|warn|error, default info.
func NewLogger(w io.Writer, format, level string) *slog.Logger {
	if w == nil {
		w = os.Stderr
	}
	opts := &slog.HandlerOptions{Level: ParseLevel(level)}
	var h slog.Handler
	if strings.EqualFold(format, "text") {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(&contextHandler{Handler: h})
}

// ParseLevel maps a level name to a slog.Level, defaulting to Info.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// contextHandler stamps the request and trace ids from ctx onto every record.
type contextHandler struct{ slog.Handler }

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestID(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	if tid := TraceID(ctx); tid != "" {
		r.AddAttrs(slog.String("trace_id", tid))
	}
	return h.Handler.Handle(ctx, r)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithGroup(name)}
}

// TeeHandler fans every record out to a second sink, the dashboard's SSE log pane.
type TeeHandler struct {
	slog.Handler
	// Publish receives the level and message only; the consumer is a human-facing pane.
	Publish func(level, msg string)
}

func (t *TeeHandler) Handle(ctx context.Context, r slog.Record) error {
	if t.Publish != nil {
		t.Publish(r.Level.String(), r.Message)
	}
	return t.Handler.Handle(ctx, r)
}

func (t *TeeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &TeeHandler{Handler: t.Handler.WithAttrs(attrs), Publish: t.Publish}
}

func (t *TeeHandler) WithGroup(name string) slog.Handler {
	return &TeeHandler{Handler: t.Handler.WithGroup(name), Publish: t.Publish}
}
