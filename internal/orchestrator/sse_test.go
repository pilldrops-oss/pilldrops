package orchestrator

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// readStatsEvent reads one stats event off the stream; a handler that never
// writes a frame would pass a status-only assertion.
func readStatsEvent(t *testing.T, srv *httptest.Server, auth string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/dashboard/api/events", nil)
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	if resp.StatusCode != 200 {
		t.Fatalf("stream = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	sc := bufio.NewScanner(resp.Body)
	sawEventLine := false
	for sc.Scan() {
		line := sc.Text()
		if line == "event: stats" {
			sawEventLine = true
			continue
		}
		if sawEventLine && strings.HasPrefix(line, "data: ") {
			var out map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &out); err != nil {
				t.Fatalf("stats payload is not JSON: %v", err)
			}
			return out
		}
	}
	t.Fatal("stream closed without delivering a stats event")
	return nil
}

func TestDashboardEventsStreamsFleetStats(t *testing.T) {
	s := fleetServer("apikey", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	registerNode(t, srv, `{"id":"n1","url":"http://n1","zone":"fsn1","capacity":{"cpu_cores":16,"mem_total_mb":64000,"mem_available_mb":24000,"mem_committed_mb":4000,"sandboxes_active":3}}`)
	registerNode(t, srv, `{"id":"n2","url":"http://n2","zone":"fsn1","capacity":{"cpu_cores":8,"mem_total_mb":32000,"mem_available_mb":8000,"mem_committed_mb":2000,"sandboxes_active":2}}`)

	got := readStatsEvent(t, srv, "apikey")

	if n := got["active_count"]; n != float64(5) {
		t.Errorf("active_count = %v, want 5 (3 on n1 + 2 on n2)", n)
	}

	// max_active is running plus what would still fit.
	wantHeadroom := (24000-cluster.FitMarginMB)/1024 + (8000-cluster.FitMarginMB)/1024
	if n := got["max_active"]; n != float64(5+wantHeadroom) {
		t.Errorf("max_active = %v, want %d (5 active + %d headroom)", n, 5+wantHeadroom, wantHeadroom)
	}

	if _, ok := got["pool_stats"]; !ok {
		t.Error("pool_stats missing — the client indexes into it")
	}
}

// Active counts every node; headroom counts only schedulable ones. Folding
// active into the schedulable branch passes every other test here.
func TestDashboardEventsCountsSandboxesOnCordonedNodes(t *testing.T) {
	s := fleetServer("apikey", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	registerNode(t, srv, `{"id":"n1","url":"http://n1","zone":"fsn1","capacity":{"cpu_cores":16,"mem_total_mb":64000,"mem_available_mb":24000,"mem_committed_mb":4000,"sandboxes_active":3}}`)
	registerNode(t, srv, `{"id":"n2","url":"http://n2","zone":"fsn1","capacity":{"cpu_cores":8,"mem_total_mb":32000,"mem_available_mb":8000,"mem_committed_mb":2000,"sandboxes_active":2}}`)

	if code, _ := getJSON(t, srv, "POST", "/v1/nodes/n2/cordon"); code != 200 {
		t.Fatalf("cordon = %d, want 200", code)
	}

	got := readStatsEvent(t, srv, "apikey")

	if n := got["active_count"]; n != float64(5) {
		t.Errorf("active_count = %v, want 5 — cordoning n2 must not hide the 2 sandboxes on it", n)
	}
	wantHeadroom := (24000 - cluster.FitMarginMB) / 1024
	if n := got["max_active"]; n != float64(5+wantHeadroom) {
		t.Errorf("max_active = %v, want %d — a cordoned node contributes no headroom", n, 5+wantHeadroom)
	}
}

// The stream and /v1/cluster/stats aggregate the registry separately.
func TestDashboardEventsAgreesWithClusterStats(t *testing.T) {
	s := fleetServer("apikey", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	registerNode(t, srv, `{"id":"n1","url":"http://n1","zone":"fsn1","capacity":{"cpu_cores":16,"mem_total_mb":64000,"mem_available_mb":24000,"mem_committed_mb":4000,"sandboxes_active":3}}`)

	code, cluster := getJSON(t, srv, "GET", "/v1/cluster/stats")
	if code != 200 {
		t.Fatalf("cluster/stats = %d", code)
	}
	ev := readStatsEvent(t, srv, "apikey")

	if ev["active_count"] != cluster["sandboxes_active"] {
		t.Errorf("active_count = %v but cluster/stats sandboxes_active = %v",
			ev["active_count"], cluster["sandboxes_active"])
	}
	wantMax := cluster["sandboxes_active"].(float64) + cluster["headroom_sandboxes"].(float64)
	if ev["max_active"] != wantMax {
		t.Errorf("max_active = %v, want %v (active + headroom from cluster/stats)", ev["max_active"], wantMax)
	}
}

func TestDashboardEventsRequiresAuth(t *testing.T) {
	s := fleetServer("apikey", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/dashboard/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	if resp.StatusCode != 401 {
		t.Fatalf("anonymous stream = %d, want 401", resp.StatusCode)
	}
}

func TestDashboardEventsOnEmptyFleet(t *testing.T) {
	s := fleetServer("apikey", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	got := readStatsEvent(t, srv, "apikey")
	if got["active_count"] != float64(0) || got["max_active"] != float64(0) {
		t.Fatalf("empty fleet = %v, want zeroes", got)
	}
}

// The CLI status command polls this.
func TestDashboardStatsSnapshotMatchesTheStream(t *testing.T) {
	s := fleetServer("apikey", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	registerNode(t, srv, `{"id":"n1","url":"http://n1","zone":"fsn1","capacity":{"cpu_cores":16,"mem_total_mb":64000,"mem_available_mb":24000,"mem_committed_mb":4000,"sandboxes_active":3}}`)

	code, snap := getJSON(t, srv, "GET", "/dashboard/api/stats")
	if code != 200 {
		t.Fatalf("/dashboard/api/stats = %d, want 200", code)
	}
	// The CLI indexes both of these unguarded.
	for _, k := range []string{"active_count", "max_active"} {
		if _, ok := snap[k]; !ok {
			t.Errorf("snapshot missing %q — the CLI reads it unguarded", k)
		}
	}

	ev := readStatsEvent(t, srv, "apikey")
	if snap["active_count"] != ev["active_count"] || snap["max_active"] != ev["max_active"] {
		t.Errorf("snapshot %v disagrees with the stream %v", snap, ev)
	}
}

func TestDashboardStatsRequiresAuth(t *testing.T) {
	s := fleetServer("apikey", "nodetoken")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()

	req, _ := http.NewRequestWithContext(t.Context(), "GET", srv.URL+"/dashboard/api/stats", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	if resp.StatusCode != 401 {
		t.Fatalf("anonymous stats = %d, want 401", resp.StatusCode)
	}
}
