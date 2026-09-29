package docker

import (
	"strings"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// TestBootImageAppliesResourceLimitsAndNoNewPrivileges source-inspects bootImage
// to check the pids/nofile/no-new-privileges flags sit in the unconditional argv.
// docker exec sessions bypass envd's per-process rlimits, so these are the only guard.
func TestBootImageAppliesResourceLimitsAndNoNewPrivileges(t *testing.T) {
	body := sourceContaining(t, `func (r *Runtime) bootImage(`)

	fnStart := strings.Index(body, "func (r *Runtime) bootImage(")
	if fnStart < 0 {
		t.Fatal("bootImage has moved or been renamed; this test no longer checks anything")
	}
	// The last unconditional line before bootImage branches on service. The
	// first `if !service` is too early: the limit block sits after it.
	agentTokenIdx := strings.Index(body[fnStart:], "var agentToken, agentHash string")
	if agentTokenIdx < 0 {
		t.Fatal("the agentToken anchor has moved; this test no longer checks anything")
	}
	unconditional := body[fnStart : fnStart+agentTokenIdx]

	for _, want := range []string{
		`"--security-opt", "no-new-privileges"`,
		`"--pids-limit"`,
		`nofile=%d:%d`,
	} {
		if !strings.Contains(unconditional, want) {
			t.Errorf("bootImage's unconditional (pre-`if !service`) argv section does not contain %q — either it's missing or it moved into a template-conditional branch, which would leave service templates (postgres/mysql/mongo) unlimited", want)
		}
	}

	// RLIMIT_NPROC is per-uid host-wide, so a per-container nproc cap starves
	// every sandbox sharing uid 1001.
	if strings.Contains(body, `"--ulimit", fmt.Sprintf("nproc=`) {
		t.Error(`found --ulimit nproc in bootImage's source — RLIMIT_NPROC is enforced per-UID host-wide, so a per-container cap starves co-tenants sharing that uid`)
	}
}

// TestTenantSurfaceDoesNotRunAsRoot checks every tenant entry point passes
// --user. The drop is on docker exec and setpriv, not docker run: PID 1 must
// stay root to chown the workspace and let service entrypoints initialise.
func TestTenantSurfaceDoesNotRunAsRoot(t *testing.T) {
	for _, tc := range []struct{ marker, what string }{
		{"func (i *dockerInstance) Exec(", "POST /v1/sandboxes/{id}/exec"},
		{"func (i *dockerInstance) StartServer(", "the customer's own server process"},
		{"func (d *dockerFS) exec(", "the file API"},
	} {
		body := sourceContaining(t, tc.marker)
		if !strings.Contains(body, `"--user"`) {
			t.Errorf("%s (%s) is in a source file with no --user: tenant code there runs as uid 0, "+
				"and with no userns-remap on this dockerd that is host uid 0", tc.marker, tc.what)
		}
	}
	// `--user 0:0` would satisfy the check above and change nothing.
	if sandboxRunUID == "0" || sandboxRunUser == "0:0" || sandboxRunUser == "root" {
		t.Fatalf("sandboxRunUser = %q, which is root — the flag is present and the boundary is not", sandboxRunUser)
	}
}

// TestResourceLimitDefaultsMirrorEnvd pins the nofile default to envd's limitNofile.
func TestResourceLimitDefaultsMirrorEnvd(t *testing.T) {
	if defaultNofileLimit != 65536 {
		t.Errorf("defaultNofileLimit = %d, want 65536 to match cmd/envd/limits_linux.go's limitNofile", defaultNofileLimit)
	}
	if defaultPidsLimit <= 0 || defaultPidsLimit > 4096 {
		t.Errorf("defaultPidsLimit = %d, want a positive backstop well below a four-digit runaway ceiling", defaultPidsLimit)
	}
}

// A zero BootSpec must fall through to the package defaults, not request "no limit".
func TestBootSpecResourceLimitFieldsDefaultToZero(t *testing.T) {
	var spec rt.BootSpec
	if spec.PidsLimit != 0 || spec.NofileLimit != 0 {
		t.Fatalf("BootSpec resource-limit fields are not zero-valued by default: %+v", spec)
	}
}

// TestBootImageDisablesIPv6InTheSandbox: every egress rule on a Docker node is
// iptables/IPv4, so an IPv6 stack in the container would be unfiltered.
func TestBootImageDisablesIPv6InTheSandbox(t *testing.T) {
	body := sourceContaining(t, `func (r *Runtime) bootImage(`)
	fnStart := strings.Index(body, "func (r *Runtime) bootImage(")
	agentTokenIdx := strings.Index(body[fnStart:], "var agentToken, agentHash string")
	if fnStart < 0 || agentTokenIdx < 0 {
		t.Fatal("bootImage or its agentToken anchor has moved; this test no longer checks anything")
	}
	unconditional := body[fnStart : fnStart+agentTokenIdx]

	if !strings.Contains(unconditional, `net.ipv6.conf.all.disable_ipv6=1`) {
		t.Error("bootImage does not disable IPv6 in the container. Every egress rule on a " +
			"Docker node is iptables/IPv4; nothing in this repo writes an ip6tables rule, so " +
			"the container's IPv6 stack is unfiltered end to end.")
	}
	// On a kernel without IPv6 the sysctl key is absent and docker run fails.
	if !strings.Contains(unconditional, "hostHasIPv6()") {
		t.Error("the disable_ipv6 sysctl is not gated on hostHasIPv6(). On a kernel built " +
			"without IPv6 the sysctl key is absent and docker run exits non-zero, so every " +
			"create on that node fails.")
	}
}

func TestHostHasIPv6ReadsTheKernel(t *testing.T) {
	body := sourceContaining(t, "func hostHasIPv6()")
	if !strings.Contains(body, "/proc/net/if_inet6") {
		t.Error("hostHasIPv6 no longer consults /proc/net/if_inet6, the file the IPv6 module " +
			"creates — any other source can disagree with whether the stack is actually up")
	}
}
