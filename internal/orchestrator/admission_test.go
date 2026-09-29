package orchestrator

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/entitlements"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// workstationOrg assigns org a plan that allows workstations and returns it.
func workstationOrg(t *testing.T, st *store.Store, org, planID string) entitlements.Plan {
	t.Helper()
	if err := st.SetPlan(t.Context(), org, planID); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	p, ok := entitlements.PlanByID(planID)
	if !ok {
		t.Fatalf("plan %q is not in the catalogue", planID)
	}
	if !p.Persistent || p.MaxWorkstations <= 0 {
		t.Fatalf("preset %q allows no workstations (%+v) — wrong fixture for this test", planID, p)
	}
	return p
}

func TestWorkstationCapIsCountedAcrossTheFleet(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "ws.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()
	s := NewServer("k", "t")
	s.SetStore(st)

	const org = "u:workstation"
	plan := workstationOrg(t, st, org, "personal")

	// Zero expires_at is what marks a persistent sandbox.
	for i := 0; i < plan.MaxWorkstations; i++ {
		if err := st.SaveSandbox(t.Context(), store.SandboxRecord{
			ID: "ws-on-node-" + string(rune('a'+i)), OrgID: org, Template: "base", State: "running",
			CreatedAt: 1, UpdatedAt: 1, ExpiresAt: 0,
		}); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	req := createRequest(org, `{"template":"base","timeout":-1}`)
	_, msg, ok := s.admitBody(req, []byte(`{"template":"base","timeout":-1}`))
	if ok {
		t.Fatalf("admitted workstation %d on a plan that includes %d — the cap does not hold across nodes",
			plan.MaxWorkstations+1, plan.MaxWorkstations)
	}
	if !strings.Contains(msg, plan.ID) {
		t.Errorf("refusal %q does not name the plan the customer is on", msg)
	}
	if !strings.Contains(msg, "workstation") {
		t.Errorf("refusal %q does not say which limit was hit", msg)
	}

	// An ordinary create from the same org is not subject to the workstation cap.
	body := []byte(`{"template":"base","timeout":900}`)
	release, _, ok := s.admitBody(createRequest(org, string(body)), body)
	if !ok {
		t.Error("an ephemeral create was refused by the workstation cap — the wrong limit is being applied")
	}
	release()

	const fresh = "u:first-timer"
	workstationOrg(t, st, fresh, "personal")
	ws := []byte(`{"timeout":-1}`)
	release, _, ok = s.admitBody(createRequest(fresh, string(ws)), ws)
	if !ok {
		t.Error("an org with no workstations was refused its first — the cap is now refusing everyone")
	}
	release()
}

// On a plan with no workstations a negative timeout degrades to the short lease, so it must be admitted.
func TestWorkstationCapIgnoresNonPersistentPlans(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "free.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()
	s := NewServer("k", "t")
	s.SetStore(st)

	const org = "u:freebie"
	free := entitlements.DefaultPlan()
	if free.MaxWorkstations != 0 {
		t.Skipf("free now includes %d workstation(s) — fixture no longer proves the point", free.MaxWorkstations)
	}
	body := []byte(`{"timeout":-1}`)
	release, msg, ok := s.admitBody(createRequest(org, string(body)), body)
	if !ok {
		t.Fatalf("a free-plan create asking for -1 was refused (%q) — it is a lease, not a workstation", msg)
	}
	release()
}

func TestAdmitPutsTheBodyBack(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "peek.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()
	s := NewServer("k", "t")
	s.SetStore(st)

	const org = "u:peeked"
	workstationOrg(t, st, org, "personal")

	const raw = `{"template":"base","timeout":-1,"vcpu":2}`
	release, _, ok := s.admitBody(createRequest(org, raw), []byte(raw))
	if !ok {
		t.Fatal("first workstation refused")
	}
	release()

	// The peek must actually see the timeout: at the limit, the next one is refused on the body alone.
	if err := st.SaveSandbox(t.Context(), store.SandboxRecord{
		ID: "ws-live", OrgID: org, Template: "base", State: "running", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, _, ok := s.admitBody(createRequest(org, raw), []byte(raw)); ok {
		t.Error("the workstation request in the body was not read — a second permanent machine was admitted")
	}
}

// createRequest builds a POST /v1/sandboxes as it arrives after auth(). The
// operator marker is required: X-Org-Override is only honoured for the fleet
// key, so without it every test would silently run as "default".
func createRequest(org, body string) *http.Request {
	r, _ := http.NewRequest("POST", "/v1/sandboxes", strings.NewReader(body))
	r.Header.Set(orgOverride, org)
	return r.WithContext(withOperator(r.Context()))
}
