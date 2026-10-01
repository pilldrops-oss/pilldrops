package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

func TestRootIsTheExceptionNotTheDefault(t *testing.T) {
	if r := rootReason(false); r != "" {
		t.Fatalf("a plain template is excluded from the drop with reason %q — the class this finding is about is the one that must NOT be excluded", r)
	}
	if u := dropUser(false); u != sandboxRunUser {
		t.Fatalf("dropUser(plain) = %q, want %q", u, sandboxRunUser)
	}
}

func TestServiceTemplatesStayRoot(t *testing.T) {
	if rootReason(true) == "" {
		t.Fatal("an image-backed service template was dropped to a non-root uid; its entrypoint cannot initialise and postgres/mysql first boot breaks")
	}
	if u := dropUser(true); u != "" {
		t.Errorf("dropUser(service) = %q, want \"\" (root)", u)
	}
}

func TestServiceClassMatchesWhatBootImageBranchesOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec rt.BootSpec
		want bool
	}{
		{"plain template", rt.BootSpec{Template: rt.Template{Name: "base"}}, false},
		{"image, no ports (toolbox)", rt.BootSpec{Template: rt.Template{Image: "python:3.11-slim"}}, false},
		{"image + ports (service)", rt.BootSpec{Template: rt.Template{Image: "postgres:16", Ports: []int{5432}}}, true},
		{"ports, no image", rt.BootSpec{Template: rt.Template{Name: "desktop", Ports: []int{8080}}}, false},
	} {
		if got := serviceTemplate(tc.spec); got != tc.want {
			t.Errorf("%s: serviceTemplate = %v, want %v", tc.name, got, tc.want)
		}
	}
	body := sourceContaining(t, "func (r *Runtime) bootImage(")
	if !strings.Contains(body, "service := serviceTemplate(spec)") {
		t.Error("bootImage no longer derives `service` from serviceTemplate — rootReason and bootImage can now disagree about which templates run their own entrypoint")
	}
}

// TestEveryContainerEntryPointDropsPrivilege enumerates every function that
// builds a docker exec, so a new one must either drop or be listed in
// rootByDesign. Scanned per function because argv is assembled across statements.
func TestEveryContainerEntryPointDropsPrivilege(t *testing.T) {
	rootByDesign := map[string]string{
		"DiskUsageMB":  "a platform storage meter; `du` of the workspace, no tenant input",
		"LiveStats":    "the same du, on the metrics path",
		"Signal":       "must outrank the dropped process it is signalling",
		"OOMKillCount": "reads the container's own cgroup memory.events counter",
		"Procs":        "docker top; a platform read",
	}
	drops := []string{`"--user"`, "userArgs()", "execArgs(", "User: d.user", "User: i.runUser"}

	seen := 0
	for _, marker := range []string{
		"func (i *dockerInstance) Exec(",
		"func (d *dockerFS) exec(",
		"func (r *Runtime) bootImage(",
	} {
		for name, body := range topLevelFuncs(sourceContaining(t, marker)) {
			if !strings.Contains(body, `"exec"`) && !strings.Contains(body, `"docker", cliArgs`) &&
				!strings.Contains(body, `execConfig{`) {
				continue
			}
			if !strings.Contains(body, `"exec"`) && !strings.Contains(body, `execConfig{`) {
				continue
			}
			seen++
			if why, ok := rootByDesign[name]; ok {
				if why == "" {
					t.Errorf("%s is exempted with no reason", name)
				}
				continue
			}
			dropped := false
			for _, d := range drops {
				if strings.Contains(body, d) {
					dropped = true
					break
				}
			}
			if !dropped {
				t.Errorf("%s builds a `docker exec` but never applies the run user. "+
					"Tenant code reachable through it runs as uid 0, which on a dockerd with no "+
					"userns-remap is host uid 0. Apply the drop, or add %q to rootByDesign with "+
					"the reason it must be root.", name, name)
			}
		}
	}
	if seen < 6 {
		t.Fatalf("only %d exec-building functions found; the scan is not seeing the package and this test is not checking anything", seen)
	}
}

