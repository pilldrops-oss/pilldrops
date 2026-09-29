package orchestrator

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/observability"
)

func TestRequestPathLogsCarryTheRequestID(t *testing.T) {
	var buf bytes.Buffer
	s := NewServer("apikey", "nodetoken")
	s.logger = observability.NewLogger(&buf, "json", "debug")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	body := `{"kind":"bug","name":"Ada","email":"ada@example.com","message":"the widget is upside down"}`
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/feedback", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer apikey")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(observability.HeaderRequestID, "req_correlate_me")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("post feedback: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/feedback = %d, want 200", resp.StatusCode)
	}

	rec := findLogRecord(t, buf.Bytes(), "feedback received")
	if rec == nil {
		t.Fatalf("no structured record for the handler; log was:\n%s", buf.String())
	}
	if got := rec["request_id"]; got != "req_correlate_me" {
		t.Errorf("request_id = %v, want req_correlate_me — without it this line "+
			"cannot be tied to the request that produced it", got)
	}
	// "kind" must be its own queryable field.
	if got := rec["kind"]; got != "bug" {
		t.Errorf("kind = %v, want bug as its own field", got)
	}
	if got := rec["component"]; got != "feedback" {
		t.Errorf("component = %v, want feedback", got)
	}
}

// An always-present but usually-blank request_id trains everyone to ignore it.
func TestNonRequestLogsOmitTheRequestID(t *testing.T) {
	var buf bytes.Buffer
	log := observability.NewLogger(&buf, "json", "debug")
	log.Info("started", "component", "orchestrator")

	rec := findLogRecord(t, buf.Bytes(), "started")
	if rec == nil {
		t.Fatalf("no record; log was:\n%s", buf.String())
	}
	if _, present := rec["request_id"]; present {
		t.Errorf("request_id present with no request in scope: %v", rec)
	}
}

func findLogRecord(t *testing.T, out []byte, msg string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %v\n  %s", err, line)
		}
		if rec["msg"] == msg {
			return rec
		}
	}
	return nil
}
