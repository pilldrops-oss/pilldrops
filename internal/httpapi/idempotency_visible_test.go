package httpapi

import (
	"bytes"
	"database/sql"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// A create refused over its Idempotency-Key must name the cause, in the log and
// in the body. Against the old code this fails twice: nothing was logged at all,
// and the body said only "idempotency check failed", which is what made an
// intermittent store fault read as a flaky rig for the length of a test suite.
func TestAnIdempotencyRefusalNamesItsCause(t *testing.T) {
	var dataDir string
	srv, _, key := newTestServerWithStore(t, func(cfg *daemon.Config, _ *store.Store) {
		dataDir = filepath.Dir(cfg.SnapshotDir) // the tempdir holding state.db
	})
	defer srv.Close()

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Break the reservation statement and nothing else, so the request still
	// authenticates and fails exactly where the node's own failures land.
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(), `DROP TABLE idempotency`); err != nil {
		t.Fatalf("drop the idempotency table: %v", err)
	}

	code, out := createWithIdemKey(t, srv.URL+"/v1/sandboxes", key,
		`{"template":"python-3.11"}`, "customer-key-1")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", code)
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "Idempotency-Key") || len(msg) <= len("idempotency check failed") {
		t.Errorf("body %q does not say which request was refused or why", msg)
	}
	line := logs.String()
	if !strings.Contains(line, "Idempotency-Key could not be claimed") {
		t.Fatalf("nothing was logged for the refusal; log was:\n%s", line)
	}
	if !strings.Contains(line, "customer-key-1") || !strings.Contains(line, "err=") {
		t.Errorf("the log line names neither the key nor the underlying error:\n%s", line)
	}
}
