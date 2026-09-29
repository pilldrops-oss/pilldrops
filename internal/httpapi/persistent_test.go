package httpapi

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/entitlements"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/process"
	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// newBilledTestServer is newTestServerWithStore with the org on a plan, since
// an org with none is unlimited and no cap can be observed.
func newBilledTestServer(t *testing.T) (*httptest.Server, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	_, key, err := st.CreateKey(t.Context(), "test", "default")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	d := daemon.New(daemon.Config{
		Runtime:     process.New(filepath.Join(dir, "sb")),
		Store:       daemon.StoreAdapter{S: st},
		SnapshotDir: filepath.Join(dir, "snap"),
		IdleTimeout: time.Hour, HardTTL: time.Hour,
	})
	s := New(d, st)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(srv.Close)
	// The first plan in the current catalogue, not a hardcoded id: a test that
	// installed its own catalogue would otherwise fall back to unlimited.
	if err := st.SetPlan(t.Context(), "default", entitlements.Plans()[0].ID); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	return srv, st, key
}

// usePlan puts the org on a plan.
func usePlan(t *testing.T, st *store.Store, org, plan string) {
	t.Helper()
	if err := st.SetPlan(t.Context(), org, plan); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
}

// A negative timeout asks for no expiry; plans without Persistent degrade to
// the short lease instead.
func TestCreatePersistentRequiresPersistentPlan(t *testing.T) {
	srv, st, key := newBilledTestServer(t)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base","timeout":-1}`)
	if code != http.StatusCreated {
		t.Fatalf("create on free = %d (%v)", code, out)
	}
	if out["expires_at"] == nil {
		t.Error("free plan got a persistent sandbox — expires_at is null, want a lease expiry")
	}

	usePlan(t, st, "default", "pro")
	code, out = do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base","timeout":-1}`)
	if code != http.StatusCreated {
		t.Fatalf("create on pro = %d (%v)", code, out)
	}
	if out["expires_at"] != nil {
		t.Errorf("persistent sandbox expires_at = %v, want null (no expiry)", out["expires_at"])
	}
}

// Persistence is opt-in: an unset timeout keeps the lease even on a persistent plan.
func TestCreateDefaultTimeoutStaysLeasedOnPersistentPlan(t *testing.T) {
	srv, st, key := newBilledTestServer(t)
	usePlan(t, st, "default", "pro")

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d (%v)", code, out)
	}
	if out["expires_at"] == nil {
		t.Error("an unset timeout must still get a lease, not persistence")
	}
}

func TestUnlimitedIsPersistent(t *testing.T) {
	if !entitlements.Unlimited().Persistent {
		t.Error("Unlimited plan must allow persistent sandboxes")
	}
	if entitlements.DefaultPlan().Persistent {
		t.Error("the free default plan must NOT allow persistent sandboxes (abuse vector)")
	}
}

// An operator-granted plan stamps renews_at a year out; deriving the cycle
// start from it must not land in the future and zero the hours.
func TestActiveHoursSurvivesFarFutureRenewal(t *testing.T) {
	srv, st, key := newBilledTestServer(t)
	usePlan(t, st, "default", "pro")

	if err := st.RecordUsage(t.Context(), "sb_x", "default", store.UsageDelta{
		VCPUSeconds: 7200, RAMGBSeconds: 7200, RunSeconds: 3600,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPlan(t.Context(), "default", "pro"); err != nil {
		t.Fatal(err)
	}

	code, out := do(t, "GET", srv.URL+"/v1/limits", key, "")
	if code != http.StatusOK {
		t.Fatalf("limits = %d", code)
	}
	hours, _ := out["hours"].(map[string]any)
	used, _ := hours["used"].(float64)
	if used < 0.9 || used > 1.1 {
		t.Errorf("hours.used = %v, want ~1 — a far-future renewal must not zero the window", used)
	}
}

// MaxConcurrent counts all sandboxes, so workstations need their own cap.
func TestPersistentWorkstationCapIsEnforced(t *testing.T) {
	srv, st, key := newBilledTestServer(t)
	usePlan(t, st, "default", "pro") // Pro includes 1 workstation

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base","timeout":-1}`)
	if code != http.StatusCreated {
		t.Fatalf("first workstation = %d (%v)", code, out)
	}
	if out["expires_at"] != nil {
		t.Fatal("first workstation is not persistent — the cap test proves nothing")
	}

	code, out = do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base","timeout":-1}`)
	if code != http.StatusTooManyRequests {
		t.Errorf("second workstation on a 1-workstation plan = %d, want 429 (%v)", code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "upgrade") {
		t.Errorf("refusal does not say how to fix it: %q", msg)
	}

	// Leased sandboxes must still work at the workstation cap.
	code, _ = do(t, "POST", srv.URL+"/v1/sandboxes", key, `{"template":"base"}`)
	if code != http.StatusCreated {
		t.Errorf("leased sandbox blocked by the workstation cap: %d", code)
	}
}

// Every plan's advertised workstation count must be a real limit.
func TestWorkstationCapsMatchThePlans(t *testing.T) {
	for id, want := range map[string]int{"free": 0, "personal": 1, "pro": 1, "max": 3} {
		p, ok := entitlements.PlanByID(id)
		if !ok {
			t.Fatalf("%s missing", id)
		}
		if p.MaxWorkstations != want {
			t.Errorf("%s allows %d workstations, page says %d", id, p.MaxWorkstations, want)
		}
	}
	// Free must be zero and non-persistent; either alone is a loophole.
	if f := entitlements.DefaultPlan(); f.MaxWorkstations != 0 || f.Persistent {
		t.Errorf("free can hold a permanent machine: %+v", f)
	}
}
