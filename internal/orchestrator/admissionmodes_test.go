package orchestrator

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/entitlements"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// These tests record admission behaviour per SANDBOXD_MODE; run with -v to see the table.

// admitOnce reports whether a create for org would be admitted, and why.
func admitOnce(t *testing.T, s *Server, org string) (admitted bool, msg string) {
	t.Helper()
	const body = `{"template":"base"}`
	release, msg, ok := s.admitBody(createRequest(org, body), []byte(body))
	if ok {
		release()
	}
	return ok, msg
}

// fillSandboxes puts n running sandboxes in the shared store for org.
func fillSandboxes(t *testing.T, st *store.Store, org string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := st.SaveSandbox(t.Context(), store.SandboxRecord{
			ID:        fmt.Sprintf("sb-%s-%d", org, i),
			OrgID:     org,
			Template:  "base",
			State:     "running",
			CreatedAt: 1, UpdatedAt: 1, ExpiresAt: 1 << 40,
		}); err != nil {
			t.Fatalf("save sandbox %d: %v", i, err)
		}
	}
}

func TestAdmissionInTheThreeStatesAnOperatorCanBeIn(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "modes.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	s := NewServer("k", "t")
	s.SetStore(st)

	t.Log("")
	t.Log("  state                        | at the cap? | admitted | what the caller sees")
	t.Log("  -----------------------------+-------------+----------+---------------------")

	// 1. No preset assigned, the state every new org is in until setplan runs.
	const fresh = "u:never-assigned-a-plan"
	fillSandboxes(t, st, fresh, 50) // far past any preset's ceiling
	ok, msg := admitOnce(t, s, fresh)
	t.Logf("  no preset assigned           |  50 running |   %-5v  | %s", ok, orNothing(msg))
	if !ok {
		t.Error("expected a brand-new org to be unlimited — that is the documented self-hosting default")
	}

	// 2. A preset assigned, at its ceiling.
	const capped = "u:on-a-preset"
	if err := st.SetPlan(t.Context(), capped, "free"); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	plan, _ := entitlements.PlanByID("free")
	fillSandboxes(t, st, capped, plan.MaxConcurrent)
	ok, msg = admitOnce(t, s, capped)
	t.Logf("  preset '%s' (max %d)        |  %2d running |   %-5v  | %s",
		plan.ID, plan.MaxConcurrent, plan.MaxConcurrent, ok, orNothing(msg))
	if ok {
		t.Errorf("admitted past the %s preset's cap of %d — the cap does not bind at all",
			plan.ID, plan.MaxConcurrent)
	}

	// 3. The same capped org while the store is unreachable (closed).
	st.Close()
	ok, msg = admitOnce(t, s, capped)
	t.Logf("  same org, database DOWN      |  %2d running |   %-5v  | %s",
		plan.MaxConcurrent, ok, orNothing(msg))
	t.Log("")

	if !ok {
		t.Error("expected a store error to fail OPEN — that is the current documented behaviour, " +
			"and a change to it is the SANDBOXD_MODE work, not a bug fix")
	}
}

func orNothing(msg string) string {
	if msg == "" {
		return "(no refusal — create proceeds)"
	}
	return msg
}

// Each case asserts that the two modes disagree, so a regression that re-unifies them fails here.
func TestMultiTenantAdmissionFailsClosedWhereSelfHostedFailsOpen(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "mt.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	self := NewServer("k", "t")
	self.SetStore(st)
	mt := NewServer("k", "t")
	mt.SetStore(st)
	mt.multiTenant = true

	t.Log("")
	t.Log("  state                     | self-hosted | multi-tenant")
	t.Log("  --------------------------+-------------+-------------")

	// 1. No plan row: unlimited self-hosted, refused multi-tenant.
	const fresh = "u:no-plan-assigned"
	fillSandboxes(t, st, fresh, 50)
	selfOK, _ := admitOnce(t, self, fresh)
	mtOK, mtMsg := admitOnce(t, mt, fresh)
	t.Logf("  no plan assigned          |    %-5v    |    %-5v", selfOK, mtOK)
	if !selfOK {
		t.Error("self-hosted must keep allowing an org with no plan row")
	}
	if mtOK {
		t.Error("multi-tenant admitted an org with no plan row — every org is then uncapped from signup")
	}
	if mtMsg == "" {
		t.Error("a multi-tenant refusal must say something actionable")
	}

	// 2. A readable, exceeded cap refuses in both modes.
	const capped = "u:on-a-preset"
	if err := st.SetPlan(t.Context(), capped, "free"); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	plan, _ := entitlements.PlanByID("free")
	fillSandboxes(t, st, capped, plan.MaxConcurrent)
	selfOK, _ = admitOnce(t, self, capped)
	mtOK, _ = admitOnce(t, mt, capped)
	t.Logf("  at the cap                |    %-5v    |    %-5v", selfOK, mtOK)
	if selfOK || mtOK {
		t.Error("a readable, exceeded cap must refuse in both modes")
	}

	// 3. Database down.
	st.Close()
	selfOK, _ = admitOnce(t, self, capped)
	mtOK, mtMsg = admitOnce(t, mt, capped)
	t.Logf("  database DOWN             |    %-5v    |    %-5v", selfOK, mtOK)
	t.Log("")
	if !selfOK {
		t.Error("self-hosted must still fail OPEN on a store error")
	}
	if mtOK {
		t.Error("multi-tenant admitted while the store was unreachable — an attacker who can " +
			"degrade the database turns every cap into unlimited")
	}
	if mtMsg != admissionUnavailable {
		t.Errorf("store-error refusal message = %q, want the generic %q so the failing query is not probeable",
			mtMsg, admissionUnavailable)
	}
}

// The orchestrator boots store-less when store.Open fails, and must not then serve with admission disabled.
func TestMultiTenantRefusesWhenNoStoreIsWired(t *testing.T) {
	mt := NewServer("k", "t")
	mt.multiTenant = true
	if ok, msg := admitOnce(t, mt, "u:someone"); ok {
		t.Error("multi-tenant admitted with no store wired — that replica enforces no limit at all")
	} else if msg != admissionUnavailable {
		t.Errorf("msg = %q, want %q", msg, admissionUnavailable)
	}
	self := NewServer("k", "t")
	if ok, _ := admitOnce(t, self, "u:someone"); !ok {
		t.Error("self-hosted/standalone must still work with no store")
	}
}

func TestSandboxdModeParsing(t *testing.T) {
	for _, c := range []struct {
		env  string
		want bool
	}{
		{"", false}, {"self-hosted", false}, {"anything-else", false},
		{"multi-tenant", true}, {"MULTI-TENANT", true}, {" multi-tenant ", true},
		{"multitenant", true}, {"multi_tenant", true},
	} {
		t.Setenv("SANDBOXD_MODE", c.env)
		if got := multiTenantFromEnv(); got != c.want {
			t.Errorf("SANDBOXD_MODE=%q → multiTenant=%v, want %v", c.env, got, c.want)
		}
	}
}