// topLevelFuncs splits a Go source file into name to body for its top-level
// funcs. A crude `\nfunc ` scan is enough to bound a substring search.
func topLevelFuncs(src string) map[string]string {
	out := map[string]string{}
	parts := strings.Split(src, "\nfunc ")
	for _, p := range parts[1:] {
		head, body, ok := strings.Cut(p, "\n")
		if !ok {
			continue
		}
		if strings.HasPrefix(head, "(") {
			if _, rest, found := strings.Cut(head, ") "); found {
				head = rest
			}
		}
		name, _, _ := strings.Cut(head, "(")
		out[strings.TrimSpace(name)] = body
	}
	return out
}

// Chowning /usr/local/bin would let the tenant edit start.sh, which root PID 1
// re-runs after a resume.
func TestPrivilegeHandoverTouchesNoExecutablePrefix(t *testing.T) {
	h := privilegeHandover(sandboxRunUser)
	if h == "" {
		t.Fatal("privilegeHandover produced nothing for a non-root sandbox; the workspace would stay root-owned and the sandbox would boot read-only")
	}
	for _, forbidden := range []string{"/usr/local/bin", "/usr/local/lib", "/usr/bin", "/bin", "/etc", "/usr/sbin"} {
		if strings.Contains(h, "chown -R "+sandboxRunUser+" "+forbidden) {
			t.Errorf("privilegeHandover chowns %s to the tenant. Anything root executes from there is tenant-writable, "+
				"and Restore re-runs start.sh as root from a post-tenant snapshot image.", forbidden)
		}
	}
	if !strings.Contains(h, containerWorkdir) {
		t.Errorf("privilegeHandover does not chown %s; a dropped sandbox cannot write its own workspace", containerWorkdir)
	}
	if privilegeHandover("") != "" {
		t.Error("privilegeHandover emitted a chown for a root-class sandbox, which has no run user to hand anything to")
	}
}

// su/sudo are setuid and fail silently under no-new-privileges; envd would never appear.
func TestDropPrefixCannotBeUndone(t *testing.T) {
	p := dropPrefix(sandboxRunUser)
	if !strings.HasPrefix(p, "setpriv ") {
		t.Fatalf("dropPrefix = %q, want a setpriv invocation", p)
	}
	if !strings.Contains(p, "--inh-caps=-all") {
		t.Error("dropPrefix does not clear inheritable capabilities; the dropped process could keep the eight in sandboxCaps")
	}
	if !strings.Contains(p, "--reuid="+sandboxRunUID) {
		t.Errorf("dropPrefix = %q, want --reuid=%s", p, sandboxRunUID)
	}
	if dropPrefix("") != "" {
		t.Error("dropPrefix emitted a setpriv for a root-class sandbox")
	}
}

func TestUserSplitHandlesBothForms(t *testing.T) {
	if u, g := userPart(sandboxRunUser), groupPart(sandboxRunUser); u != sandboxRunUID || g != sandboxRunUID {
		t.Errorf("split(%q) = (%q, %q), want (%q, %q)", sandboxRunUser, u, g, sandboxRunUID, sandboxRunUID)
	}
	// The bare-name form, with no ":group" suffix.
	const bareName = "node"
	if u, g := userPart(bareName), groupPart(bareName); u != bareName || g != bareName {
		t.Errorf("split(%q) = (%q, %q), want both %q", bareName, u, g, bareName)
	}
}

