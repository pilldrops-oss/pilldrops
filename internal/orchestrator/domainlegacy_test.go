package orchestrator

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// No org may attach a host under the current domain, a legacy one, or the
// dashboard's registrable domain: while old wildcard DNS still points here,
// login.<dashboard domain> would otherwise be anyone's for the taking.
func TestDomainCreateRefusesEveryPlatformDomain(t *testing.T) {
	s, _ := domainServer(t)
	s.sandboxDomain = "pilldrops.app"
	s.portalOrigin = "https://console.example.net"
	// Until previews move, a self-hoster may attach hosts under the dashboard's own domain.
	if w := apiReq(t, s, "POST", "u:one", "/v1/domains", `{"host":"demo.example.net","sandbox_id":"sb1"}`); w.Code != http.StatusCreated {
		t.Fatalf("no legacy domain: demo.example.net = %d %q, want 201", w.Code, w.Body.String())
	}
	if w := apiReq(t, s, "POST", "u:one", "/v1/domains", `{"host":"x.pilldrops.app","sandbox_id":"sb1"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("no legacy domain: x.pilldrops.app = %d, want 400", w.Code)
	}

	s.setLegacyDomains([]string{"old.example"})
	for _, host := range []string{"x.pilldrops.app", "pilldrops.app", "x.old.example", "*.old.example", "login.example.net", "example.net"} {
		if w := apiReq(t, s, "POST", "u:one", "/v1/domains", `{"host":`+quote(host)+`,"sandbox_id":"sb1"}`); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d %q, want 400", host, w.Code, w.Body.String())
		}
	}
	if w := apiReq(t, s, "POST", "u:one", "/v1/domains", `{"host":"app.customer.com","sandbox_id":"sb1"}`); w.Code != http.StatusCreated {
		t.Fatalf("a customer's own host = %d %q, want 201", w.Code, w.Body.String())
	}
}

func TestDomainLegacyTargets(t *testing.T) {
	s := NewServer("fleet-key", "nodetoken")
	s.sandboxDomain = "pilldrops.app"
	if got := s.domainLegacyTargets(store.Domain{SandboxID: "sb1", Port: 3000}); got != nil {
		t.Fatalf("no legacy domain: %v", got)
	}
	s.setLegacyDomains([]string{"pilldrops.com"})
	if got := s.domainLegacyTargets(store.Domain{SandboxID: "sb1", Port: 3000}); len(got) != 1 || got[0] != "3000-sb1.pilldrops.com" {
		t.Fatalf("exact row = %v", got)
	}
	if got := s.domainLegacyTargets(store.Domain{Host: "*.vms.example.com", Wildcard: true}); len(got) != 1 || got[0] != "pilldrops.com" {
		t.Fatalf("wildcard row = %v", got)
	}
}

// A customer host still on its old CNAME keeps working while it gets traffic,
// so the traffic is recorded, at most once per window per replica, and the
// Domains list tells the owner which record to create instead.
func TestLegacyCustomDomainTrafficIsRecordedOncePerWindow(t *testing.T) {
	s, _ := domainServer(t)
	s.sandboxDomain = "pilldrops.app"
	now := time.Now()
	s.now = func() time.Time { return now }
	st := s.st.Load()
	if w := apiReq(t, s, "POST", "u:one", "/v1/domains", `{"host":"app.example.com","sandbox_id":"sb1"}`); w.Code != http.StatusCreated {
		t.Fatalf("create = %d %q", w.Code, w.Body.String())
	}
	if err := st.SetDomainLegacySeen(t.Context(), "app.example.com", now.Unix()-100); err != nil {
		t.Fatal(err)
	}
	activate(t, s, "app.example.com")
	seen := func() int64 {
		t.Helper()
		d, _, err := st.GetDomain(t.Context(), "app.example.com")
		if err != nil {
			t.Fatal(err)
		}
		return d.LegacyTargetSeenAt
	}

	if w := hostReq(t, s, "app.example.com", "/"); !strings.Contains(w.Body.String(), "FROM-APP") {
		t.Fatalf("legacy-CNAME host = %d %q, want the app", w.Code, w.Body.String())
	}
	if got := seen(); got != now.Unix() {
		t.Fatalf("seen = %d, want %d", got, now.Unix())
	}
	first := now.Unix()
	now = now.Add(5 * time.Minute)
	hostReq(t, s, "app.example.com", "/")
	if got := seen(); got != first {
		t.Fatalf("seen = %d five minutes later, want %d: one replica wrote twice in a window", got, first)
	}
	now = now.Add(6 * time.Minute)
	hostReq(t, s, "app.example.com", "/")
	if got := seen(); got != now.Unix() {
		t.Fatalf("seen = %d eleven minutes later, want %d", got, now.Unix())
	}

	var list struct {
		Domains []map[string]any `json:"domains"`
	}
	w := apiReq(t, s, "GET", "u:one", "/v1/domains", "")
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Domains) != 1 {
		t.Fatalf("list = %q %v", w.Body.String(), err)
	}
	if list.Domains[0]["legacy_target"] != true || list.Domains[0]["record"] != "app.example.com CNAME 8000-sb1.pilldrops.app" {
		t.Fatalf("listed row = %v, want legacy_target and the new record", list.Domains[0])
	}
}

// A row whose marker is older than the window has lapsed; a request that still
// reaches it (cached route, or before the next recheck) must not revive it.
func TestLapsedLegacyCustomDomainTrafficIsNotRecorded(t *testing.T) {
	s, _ := domainServer(t)
	s.sandboxDomain = "pilldrops.app"
	now := time.Now()
	s.now = func() time.Time { return now }
	st := s.st.Load()
	if w := apiReq(t, s, "POST", "u:one", "/v1/domains", `{"host":"app.example.com","sandbox_id":"sb1"}`); w.Code != http.StatusCreated {
		t.Fatalf("create = %d %q", w.Code, w.Body.String())
	}
	lapsed := now.Add(-store.LegacyUseWindow - time.Hour).Unix()
	if err := st.SetDomainLegacySeen(t.Context(), "app.example.com", lapsed); err != nil {
		t.Fatal(err)
	}
	activate(t, s, "app.example.com")

	hostReq(t, s, "app.example.com", "/")
	d, _, err := st.GetDomain(t.Context(), "app.example.com")
	if err != nil || d.LegacyTargetSeenAt != lapsed {
		t.Fatalf("seen = %d err=%v, want %d: traffic revived a lapsed row", d.LegacyTargetSeenAt, err, lapsed)
	}
}
