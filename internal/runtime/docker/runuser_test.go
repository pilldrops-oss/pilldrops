package docker

import (
	"strings"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

func TestRootIsTheExceptionNotTheDefault(t *testing.T) {
	if r := rootReason(false, nil, ""); r != "" {
		t.Fatalf("a plain template is excluded from the drop with reason %q — the class this finding is about is the one that must NOT be excluded", r)
	}
	if u := dropUser(false, nil, false, ""); u != sandboxRunUser {
		t.Fatalf("dropUser(plain) = %q, want %q", u, sandboxRunUser)
	}
}

func TestDeviceTemplatesRunAsTheBuiltinsOwnUser(t *testing.T) {
	// service=true is the real call shape: a device builtin is also a service,
	// so this catches the device branch being ordered after the service branch.
	if r := rootReason(true, []string{"/dev/kvm"}, androidRunUser); r != "" {
		t.Fatalf("a device builtin that declares a run user is still excluded from the drop: %q", r)
	}
	if u := dropUser(true, []string{"/dev/kvm"}, false, androidRunUser); u != androidRunUser {
		t.Errorf("dropUser(device) = %q, want %q — the identity the image itself declares", u, androidRunUser)
	}
	if androidRunUser == "" || androidRunUser == "0:0" || androidRunUser == "root" {
		t.Errorf("androidRunUser = %q, which is root", androidRunUser)
	}

	// A device builtin with no declared identity falls back to root with a reason, not a guessed uid.
	r := rootReason(false, []string{"/dev/kvm"}, "")
	if r == "" {
		t.Fatal("a device builtin with no declared run user was dropped to some uid anyway; the fallback is supposed to be explicit")
	}
	if !strings.Contains(r, "device") {
		t.Errorf("rootReason for an undeclared device template = %q, want a reason naming the device passthrough", r)
	}
	if u := dropUser(false, []string{"/dev/kvm"}, false, ""); u != "" {
		t.Errorf("dropUser(device, no declared user) = %q, want \"\"", u)
	}

	// With no devices, deviceUser must be ignored or a tenant could pick the uid by template name.
	if u := dropUser(false, nil, false, androidRunUser); u != sandboxRunUser {
		t.Errorf("dropUser(no devices, deviceUser=%q) = %q, want %q — a builtin's identity leaked to a "+
			"request that gets no device", androidRunUser, u, sandboxRunUser)
	}
	if u := dropUser(true, nil, false, androidRunUser); u != "" {
		t.Errorf("dropUser(service, no devices, deviceUser set) = %q, want \"\"", u)
	}
}

func TestEveryDeviceBearingBuiltinDeclaresANonRootUser(t *testing.T) {
	devicesSeen := 0
	for name, bs := range builtinServices {
		if len(bs.devices) == 0 {
			continue
		}
		devicesSeen++
		if bs.runUser == "" {
			t.Errorf("builtin %q is granted %v but declares no runUser, so its container and its "+
				"whole tenant surface run as uid 0 with the host device mapped in", name, bs.devices)
		}
		if userPart(bs.runUser) == "0" || bs.runUser == "root" {
			t.Errorf("builtin %q declares runUser %q, which is root", name, bs.runUser)
		}
		if got := builtinRunUser(name); got != bs.runUser {
			t.Errorf("builtinRunUser(%q) = %q, want %q — bootImage would apply the wrong identity", name, got, bs.runUser)
		}
	}
	if devicesSeen == 0 {
		t.Fatal("no device-bearing builtin found; this test is not looking at the map bootImage reads")
	}
}

// --user without --group-add leaves the emulator unable to open /dev/kvm (0660
// root:kvm), which presents as slowness rather than failure.
func TestDeviceTemplatesGetAPinnedUserAndTheDeviceGroup(t *testing.T) {
	body := sourceContaining(t, "func (r *Runtime) bootImage(")
	fn := bootImageBody(t, body)

	if !strings.Contains(fn, `base = append(base, "--user", runUser)`) {
		t.Error("bootImage never passes --user on the docker run. A device builtin runs the IMAGE's own " +
			"entrypoint, so there is no sh -c prologue to drop from and the container's identity is " +
			"whatever the image (or a node-baked warm image) happens to declare.")
	}
	if !strings.Contains(fn, `"--group-add", gid`) {
		t.Error("bootImage does not add the device's owning group; a non-root container cannot open a " +
			"0660 root:kvm device and falls back to software emulation silently")
	}
	if !strings.Contains(fn, "deviceGroup(d)") {
		t.Error("the group is not derived from the device itself; a hard-coded gid is wrong on any node " +
			"whose kvm group is not that number")
	}
	// An empty runUser would emit `--user ` and fail the boot.
	if !strings.Contains(fn, `if service && runUser != "" {`) {
		t.Error("the --user is not gated on a non-empty runUser")
	}
}

// --group-add resolves names against the container's /etc/group, where the host's "kvm" does not exist.
func TestDeviceGroupIsNumericAndSkipsRoot(t *testing.T) {
	if g := deviceGroup("/nonexistent/device/xyz"); g != "" {
		t.Errorf("deviceGroup on a missing path = %q, want \"\"", g)
	}
	if g := deviceGroup("/dev/null"); g == "0" {
		t.Error("deviceGroup returned gid 0; that adds the root group to the container")
	}
	if g := deviceGroup("/dev/null"); g != "" {
		for _, c := range g {
			if c < '0' || c > '9' {
				t.Fatalf("deviceGroup = %q, which is not numeric; --group-add would resolve it inside the container and fail", g)
			}
		}
	}
}

func TestServiceTemplatesStayRoot(t *testing.T) {
	if rootReason(true, nil, "") == "" {
		t.Fatal("an image-backed service template was dropped to a non-root uid; its entrypoint cannot initialise and postgres/mysql first boot breaks")
	}
	if u := dropUser(true, nil, false, ""); u != "" {
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

// agentd chowns the workspace to `node`, so a second uid on the exec surface
// could not touch the files the agent wrote.
func TestAgentTemplatesKeepTheirOwnIdentity(t *testing.T) {
	if u := dropUser(false, nil, true, ""); u != agentRunUser {
		t.Errorf("dropUser(agent) = %q, want %q", u, agentRunUser)
	}
	if agentRunUser == sandboxRunUser {
		t.Error("agentRunUser and sandboxRunUser are the same identity; the split this test guards no longer exists")
	}
}

// TestEveryContainerEntryPointDropsPrivilege enumerates every function that
// builds a docker exec, so a new one must either drop or be listed in
// rootByDesign. Scanned per function because argv is assembled across statements.
func TestEveryContainerEntryPointDropsPrivilege(t *testing.T) {
	rootByDesign := map[string]string{
		"DiskUsageMB":   "a platform storage meter; `du` of the workspace, no tenant input",
		"LiveStats":     "the same du, on the metrics path",
		"Signal":        "must outrank the dropped process it is signalling",
		"OOMKillCount":  "reads the container's own cgroup memory.events counter",
		"Procs":         "docker top; a platform read",
		"injectHarness": "harness templates are device-bearing (mobile-test), i.e. an already-excluded class, and it explicitly passes -u 0",
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
	if u, g := userPart(agentRunUser), groupPart(agentRunUser); u != agentRunUser || g != agentRunUser {
		t.Errorf("split(%q) = (%q, %q), want both %q", agentRunUser, u, g, agentRunUser)
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
