package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// These tests assert that run reaches each boot guard; the guards themselves
// are tested in internal/orchestrator.

// bootEnv clears every variable run reads, then applies the case's own.
func bootEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range []string{
		"ORCHESTRATOR_TOKEN", "NODE_KEY_SECRET", "ORG_ASSERT_KEY", "DEV_INSECURE",
		"SANDBOXD_DEV", "SANDBOXD_DATABASE_URL", "ORCHESTRATOR_ARTIFACT_DIR",
		"OTEL_EXPORTER_OTLP_ENDPOINT", "PORT",
		"ORCHESTRATOR_ROLE", "REDIS_URL", "VAULT_SECRET_KEY", "ALLOW_PLAINTEXT_VAULT",
		"PORTAL_ORIGIN", "PORTAL_SANDBOX_DOMAIN", "PORTAL_LEGACY_SANDBOX_DOMAINS",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestRunRefusesAClusteredControlPlaneWithNoNodeKeySecret(t *testing.T) {
	bootEnv(t, map[string]string{"ORCHESTRATOR_TOKEN": "fleet-secret"})

	err := run(t.Context(), func() {})
	if err == nil {
		t.Fatal("a clustered control plane started with NODE_KEY_SECRET unset — every node's " +
			"credentials would be derived from the token every node already holds")
	}
	if !strings.Contains(err.Error(), "NODE_KEY_SECRET") {
		t.Errorf("refused, but not for the node-key reason: %v", err)
	}
}

func TestRunRefusesAClusteredControlPlaneWithNoOrgAssertKey(t *testing.T) {
	bootEnv(t, map[string]string{
		"ORCHESTRATOR_TOKEN": "fleet-secret",
		"NODE_KEY_SECRET":    "0123456789abcdef",
	})

	err := run(t.Context(), func() {})
	if err == nil {
		t.Fatal("started with ORG_ASSERT_KEY unset: every X-Org-Override would 403 at the node")
	}
	if !strings.Contains(err.Error(), "ORG_ASSERT_KEY") {
		t.Errorf("refused, but not for the org-assert reason: %v", err)
	}
}

func TestRunRefusesAClusteredControlPlaneWithNoVaultKey(t *testing.T) {
	bootEnv(t, map[string]string{
		"ORCHESTRATOR_TOKEN": "fleet-secret",
		"NODE_KEY_SECRET":    "0123456789abcdef",
		"ORG_ASSERT_KEY":     strings.Repeat("ab", 32),
		// Past the guard, boot fails here instead of serving.
		"ORCHESTRATOR_ARTIFACT_DIR": unusableDir(t),
	})

	err := run(t.Context(), func() {})
	if err == nil || !strings.Contains(err.Error(), "VAULT_SECRET_KEY") {
		t.Fatalf("err = %v, want the VAULT_SECRET_KEY refusal", err)
	}
}

func TestDevInsecureLetsAClusteredControlPlanePastBothKeyGuards(t *testing.T) {
	bootEnv(t, map[string]string{
		"ORCHESTRATOR_TOKEN": "fleet-secret",
		"DEV_INSECURE":       "1",
		// Fails later, at the artifact store, proving both key guards passed.
		"ORCHESTRATOR_ARTIFACT_DIR": unusableDir(t),
	})

	err := run(t.Context(), func() {})
	if err == nil {
		t.Fatal("expected the artifact-dir refusal")
	}
	if strings.Contains(err.Error(), "NODE_KEY_SECRET") || strings.Contains(err.Error(), "ORG_ASSERT_KEY") {
		t.Errorf("DEV_INSECURE=1 no longer bypasses the key guards: %v", err)
	}
}

func TestRunRefusesAnUnusableArtifactDir(t *testing.T) {
	bootEnv(t, map[string]string{"ORCHESTRATOR_ARTIFACT_DIR": unusableDir(t)})

	err := run(t.Context(), func() {})
	if err == nil {
		t.Fatal("started with an unusable ORCHESTRATOR_ARTIFACT_DIR")
	}
	if !strings.Contains(err.Error(), "ORCHESTRATOR_ARTIFACT_DIR") {
		t.Errorf("refused, but not for the artifact-store reason: %v", err)
	}
}

// unusableDir returns a path whose parent is a regular file, so MkdirAll fails without root.
func unusableDir(t *testing.T) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return filepath.Join(f, "artifacts")
}

// A dashboard on a host the legacy redirect would take could never be reached.
func TestRunRefusesADashboardTheLegacyRedirectWouldTake(t *testing.T) {
	bootEnv(t, map[string]string{
		"PORTAL_SANDBOX_DOMAIN":         "pilldrops.app",
		"PORTAL_LEGACY_SANDBOX_DOMAINS": "pilldrops.com",
		"PORTAL_ORIGIN":                 "https://console.pilldrops.com",
		// Past the guard, boot fails here instead of serving.
		"ORCHESTRATOR_ARTIFACT_DIR": unusableDir(t),
	})
	err := run(t.Context(), func() {})
	if err == nil || !strings.Contains(err.Error(), "PORTAL_ORIGIN host console.pilldrops.com") {
		t.Fatalf("err = %v, want the refusal naming the dashboard host", err)
	}
}

func TestRunRefusesLegacyDomainsWithNoDashboardOrigin(t *testing.T) {
	bootEnv(t, map[string]string{
		"PORTAL_SANDBOX_DOMAIN":         "pilldrops.app",
		"PORTAL_LEGACY_SANDBOX_DOMAINS": "pilldrops.com",
		"ORCHESTRATOR_ARTIFACT_DIR":     unusableDir(t),
	})
	err := run(t.Context(), func() {})
	if err == nil || !strings.Contains(err.Error(), "PORTAL_ORIGIN") {
		t.Fatalf("err = %v, want the PORTAL_ORIGIN refusal", err)
	}
}

func TestRunRefusesAnEdgeWithNoRedis(t *testing.T) {
	bootEnv(t, map[string]string{"ORCHESTRATOR_ROLE": "edge"})
	err := run(t.Context(), func() {})
	if err == nil || !strings.Contains(err.Error(), "REDIS_URL") {
		t.Fatalf("an edge with no shared route table started (err=%v); it would 404 every sandbox", err)
	}
}

// A database that is down when the control plane boots must not leave it
// unready until someone restarts it: the process is alive, so systemd will not.
func TestOpenStoreRetriesUntilTheDatabaseIsUp(t *testing.T) {
	want, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = want.Close() })
	attempts := 0
	open := func(context.Context, string) (*store.Store, error) {
		attempts++
		if attempts < 3 {
			return nil, errors.New("connection refused")
		}
		return want, nil
	}
	if got := openStoreWithRetry(t.Context(), "dsn", open, time.Millisecond); got != want || attempts != 3 {
		t.Fatalf("got store %v after %d attempts, want the store on attempt 3", got, attempts)
	}
}

func TestOpenStoreStopsRetryingOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	open := func(context.Context, string) (*store.Store, error) { return nil, errors.New("connection refused") }
	if got := openStoreWithRetry(ctx, "dsn", open, time.Hour); got != nil {
		t.Fatalf("got store %v after shutdown, want nil", got)
	}
}
