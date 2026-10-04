package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cron"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// The floor is measured from two consecutive fires, so a period and the cron
// expression that means the same thing are refused the same way. Spelling the
// rule against the text instead would let "*/5 * * * *" through.
func TestBackupScheduleFloorIsMeasuredFromTheFires(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		expr    string
		refused bool
	}{
		{"*/5 * * * *", true},
		{"*/14 * * * *", true},
		{"*/15 * * * *", false},
		{"*/30 * * * *", false},
		{"0 * * * *", false},
		{"0 0 * * *", false},
	} {
		err := scheduleGapError(tc.expr, from)
		if tc.refused && !errors.Is(err, ErrScheduleTooFrequent) {
			t.Errorf("%q = %v, want a refusal naming the floor", tc.expr, err)
		}
		if !tc.refused && err != nil {
			t.Errorf("%q = %v, want accepted", tc.expr, err)
		}
	}
}

func TestBackupPeriodBecomesACronExpression(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, tc := range []struct{ period, want string }{
		{"24h", "0 0 * * *"},
		{"6h", "0 */6 * * *"},
		{"15m", "*/15 * * * *"},
		{"30m", "*/30 * * * *"},
	} {
		got, err := cronForPeriod(tc.period)
		if err != nil || got != tc.want {
			t.Errorf("cronForPeriod(%q) = %q, %v; want %q", tc.period, got, err, tc.want)
		}
		// Whatever the period produced must itself clear the floor, or the two
		// checks disagree and one of them is decoration.
		if err := scheduleGapError(got, from); err != nil {
			t.Errorf("cronForPeriod(%q) produced %q, which the floor then refuses: %v", tc.period, got, err)
		}
	}

	// A 24h period's next two fires really are 24 h apart.
	expr, _ := cronForPeriod("24h")
	sched, perr := cron.Parse(expr)
	if perr != nil {
		t.Fatal(perr)
	}
	first := sched.Next(from)
	if gap := sched.Next(first).Sub(first); gap != 24*time.Hour {
		t.Errorf("a 24h period fires every %s", gap)
	}

	for _, bad := range []string{"5m", "90m", "36h", "nonsense", ""} {
		if _, err := cronForPeriod(bad); err == nil {
			t.Errorf("cronForPeriod(%q) was accepted", bad)
		}
	}
}

func TestBackupRetentionParsing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{"", 0, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"48h", 48 * time.Hour, false},
		{"0d", 0, true},
		{"soon", 0, true},
		{"-3d", 0, true},
	} {
		got, err := parseRetention(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("parseRetention(%q) = %v, want an error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("parseRetention(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
}

// Retention alone has nothing to apply to: nothing creates a backup to expire.
func TestBackupRetentionWithoutAPeriodIsRefused(t *testing.T) {
	srv, key := newTestServer(t)
	id := createSandbox(t, srv.URL, key)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/config", key,
		`{"backups":{"retention":"7d"}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("retention with no period = %d, want 400 (%v)", code, out)
	}
	if msg := strings.ToLower(fmt.Sprint(out["error"])); !strings.Contains(msg, "period") {
		t.Errorf("the refusal must say what is missing, got %q", msg)
	}
}

// nullBlobs is an object store that exists. Nothing here uploads anything; a
// schedule only has to know the node has somewhere to put a backup.
type nullBlobs struct{}

func (nullBlobs) Put(context.Context, string, io.Reader, int64) error { return nil }
func (nullBlobs) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("no blob")
}
func (nullBlobs) Delete(context.Context, string) error { return nil }
func (nullBlobs) Name() string                         { return "null" }

// withBackups gives a test node the two tiers a backup needs.
func withBackups(cfg *daemon.Config, st *store.Store) {
	cfg.Backups, cfg.Blobs = st, nullBlobs{}
}

// A node with nowhere to put a backup answers every backup verb 501, and its
// own startup log says every schedule on it fails every fire. Accepting one
// anyway, with a 200 and enabled:true, is the answer a caller cannot act on:
// it reads as backups being configured when nothing about them works.
func TestBackupScheduleIsRefusedWhereBackupsCannotRun(t *testing.T) {
	srv, key := newTestServer(t)
	id := createSandbox(t, srv.URL, key)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/config", key,
		`{"backups":{"period":"24h"}}`)
	if code != http.StatusNotImplemented {
		t.Fatalf("24h period on a node with no object store = %d, want 501 (%v)", code, out)
	}
	if out["code"] != "backups_need_object_store" {
		t.Errorf("code = %v, want the same one `backup create` answers with", out["code"])
	}
	// And nothing was written, so no sweeper has a job to fail on.
	_, cfg := do(t, "GET", srv.URL+"/v1/sandboxes/"+id+"/config", key, "")
	if cfg["backups"] != nil {
		t.Fatalf("a refused schedule was stored anyway: %v", cfg["backups"])
	}
}

// A schedule under the floor is refused over HTTP with a code, and nothing is
// written: the env in the same body must not land while the schedule is refused.
func TestBackupScheduleOverHTTP(t *testing.T) {
	srv, _, key := newTestServerWithStore(t, withBackups)
	id := createSandbox(t, srv.URL, key)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/config", key,
		`{"env":{"SHOULD_NOT_LAND":"1"},"backups":{"period":"5m"}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("5m period = %d, want 400 (%v)", code, out)
	}
	if out["code"] != "invalid_backup_schedule" {
		t.Errorf("code = %v, want invalid_backup_schedule", out["code"])
	}
	if msg := fmt.Sprint(out["error"]); !strings.Contains(msg, "15m") {
		t.Errorf("the refusal must name the floor, got %q", msg)
	}
	_, cfg := do(t, "GET", srv.URL+"/v1/sandboxes/"+id+"/config", key, "")
	if strings.Contains(fmt.Sprint(cfg["env"]), "SHOULD_NOT_LAND") {
		t.Error("the refused request still wrote the environment; a refusal must change nothing")
	}

	// A legal one is stored and readable back.
	if code, out := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/config", key,
		`{"backups":{"period":"24h","retention":"7d"}}`); code != http.StatusOK {
		t.Fatalf("24h period = %d (%v)", code, out)
	}
	_, cfg = do(t, "GET", srv.URL+"/v1/sandboxes/"+id+"/config", key, "")
	sched, _ := cfg["backups"].(map[string]any)
	if sched == nil || sched["cron"] != "0 0 * * *" {
		t.Fatalf("schedule reads %v, want a daily cron", cfg["backups"])
	}
	if next, _ := sched["next_run_at"].(float64); next <= 0 {
		t.Error("the job has no next run, so it would never fire")
	}

	// And "off" removes it.
	if code, out := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/config", key,
		`{"backups":{"period":"off"}}`); code != http.StatusOK {
		t.Fatalf("off = %d (%v)", code, out)
	}
	_, cfg = do(t, "GET", srv.URL+"/v1/sandboxes/"+id+"/config", key, "")
	if cfg["backups"] != nil {
		t.Fatalf("the schedule survived being turned off: %v", cfg["backups"])
	}
}
