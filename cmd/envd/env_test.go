package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envString(env []string) string { return strings.Join(env, "\n") + "\n" }

func TestSessionEnvHasIdentity(t *testing.T) {
	t.Setenv("HOME", "/")
	t.Setenv("USER", "")

	env := buildSessionEnvFrom(t.TempDir(), nil)
	got := envString(env)
	for _, want := range []string{"HOME=/root\n", "USER=root\n", "LOGNAME=root\n", "SHELL=/bin/bash\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in session env:\n%s", want, got)
		}
	}
	if strings.Contains(got, "HOME=/\n") {
		t.Errorf("polluted HOME leaked through:\n%s", got)
	}
}

func TestSessionEnvHasSanePath(t *testing.T) {
	t.Setenv("PATH", "")
	env := buildSessionEnvFrom(t.TempDir(), nil)
	got := envString(env)
	if !strings.Contains(got, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n") {
		t.Errorf("default PATH not set:\n%s", got)
	}
}

func TestSessionEnvMergesBakedSandboxEnv(t *testing.T) {
	dir := t.TempDir()
	content := "PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n" +
		"GOLANG_VERSION=1.22.12\n" +
		"GOPATH=/go\n"
	if err := os.WriteFile(filepath.Join(dir, "sandbox-env"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	env := buildSessionEnvFrom(dir, nil)
	got := envString(env)
	if !strings.Contains(got, "PATH=/usr/local/go/bin:") {
		t.Errorf("baked image PATH not applied:\n%s", got)
	}
	if !strings.Contains(got, "GOPATH=/go\n") {
		t.Errorf("baked image var not applied:\n%s", got)
	}
}

func TestSessionEnvTemplateEnvOverridesSandboxEnv(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "sandbox-env"), []byte("MODE=image\nKEEP=1\n"), 0644)
	os.WriteFile(filepath.Join(dir, "template.env"), []byte("MODE=operator\n"), 0644)

	got := envString(buildSessionEnvFrom(dir, nil))
	if !strings.Contains(got, "MODE=operator\n") {
		t.Errorf("template.env must override sandbox-env:\n%s", got)
	}
	if !strings.Contains(got, "KEEP=1\n") {
		t.Errorf("non-overridden image var lost:\n%s", got)
	}
}

func TestSessionEnvMergesTemplateEnvFile(t *testing.T) {
	dir := t.TempDir()
	content := "# template env for go\n" +
		"PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n" +
		"GOPATH=/root/go\n" +
		"\n" +
		"malformed-line-no-equals\n"
	if err := os.WriteFile(filepath.Join(dir, "template.env"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	env := buildSessionEnvFrom(dir, nil)
	got := envString(env)
	if !strings.Contains(got, "PATH=/usr/local/go/bin:") {
		t.Errorf("template PATH not merged:\n%s", got)
	}
	if !strings.Contains(got, "GOPATH=/root/go\n") {
		t.Errorf("template var not merged:\n%s", got)
	}
	if strings.Contains(got, "malformed") {
		t.Errorf("malformed line should be skipped:\n%s", got)
	}
}

func TestSessionEnvRequestOverridesEverything(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "template.env"), []byte("MODE=template\n"), 0644)

	env := buildSessionEnvFrom(dir, map[string]string{"MODE": "request", "EXTRA": "1"})
	got := envString(env)
	if !strings.Contains(got, "MODE=request\n") {
		t.Errorf("per-request env must win over template.env:\n%s", got)
	}
	if !strings.Contains(got, "EXTRA=1\n") {
		t.Errorf("per-request env missing:\n%s", got)
	}
	if strings.Contains(got, "MODE=template\n") {
		t.Errorf("duplicate/stale key left behind:\n%s", got)
	}
}

func TestSessionEnvPreservesBootEnv(t *testing.T) {
	t.Setenv("PORT", "8000")
	env := buildSessionEnvFrom(t.TempDir(), nil)
	if !strings.Contains(envString(env), "PORT=8000\n") {
		t.Errorf("boot env PORT lost:\n%s", envString(env))
	}
}

func TestWriteProfileDropIn(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "sandbox-env"),
		[]byte("PATH=/usr/local/go/bin:/usr/bin\nGOPATH=/go\nWEIRD=has'quote\n"), 0644)
	profileDir := filepath.Join(dir, "profile.d")

	if err := writeProfileDropIn(dir, profileDir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(profileDir, "zz-pilldrops-env.sh"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{
		"export PATH='/usr/local/go/bin:/usr/bin'\n",
		"export GOPATH='/go'\n",
		`export WEIRD='has'\''quote'` + "\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("drop-in missing %q:\n%s", want, got)
		}
	}
}

// With no baked env the drop-in still exists: it is what sources the sandbox
// identity into login shells, and that file arrives after boot.
func TestWriteProfileDropInNoEnvFiles(t *testing.T) {
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "profile.d")
	if err := writeProfileDropIn(dir, profileDir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(profileDir, "zz-pilldrops-env.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); strings.Contains(got, "export ") || !strings.Contains(got, identityProfileLine(identityDir)) {
		t.Errorf("drop-in must carry only the identity source line:\n%s", got)
	}
}

func TestSessionEnvNoDuplicateKeys(t *testing.T) {
	t.Setenv("HOME", "/")
	env := buildSessionEnvFrom(t.TempDir(), map[string]string{"HOME": "/root"})
	seen := map[string]int{}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		seen[k]++
	}
	for k, n := range seen {
		if n > 1 {
			t.Errorf("duplicate key %q appears %d times", k, n)
		}
	}
}
