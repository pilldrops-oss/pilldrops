//go:build !core

package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/extras/agent/agentd"
)

func TestPrometheusMetricsEndpoint(t *testing.T) {
	srv, key := newTestServer(t)

	// This anonymous probe counts as one auth failure below.
	if code, _ := getText(t, srv.URL+"/metrics", ""); code != 401 {
		t.Fatalf("anonymous /metrics = %d, want 401", code)
	}

	code, body := getText(t, srv.URL+"/metrics", key)
	if code != 200 {
		t.Fatalf("/metrics = %d, want 200", code)
	}
	for _, want := range []string{
		"# TYPE sandboxd_sandbox_creates_total counter",
		"sandboxd_active_sandboxes",
		"sandboxd_uptime_seconds",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/metrics missing %q\n%s", want, body)
		}
	}

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"python-3.11"}`)
	if code != 201 {
		t.Fatalf("create = %d (%v)", code, out)
	}
	id, _ := out["id"].(string)
	if code, _ := do(t, "POST", srv.URL+"/v1/sandboxes/"+id+"/exec", key, `{"cmd":"true"}`); code != 200 {
		t.Fatalf("exec = %d", code)
	}
	if code, _ := getText(t, srv.URL+"/v1/sandboxes", "nope"); code != 401 {
		t.Fatalf("bad auth = %d, want 401", code)
	}

	_, body = getText(t, srv.URL+"/metrics", key)
	for _, want := range []string{
		"sandboxd_sandbox_creates_total 1",
		"sandboxd_exec_total 1",
		"sandboxd_auth_failures_total 2",
		`sandboxd_sandboxes{state="running"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/metrics missing %q after activity\n%s", want, body)
		}
	}
}

func TestPerSandboxMetrics(t *testing.T) {
	srv, key := newTestServer(t)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"python-3.11"}`)
	if code != 201 {
		t.Fatalf("create = %d (%v)", code, out)
	}
	id, _ := out["id"].(string)

	code, m := do(t, "GET", srv.URL+"/v1/sandboxes/"+id+"/metrics", key, "")
	if code != 200 {
		t.Fatalf("sandbox metrics = %d (%v)", code, m)
	}
	if m["state"] != "running" {
		t.Fatalf("state = %v, want running", m["state"])
	}
	// python-3.11 resolves to the code-interpreter catalog entry: 2 vCPU, 1024 MiB.
	if v, _ := m["vcpu"].(float64); v != 2 {
		t.Fatalf("vcpu = %v, want 2", m["vcpu"])
	}
	if v, _ := m["mem_mib"].(float64); v != 1024 {
		t.Fatalf("mem_mib = %v, want 1024", m["mem_mib"])
	}
	if _, ok := m["vcpu_seconds"]; !ok {
		t.Fatalf("missing vcpu_seconds: %v", m)
	}
	if _, ok := m["uptime_seconds"]; !ok {
		t.Fatalf("missing uptime_seconds: %v", m)
	}

	if code, _ := do(t, "GET", srv.URL+"/v1/sandboxes/sb_missing/metrics", key, ""); code != 404 {
		t.Fatalf("metrics for missing sandbox = %d, want 404", code)
	}
}

func sandboxMetric(t *testing.T, url, key, id string) map[string]any {
	t.Helper()
	code, m := do(t, "GET", url+"/v1/sandboxes/"+id+"/metrics", key, "")
	if code != 200 {
		t.Fatalf("metrics = %d (%v)", code, m)
	}
	return m
}

func TestCreateResourceSelection(t *testing.T) {
	srv, key := newTestServer(t)

	_, out := do(t, "POST", srv.URL+"/v1/sandboxes", key,
		`{"template":"base","vcpu":4,"memory_mb":2048,"storage_mb":8192}`)
	id, _ := out["id"].(string)
	m := sandboxMetric(t, srv.URL, key, id)
	if v, _ := m["vcpu"].(float64); v != 4 {
		t.Fatalf("explicit vcpu = %v, want 4", m["vcpu"])
	}
	if v, _ := m["mem_mib"].(float64); v != 2048 {
		t.Fatalf("explicit mem_mib = %v, want 2048", m["mem_mib"])
	}

	_, l := do(t, "GET", srv.URL+"/v1/sandboxes", key, "")
	sbs, _ := l["sandboxes"].([]any)
	var storage float64
	for _, s := range sbs {
		if sm, _ := s.(map[string]any); sm["id"] == id {
			storage, _ = sm["storage_mb"].(float64)
		}
	}
	if storage != 8192 {
		t.Fatalf("storage_mb = %v, want 8192", storage)
	}

	// base template: 1 vCPU / 512 MiB.
	_, out2 := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	id2, _ := out2["id"].(string)
	m2 := sandboxMetric(t, srv.URL, key, id2)
	if v, _ := m2["vcpu"].(float64); v != 1 {
		t.Fatalf("default vcpu = %v, want 1 (base template)", m2["vcpu"])
	}

	_, out3 := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base","vcpu":9999}`)
	id3, _ := out3["id"].(string)
	m3 := sandboxMetric(t, srv.URL, key, id3)
	if v, _ := m3["vcpu"].(float64); v != 16 {
		t.Fatalf("clamped vcpu = %v, want 16", m3["vcpu"])
	}
}

