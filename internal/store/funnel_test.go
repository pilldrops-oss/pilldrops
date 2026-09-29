package store

import (
	"testing"
	"time"
)

func seedUser(t *testing.T, s *Store, owner string, firstSeen int64) {
	t.Helper()
	if err := s.UpsertLogin(t.Context(), firstSeen, owner, owner, owner+"@example.com"); err != nil {
		t.Fatal(err)
	}
}

func TestFunnelCountsSurvivors(t *testing.T) {
	s := openTemp(t)
	now := time.Now().Unix()
	since := now - 30*day

	// A: signed up, created, still running.
	seedUser(t, s, "org_a", now-2*day)
	if err := s.RecordEvent(t.Context(), EventRecord{OrgID: "org_a", SandboxID: "sb_a", Event: "created", At: now - 2*day}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSandbox(t.Context(), SandboxRecord{
		ID: "sb_a", OrgID: "org_a", State: "running",
		CreatedAt: now - 2*day, ExpiresAt: 0, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// B: signed up, created, destroyed after 2 hours.
	seedUser(t, s, "org_b", now-3*day)
	if err := s.RecordEvent(t.Context(), EventRecord{OrgID: "org_b", SandboxID: "sb_b", Event: "created", At: now - 3*day}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordHistory(t.Context(), HistoryRecord{
		ID: "sb_b", OrgID: "org_b", CreatedAt: now - 3*day, DestroyedAt: now - 3*day + 7200,
	}); err != nil {
		t.Fatal(err)
	}

	// C: signed up, never created anything.
	seedUser(t, s, "org_c", now-day)

	f, err := s.Funnel(t.Context(), since)
	if err != nil {
		t.Fatalf("funnel: %v", err)
	}
	if f.Signups != 3 {
		t.Errorf("signups = %d, want 3", f.Signups)
	}
	if f.Created != 2 {
		t.Errorf("created = %d, want 2 (C never created)", f.Created)
	}
	if f.AliveAt24h != 1 {
		t.Errorf("alive@24h = %d, want 1 (B died in 2h)", f.AliveAt24h)
	}
}

func TestFunnelSurvivalBoundary(t *testing.T) {
	s := openTemp(t)
	now := time.Now().Unix()
	start := now - 5*day

	for _, tc := range []struct {
		org      string
		lived    int64
		survives bool
	}{
		{"org_23h", 23 * 3600, false},
		{"org_25h", 25 * 3600, true},
	} {
		seedUser(t, s, tc.org, start)
		if err := s.RecordEvent(t.Context(), EventRecord{OrgID: tc.org, SandboxID: "sb_" + tc.org, Event: "created", At: start}); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordHistory(t.Context(), HistoryRecord{
			ID: "sb_" + tc.org, OrgID: tc.org, CreatedAt: start, DestroyedAt: start + tc.lived,
		}); err != nil {
			t.Fatal(err)
		}
	}
	f, err := s.Funnel(t.Context(), now-30*day)
	if err != nil {
		t.Fatal(err)
	}
	if f.AliveAt24h != 1 {
		t.Errorf("alive@24h = %d, want 1 (25h survives, 23h does not)", f.AliveAt24h)
	}
}

func TestFunnelCountsFirstLoginOnly(t *testing.T) {
	s := openTemp(t)
	now := time.Now().Unix()
	seedUser(t, s, "org_old", now-90*day)
	seedUser(t, s, "org_old", now)

	f, err := s.Funnel(t.Context(), now-7*day)
	if err != nil {
		t.Fatal(err)
	}
	if f.Signups != 0 {
		t.Errorf("signups = %d, want 0 — a returning user is not a new signup", f.Signups)
	}
}

func TestSignupsBySource(t *testing.T) {
	s := openTemp(t)
	now := time.Now().Unix()
	for _, e := range []struct{ email, src string }{
		{"a@x.com", "agent-lp"}, {"b@x.com", "agent-lp"},
		{"c@x.com", "self-host-lp"}, {"d@x.com", ""},
	} {
		if err := s.AddWaitlist(t.Context(), e.email, e.src, "", now); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.SignupsBySource(t.Context(), now-day)
	if err != nil {
		t.Fatal(err)
	}
	if got["agent-lp"] != 2 || got["self-host-lp"] != 1 {
		t.Errorf("by source = %v", got)
	}
	if got["unknown"] != 1 {
		t.Errorf("an untagged signup must not vanish from the scoreboard: %v", got)
	}
}
