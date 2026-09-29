package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func TestParseLevel(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug}, {"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo}, {"", slog.LevelInfo},
		{"warn", slog.LevelWarn}, {"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{" WARN ", slog.LevelWarn},
		{"nonsense", slog.LevelInfo},
	} {
		in, want := tc.in, tc.want
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNewLoggerFormats(t *testing.T) {
	var buf bytes.Buffer
	NewLogger(&buf, "json", "info").Info("hello", "k", "v")
	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("json format did not produce JSON: %v (%q)", err, buf.String())
	}
	if rec["msg"] != "hello" || rec["k"] != "v" {
		t.Errorf("record = %v, want msg=hello k=v", rec)
	}

	buf.Reset()
	NewLogger(&buf, "text", "info").Info("hello", "k", "v")
	if out := buf.String(); !strings.Contains(out, "msg=hello") || !strings.Contains(out, "k=v") {
		t.Errorf("text format = %q", out)
	}
}

func TestNewLoggerRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	lg := NewLogger(&buf, "json", "warn")
	lg.Info("suppressed")
	lg.Debug("suppressed")
	if buf.Len() != 0 {
		t.Errorf("info/debug emitted at warn level: %q", buf.String())
	}
	lg.Warn("kept")
	if !strings.Contains(buf.String(), "kept") {
		t.Error("warn was suppressed at warn level")
	}
}

func TestLoggerStampsTheRequestIDFromContext(t *testing.T) {
	var buf bytes.Buffer
	lg := NewLogger(&buf, "json", "info")
	ctx := WithRequestID(context.Background(), "req_from_ctx")

	lg.InfoContext(ctx, "something happened", "sandbox_id", "sb-1")

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rec["request_id"] != "req_from_ctx" {
		t.Errorf("request_id = %v, want req_from_ctx — the id must come from the context, not the call site", rec["request_id"])
	}
	if rec["sandbox_id"] != "sb-1" {
		t.Errorf("explicit attrs must survive: %v", rec)
	}
}

func TestLoggerOmitsRequestIDWhenAbsent(t *testing.T) {
	var buf bytes.Buffer
	NewLogger(&buf, "json", "info").Info("no ctx id")
	if strings.Contains(buf.String(), "request_id") {
		t.Errorf("request_id present with no id in context: %q", buf.String())
	}
}

// With and WithGroup return new handlers, so the wrapper must re-wrap.
func TestContextHandlerSurvivesWithAttrsAndGroup(t *testing.T) {
	var buf bytes.Buffer
	base := NewLogger(&buf, "json", "info")
	ctx := WithRequestID(context.Background(), "req_derived")

	base.With("component", "daemon").InfoContext(ctx, "derived logger")
	if !strings.Contains(buf.String(), "req_derived") {
		t.Errorf("request_id lost through With(): %q", buf.String())
	}

	buf.Reset()
	base.WithGroup("g").InfoContext(ctx, "grouped logger")
	if !strings.Contains(buf.String(), "req_derived") {
		t.Errorf("request_id lost through WithGroup(): %q", buf.String())
	}
}

func TestTeeHandlerFansOutToBothSinks(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	var published [][2]string

	base := NewLogger(&buf, "json", "info")
	lg := slog.New(&TeeHandler{
		Handler: base.Handler(),
		Publish: func(level, msg string) {
			mu.Lock()
			published = append(published, [2]string{level, msg})
			mu.Unlock()
		},
	})

	lg.Info("sandbox paused", "sandbox_id", "sb-1")

	if !strings.Contains(buf.String(), "sandbox paused") {
		t.Errorf("primary sink missed the record: %q", buf.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(published) != 1 || published[0][1] != "sandbox paused" || published[0][0] != "INFO" {
		t.Errorf("secondary sink got %v, want one INFO/sandbox paused", published)
	}
}

func TestTeeHandlerToleratesNilPublish(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(&TeeHandler{Handler: NewLogger(&buf, "json", "info").Handler()})
	lg.Info("no publisher")
	if !strings.Contains(buf.String(), "no publisher") {
		t.Error("primary sink missed the record")
	}
}

func TestTeeHandlerSurvivesWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	var got int
	lg := slog.New(&TeeHandler{
		Handler: NewLogger(&buf, "json", "info").Handler(),
		Publish: func(string, string) { got++ },
	})
	lg.With("component", "daemon").Info("derived")
	if got != 1 {
		t.Errorf("secondary sink received %d records through With(), want 1", got)
	}
}