func TestFunctionCountersExposed(t *testing.T) {
	m := newMetrics()
	m.incFunctionInvoke("hello")
	m.incFunctionInvoke("hello")
	m.incFunctionColdStart("hello")

	var b strings.Builder
	m.writeFunctionCounters(&b)
	out := b.String()
	if !strings.Contains(out, `sandboxd_function_invokes_total{name="hello"} 2`) {
		t.Fatalf("invokes missing: %s", out)
	}
	if !strings.Contains(out, `sandboxd_function_cold_starts_total{name="hello"} 1`) {
		t.Fatalf("cold starts missing: %s", out)
	}
}

func TestAgentMetricsAreLabelledPerAgent(t *testing.T) {
	m := newMetrics()
	m.incAgentTurn("claude", "accepted")
	m.incAgentTurn("claude", "accepted")
	m.incAgentTurn("codex", "refused")
	m.incAgentInterrupt("claude")
	m.incAgentUnreachable("grok")
	m.incAgentSpendStop("opencode")

	var b strings.Builder
	m.writeAgentMetrics(&b)
	out := b.String()

	for _, want := range []string{
		`sandboxd_agent_turns_total{agent="claude",result="accepted"} 2`,
		`sandboxd_agent_turns_total{agent="codex",result="refused"} 1`,
		`sandboxd_agent_interrupts_total{agent="claude"} 1`,
		`sandboxd_agent_supervisor_unreachable_total{agent="grok"} 1`,
		`sandboxd_agent_spend_stops_total{agent="opencode"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing series:\n  %s\ngot:\n%s", want, out)
		}
	}
	// Without a TYPE line Prometheus treats the series as untyped.
	for _, name := range []string{
		"sandboxd_agent_turns_total", "sandboxd_agent_interrupts_total",
		"sandboxd_agent_supervisor_unreachable_total", "sandboxd_agent_spend_stops_total",
	} {
		if !strings.Contains(out, "# TYPE "+name+" counter") {
			t.Errorf("%s has no TYPE line", name)
		}
	}
}

func TestUnreachableSupervisorIsCountedApartFromARefusedTurn(t *testing.T) {
	s := &Server{metrics: newMetrics()}

	s.countAgentFailure("claude", agentd.ErrAgentUnreachable)
	s.countAgentFailure("claude", agentd.ErrSpendCapReached)
	s.countAgentFailure("claude", errors.New("something else"))
	// A 402 arriving as a StatusError is still a spend stop.
	s.countAgentFailure("claude", &agentd.StatusError{Code: http.StatusPaymentRequired, Message: "cap"})

	var b strings.Builder
	s.metrics.writeAgentMetrics(&b)
	out := b.String()

	if !strings.Contains(out, `sandboxd_agent_supervisor_unreachable_total{agent="claude"} 1`) {
		t.Error("unreachable not counted")
	}
	if !strings.Contains(out, `sandboxd_agent_spend_stops_total{agent="claude"} 2`) {
		t.Errorf("spend stops not counted twice (direct + StatusError):\n%s", out)
	}
	if !strings.Contains(out, `sandboxd_agent_turns_total{agent="claude",result="refused"} 1`) {
		t.Error("an unclassified failure should count as a refused turn")
	}
}

func TestApprovalFrictionIsCountedSeparately(t *testing.T) {
	m := newMetrics()

	m.noteAgentApprovalsPerTurn("claude", 2)
	m.noteAgentApprovalsPerTurn("claude", 5) // at the threshold: not friction
	m.noteAgentApprovalsPerTurn("claude", 9)
	m.noteAgentApprovalsPerTurn("claude", 0) // not counted as a turn

	var b strings.Builder
	m.writeAgentMetrics(&b)
	out := b.String()

	if !strings.Contains(out, `sandboxd_agent_approval_turns_total{agent="claude"} 3`) {
		t.Errorf("turns-with-approvals miscounted (a zero-approval turn must not count):\n%s", out)
	}
	if !strings.Contains(out, `sandboxd_agent_approval_friction_total{agent="claude"} 1`) {
		t.Errorf("friction miscounted; the threshold is exclusive:\n%s", out)
	}
	// The HELP text must say how to read it, or it gets dashboarded as success.
	if !strings.Contains(out, "treat as a policy bug, not engagement") {
		t.Error("the friction metric does not say how to interpret it")
	}
}

func TestApprovalOutcomesAreLabelled(t *testing.T) {
	m := newMetrics()
	m.incAgentApproval("claude", "allowed")
	m.incAgentApproval("claude", "denied")
	m.incAgentApproval("claude", "timed_out")

	var b strings.Builder
	m.writeAgentMetrics(&b)
	out := b.String()
	// A timeout is distinct from a denial.
	for _, want := range []string{`outcome="allowed"`, `outcome="denied"`, `outcome="timed_out"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s:\n%s", want, out)
		}
	}
}
