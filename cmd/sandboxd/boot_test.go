package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests assert that run reaches each nodeboot guard; the guards
// themselves are tested in nodeboot.

func bootEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range []string{
		"SANDBOXD_DATA", "SANDBOXD_DATABASE_URL", "SANDBOXD_PRODUCTION",
		"SANDBOXD_DEV", "DEV_INSECURE", "PORT", "OTEL_EXPORTER_OTLP_ENDPOINT",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestRunRefusesProductionOnSQLite(t *testing.T) {
	bootEnv(t, map[string]string{
		"SANDBOXD_DATA":       t.TempDir(),
		"SANDBOXD_PRODUCTION": "1",
		// SANDBOXD_DATABASE_URL unset, so the DSN falls back to the SQLite file.
	})

	err := run()
	if err == nil {
		t.Fatal("a production node booted on SQLite")
	}
	if !strings.Contains(err.Error(), "SANDBOXD_PRODUCTION") {
		t.Errorf("refused, but not for the production-database reason: %v", err)
	}
}

func TestRunRefusesAnUnusableDataDir(t *testing.T) {
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	bootEnv(t, map[string]string{"SANDBOXD_DATA": filepath.Join(f, "data")})

	err := run()
	if err == nil {
		t.Fatal("booted with a data dir that cannot exist")
	}
	if !strings.Contains(err.Error(), "data dir") {
		t.Errorf("refused, but not for the data-dir reason: %v", err)
	}
}
