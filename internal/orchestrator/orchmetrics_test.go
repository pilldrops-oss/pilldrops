package orchestrator

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func scrape(t *testing.T, s *Server) string {
	t.Helper()
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("scrape: got %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// Every family must be present in a cold scrape, or a dashboard shows "no data".
func TestScrapeExposesTheControlPlaneFamilies(t *testing.T) {
	s := NewServer("k", "t")
	body := scrape(t, s)
	for _, want := range []string{
		"orchestrator_uptime_seconds",
		"orchestrator_nodes",
		"orchestrator_admissions_total",
		"orchestrator_schedules_total",
		"orchestrator_node_forwards_total",
		"orchestrator_node_forward_duration_seconds",
		"orchestrator_auth_cache_total",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape is missing %s\n---\n%s", want, body)
		}
	}
}

// The scrape names every node, so it is not public health.
func TestScrapeRequiresAuth(t *testing.T) {
	s := NewServer("k", "t")
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("unauthenticated scrape: got %d, want 401", resp.StatusCode)
	}
}

func TestAdmissionOutcomesAreDistinguished(t *testing.T) {
	s := NewServer("k", "t")
	s.metrics.recordAdmission(admitAllowed)
	s.metrics.recordAdmission(admitDenied)
	s.metrics.recordAdmission(admitUnavailable)
	s.metrics.recordAdmission(admitUnavailable)

	body := scrape(t, s)
	for _, want := range []string{
		`orchestrator_admissions_total{outcome="allowed"} 1`,
		`orchestrator_admissions_total{outcome="denied"} 1`,
		`orchestrator_admissions_total{outcome="unavailable"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q\n---\n%s", want, body)
		}
	}
}

func TestScheduleOutcomesSeparateCapacityFromCapability(t *testing.T) {
	s := NewServer("k", "t")
	s.metrics.recordSchedule(schedPlaced)
	s.metrics.recordSchedule(schedNoCapacity)
	s.metrics.recordSchedule(schedNoTemplate)

	body := scrape(t, s)
	for _, want := range []string{
		`orchestrator_schedules_total{outcome="placed"} 1`,
		`orchestrator_schedules_total{outcome="no_capacity"} 1`,
		`orchestrator_schedules_total{outcome="no_template"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q\n---\n%s", want, body)
		}
	}
}

func TestForwardsAreCountedPerNode(t *testing.T) {
	s := NewServer("k", "t")
	s.metrics.recordForward("node-1", 0.01, nil)
	s.metrics.recordForward("node-1", 0.01, nil)
	s.metrics.recordForward("node-2", 2.0, http.ErrHandlerTimeout)

	body := scrape(t, s)
	for _, want := range []string{
		`orchestrator_node_forwards_total{node="node-1",outcome="ok"} 2`,
		`orchestrator_node_forwards_total{node="node-2",outcome="error"} 1`,
		"orchestrator_node_forward_duration_seconds_count 3",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q\n---\n%s", want, body)
		}
	}
}
