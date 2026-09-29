package orchestrator

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
	"github.com/pilldrops-oss/pilldrops/internal/entitlements"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// With Redis, the cap still counts the org's live sandboxes from the store and
// the creates in flight, and refuses at the plan's limit.
func TestRedisAdmissionHoldsThePlanCap(t *testing.T) {
	url := os.Getenv("SANDBOXD_TEST_REDIS_URL")
	if url == "" {
		t.Skip("SANDBOXD_TEST_REDIS_URL not set")
	}
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := NewServer("k", "t")
	s.SetStore(st)
	if s.admit, err = cluster.NewAdmission(url); err != nil {
		t.Fatal(err)
	}

	org := "u:redis-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := st.SetPlan(t.Context(), org, "free"); err != nil {
		t.Fatal(err)
	}
	plan, _ := entitlements.PlanByID("free")
	for i := 0; i < plan.MaxConcurrent-1; i++ {
		if err := st.SaveSandbox(t.Context(), store.SandboxRecord{
			ID: org + "-" + strconv.Itoa(i), OrgID: org, Template: "base", State: "running",
			CreatedAt: 1, UpdatedAt: 1, ExpiresAt: 9e9,
		}); err != nil {
			t.Fatal(err)
		}
	}
	body := []byte(`{"template":"base"}`)
	release, _, ok := s.admitBody(createRequest(org, string(body)), body)
	if !ok {
		t.Fatalf("refused the last place under a cap of %d", plan.MaxConcurrent)
	}
	if _, msg, ok := s.admitBody(createRequest(org, string(body)), body); ok {
		t.Fatal("admitted past the cap while the previous create was still in flight")
	} else if msg == "" {
		t.Fatal("refused without saying why")
	}
	release()
}
