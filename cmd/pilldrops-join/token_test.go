package main

import (
	"os"
	"path/filepath"
	"testing"
)

func withStdin(t *testing.T, content string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatalf("seek: %v", err)
	}
	old := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = old; f.Close() })
}

func TestStdinWinsOverTheEnvironment(t *testing.T) {
	t.Setenv("JOIN_TOKEN", "from-environment")
	withStdin(t, "  sfj_from_stdin\n")

	got, err := readToken()
	if err != nil {
		t.Fatalf("readToken: %v", err)
	}
	if got != "sfj_from_stdin" {
		t.Fatalf("readToken() = %q, want the stdin value (trimmed)", got)
	}
}

func TestTokenFileIsPreferredOverTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tok")
	if err := os.WriteFile(path, []byte("sfj_from_file\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	withStdin(t, "") // empty stdin must fall through
	t.Setenv("JOIN_TOKEN_FILE", path)
	t.Setenv("JOIN_TOKEN", "from-environment")

	got, err := readToken()
	if err != nil {
		t.Fatalf("readToken: %v", err)
	}
	if got != "sfj_from_file" {
		t.Fatalf("readToken() = %q, want the file's value", got)
	}
}

func TestTheEnvironmentStillWorksAsALastResort(t *testing.T) {
	withStdin(t, "")
	t.Setenv("JOIN_TOKEN", "sfj_from_env")
	got, err := readToken()
	if err != nil {
		t.Fatalf("readToken: %v", err)
	}
	if got != "sfj_from_env" {
		t.Fatalf("readToken() = %q, want the env value", got)
	}
}

func TestNoTokenAnywhereIsEmptyNotAnError(t *testing.T) {
	withStdin(t, "   \n")
	t.Setenv("JOIN_TOKEN", "")
	got, err := readToken()
	if err != nil {
		t.Fatalf("readToken: %v", err)
	}
	if got != "" {
		t.Fatalf("readToken() = %q, want empty so main can print the usage", got)
	}
}
