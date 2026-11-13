package store

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// createHooksConcurrently fires n simultaneous creates against limit and
// returns how many were stored.
func createHooksConcurrently(t *testing.T, s *Store, org string, limit, n int) (created int) {
	t.Helper()
	var (
		start sync.WaitGroup
		done  sync.WaitGroup
		mu    sync.Mutex
	)
	start.Add(1)
	for i := 0; i < n; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			_, err := s.CreateAgentWebhook(t.Context(),
				AgentWebhook{OrgID: org, URL: "https://example.invalid/hook"}, "secret", limit)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created++
			case !errors.Is(err, ErrWebhookLimit):
				t.Errorf("create: %v", err)
			}
		}()
	}
	start.Done()
	done.Wait()
	return created
}

func TestWebhookLimitHoldsUnderConcurrency(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		s := openTempStore(t)
		if got := createHooksConcurrently(t, s, "acme", 3, 20); got != 3 {
			t.Errorf("20 simultaneous creates against limit=3 stored %d, want exactly 3", got)
		}
	})

	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("SANDBOXD_TEST_POSTGRES_URL")
		if dsn == "" {
			t.Skip("set SANDBOXD_TEST_POSTGRES_URL to run Postgres tests")
		}
		s, err := Open(t.Context(), dsn)
		if err != nil {
			t.Fatalf("open postgres: %v", err)
		}
		defer s.Close()
		org := fmt.Sprintf("hooks-%d", time.Now().UnixNano())
		t.Cleanup(func() { _, _ = s.db.Exec(s.rebind(`DELETE FROM agent_webhooks WHERE org_id = ?`), org) })
		if got := createHooksConcurrently(t, s, org, 3, 20); got != 3 {
			t.Errorf("20 simultaneous creates against limit=3 stored %d, want exactly 3", got)
		}
	})
}

func TestWebhookLimitIsPerOrgAndSandbox(t *testing.T) {
	s := openTempStore(t)
	create := func(org, sandbox string) error {
		_, err := s.CreateAgentWebhook(t.Context(),
			AgentWebhook{OrgID: org, SandboxID: sandbox, URL: "https://example.invalid/hook"}, "secret", 1)
		return err
	}
	if err := create("org-a", ""); err != nil {
		t.Fatalf("first hook: %v", err)
	}
	if err := create("org-a", ""); !errors.Is(err, ErrWebhookLimit) {
		t.Errorf("second hook in a full org: err=%v, want ErrWebhookLimit", err)
	}
	if err := create("org-b", ""); err != nil {
		t.Errorf("org-b refused because org-a was full: %v", err)
	}
	if err := create("org-a", "sb-1"); err != nil {
		t.Errorf("a sandbox hook was counted against the org-level limit: %v", err)
	}
}
