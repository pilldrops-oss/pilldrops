package entitlements

import "testing"

func TestPlanHours(t *testing.T) {
	p := Plan{ID: "starter", IncludedHours: 60}

	for _, tc := range []struct {
		used          float64
		wantRemaining float64
		wantPct       float64
	}{
		{0, 60, 0},
		{30, 30, 50},
		{48, 12, 80},
		{60, 0, 100},
		{90, 0, 150}, // remaining floors at 0, pct keeps counting
	} {
		h := p.Hours(tc.used)
		if h.Remaining != tc.wantRemaining {
			t.Errorf("used %.0f: remaining = %.1f, want %.1f", tc.used, h.Remaining, tc.wantRemaining)
		}
		if h.Pct != tc.wantPct {
			t.Errorf("used %.0f: pct = %.1f, want %.1f", tc.used, h.Pct, tc.wantPct)
		}
		if h.Included != 60 || h.Used != tc.used {
			t.Errorf("used %.0f: echoed fields wrong: %+v", tc.used, h)
		}
	}
}

// An unmetered plan must report zero, not divide by zero or imply an exhausted quota.
func TestPlanHoursUnmetered(t *testing.T) {
	h := Unlimited().Hours(500)
	if h.Included != 0 || h.Remaining != 0 || h.Pct != 0 {
		t.Errorf("unmetered plan reported an entitlement: %+v", h)
	}
	if h.Used != 500 {
		t.Errorf("used = %v, want 500 (still reported, just not limited)", h.Used)
	}
}

// The smallest preset is what an untrusted tenant gets, so its caps bound abuse.
func TestSmallestPresetBoundsAbuse(t *testing.T) {
	free := DefaultPlan()
	if free.ID != "free" {
		t.Fatalf("default plan = %q", free.ID)
	}
	if free.Persistent {
		t.Error("the smallest preset allows persistent sandboxes")
	}
	if free.SessionTTLHrs == 0 || free.SessionTTLHrs > 24 {
		t.Errorf("SessionTTLHrs = %d, want a short bounded life", free.SessionTTLHrs)
	}
	if free.MaxConcurrent == 0 {
		t.Error("free has no concurrency cap — one account could take the fleet")
	}
	if free.MaxVCPU == 0 || free.MaxVCPU > 2 {
		t.Errorf("free MaxVCPU = %d, want a small cap (mining scales with cores)", free.MaxVCPU)
	}
}

func TestPresetsAreMonotonic(t *testing.T) {
	free := DefaultPlan()
	for _, id := range []string{"pro", "max"} {
		p, ok := PlanByID(id)
		if !ok {
			t.Fatalf("%s plan missing", id)
		}
		if p.MaxConcurrent <= free.MaxConcurrent {
			t.Errorf("%s allows %d concurrent, the smallest preset allows %d",
				id, p.MaxConcurrent, free.MaxConcurrent)
		}
		if p.MaxVCPU < free.MaxVCPU || p.MaxMemMB < free.MaxMemMB || p.MaxStorageMB < free.MaxStorageMB {
			t.Errorf("%s has a smaller box than the smallest preset: %+v", id, p)
		}
		if p.IncludedHours <= free.IncludedHours {
			t.Errorf("%s includes %d hours, the smallest preset includes %d", id, p.IncludedHours, free.IncludedHours)
		}
		if !p.Persistent {
			t.Errorf("%s is not persistent, but a larger preset should be", id)
		}
	}
}