func TestRunUserEnvKeepsToolchainsOffRootOwnedPaths(t *testing.T) {
	env := runUserEnv(sandboxRunUser)
	joined := strings.Join(env, "\n")
	for _, want := range []struct{ key, why string }{
		{"HOME=", "pip's --user fallback installs under $HOME; without a writable one `pip install` fails"},
		{"NPM_CONFIG_PREFIX=", "`npm i -g` targets /usr/local/lib/node_modules, which is root-owned"},
		{"CARGO_HOME=", "rust:slim ships CARGO_HOME=/usr/local/cargo, root-owned"},
		{"GOPATH=", "the golang image defaults GOPATH=/go, root-owned"},
	} {
		if !strings.Contains(joined, want.key) {
			t.Errorf("runUserEnv is missing %s — %s", want.key, want.why)
		}
	}
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			t.Error("runUserEnv sets PATH, which replaces the image's own and loses node/cargo/go/the Playwright browsers; execPathPrefix extends it from inside the container instead")
		}
		if !strings.Contains(e, "=") {
			t.Errorf("runUserEnv entry %q is not NAME=VALUE and docker will reject the boot", e)
		}
	}
	if runUserEnv("") != nil {
		t.Error("runUserEnv returned env for a root-class sandbox, changing behaviour for a class this task deliberately did not touch")
	}
	if execPathPrefix("") != "" {
		t.Error("execPathPrefix rewrote a root-class sandbox's command")
	}
	if p := execPathPrefix(sandboxRunUser); !strings.Contains(p, "$PATH") {
		t.Errorf("execPathPrefix = %q, want it to EXTEND $PATH rather than replace it", p)
	}
}

// envd's web terminal is the most reachable tenant surface; it must drop too.
func TestBootImageDropsEnvdToo(t *testing.T) {
	body := sourceContaining(t, "func (r *Runtime) bootImage(")
	fn := bootImageBody(t, body)
	envdIdx := strings.Index(fn, "/usr/local/bin/envd >/tmp/envd.log")
	if envdIdx < 0 {
		t.Fatal("the envd launch line has moved; this test no longer checks anything")
	}
	// The drop must be on the same command string, immediately before envd.
	window := fn[max0(envdIdx-80):envdIdx]
	if !strings.Contains(window, "dropPrefix(runUser)") {
		t.Errorf("envd is launched without dropPrefix; the web terminal spawns shells as uid 0.\n\tcontext: %q", window)
	}
	if !strings.Contains(fn, "handover") {
		t.Error("bootImage's launch command does not include the privilege handover; a dropped sandbox boots with a root-owned workspace")
	}
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// bootImageBody returns bootImage's source from the start of its declaration.
func bootImageBody(t *testing.T, body string) string {
	t.Helper()
	const marker = "func (r *Runtime) bootImage("
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("%q not found — bootImage was renamed, so this test is reading the wrong "+
			"function or nothing at all", marker)
	}
	return body[i:]
}

// API B24: the run uid had no passwd entry, so `id -un` failed in every docker
// sandbox. The handover names it once and leaves an image's own entry alone.
func TestPrivilegeHandoverNamesTheRunUser(t *testing.T) {
	etc := t.TempDir()
	passwd, group := filepath.Join(etc, "passwd"), filepath.Join(etc, "group")
	if err := os.WriteFile(passwd, []byte("root:x:0:0:root:/root:/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(group, []byte("root:x:0:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(passwdEntry(sandboxRunUser), "/etc/", etc+"/")
	for range 2 {
		if out, err := exec.Command("/bin/sh", "-c", script).CombinedOutput(); err != nil {
			t.Fatalf("passwd entry script: %v %s", err, out)
		}
	}
	for file, want := range map[string]string{passwd: ":" + sandboxRunUID + ":" + sandboxRunUID + ":", group: ":" + sandboxRunUID + ":"} {
		raw, _ := os.ReadFile(file)
		if n := strings.Count(string(raw), want); n != 1 {
			t.Errorf("%s has %d entries for uid %s after two boots, want 1:\n%s", filepath.Base(file), n, sandboxRunUID, raw)
		}
	}
	if !strings.Contains(privilegeHandover(sandboxRunUser), "/etc/passwd") {
		t.Error("the handover does not add the passwd entry")
	}
}
