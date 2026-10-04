package docker

import (
	"strconv"
	"strings"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// mustNotHold is the security claim; sandboxCaps is checked against it rather
// than the other way round.
var mustNotHold = []string{
	"NET_RAW", "MKNOD", "SETFCAP", "SETPCAP", "SYS_CHROOT", "AUDIT_WRITE",
	// Not in Docker's default set either; listed so widening sandboxCaps has to edit this list.
	"SYS_ADMIN", "SYS_PTRACE", "SYS_MODULE", "DAC_READ_SEARCH", "SYS_RAWIO",
}

func TestSandboxCapsGrantsNothingDangerous(t *testing.T) {
	held := map[string]bool{}
	for _, c := range sandboxCaps {
		held[strings.ToUpper(c)] = true
	}
	for _, bad := range mustNotHold {
		if held[bad] {
			t.Errorf("sandboxCaps grants %s — see mustNotHold in caps_test.go for why it must not", bad)
		}
	}
	if len(sandboxCaps) == 0 {
		t.Fatal("sandboxCaps is empty: --cap-drop=ALL with no adds breaks the agent boot chown")
	}
}

// TestBootImageDropsAllCapabilities source-inspects bootImage, which talks to
// the daemon and has no pure argv builder to test.
func TestBootImageDropsAllCapabilities(t *testing.T) {
	body := sourceContaining(t, `func (r *Runtime) bootImage(`)

	i := strings.Index(body, "func (r *Runtime) bootImage(")
	if i < 0 {
		t.Fatal("bootImage not found in docker.go")
	}
	fn := body[i:]

	if !strings.Contains(fn, `"--cap-drop", "ALL"`) {
		t.Error(`bootImage does not pass --cap-drop=ALL; every capability Docker grants by default is held`)
	}
	if !strings.Contains(fn, "range sandboxCaps") {
		t.Error("bootImage does not add sandboxCaps back after the drop")
	}
	// The drop must be unconditional: its line sits at bootImage's own
	// indentation (one tab), so any enclosing if would indent it further.
	for _, line := range strings.Split(fn, "\n") {
		if strings.Contains(line, `"--cap-drop", "ALL"`) {
			if !strings.HasPrefix(line, "\tbase") {
				t.Errorf("--cap-drop=ALL is applied inside a conditional (%q). It was carved out for "+
					"device templates once already, on a reason that turned out to be false; "+
					"unconditional is the property.", strings.TrimSpace(line))
			}
		}
	}
}

func TestBootImageHasOneCapabilityPolicy(t *testing.T) {
	fn := sourceContaining(t, "func (r *Runtime) bootImage(")
	const marker = "func (r *Runtime) bootImage("
	i := strings.Index(fn, marker)
	if i < 0 {
		t.Fatalf("%q not found — bootImage was renamed and this test is now reading the "+
			"wrong function, or nothing at all", marker)
	}
	body := fn[i:]
	if n := strings.Count(body, `"--cap-drop"`); n != 1 {
		t.Errorf("bootImage emits --cap-drop %d times, want exactly 1; a second one means a second policy", n)
	}
	if n := strings.Count(body, "range sandboxCaps"); n != 1 {
		t.Errorf("bootImage ranges over sandboxCaps %d times, want exactly 1", n)
	}
	if strings.Contains(body, "deviceCaps") {
		t.Error("bootImage uses a separate capability list for device templates; sandboxCaps is the reviewed set")
	}
}

// TestLiveSandboxCannotHoldDangerousCapabilities boots a real sandbox and reads
// the kernel's view of its capabilities.
func TestLiveSandboxCannotHoldDangerousCapabilities(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Skipf("docker runtime unavailable: %v", err)
	}
	ctx := t.Context()

	inst, err := r.Boot(ctx, rt.BootSpec{
		SandboxID: liveSandboxID(t, "sb-caps"),
		Template:  rt.Template{Name: "base", Image: "alpine:3"},
		VCPU:      1, MemMiB: 256,
	})
	if err != nil {
		t.Skipf("boot: %v", err)
	}

	run := func(cmd string) string {
		res, err := inst.Exec(ctx, rt.ExecSpec{Cmd: cmd})
		if err != nil {
			t.Fatalf("exec %q: %v", cmd, err)
		}
		return strings.TrimSpace(res.Stdout)
	}

	// CapBnd is the bounding set, the ceiling any process here can regain.
	raw := run("grep CapBnd /proc/self/status | awk '{print $2}'")
	bnd, err := strconv.ParseUint(raw, 16, 64)
	if err != nil {
		t.Fatalf("parse CapBnd %q: %v", raw, err)
	}
	for _, bad := range mustNotHold {
		bit, ok := capBits[bad]
		if !ok {
			t.Fatalf("no bit number recorded for %s", bad)
		}
		if bnd&(1<<bit) != 0 {
			t.Errorf("sandbox holds %s (CapBnd=%s) — it is in the bounding set and can be regained", bad, raw)
		}
	}

	if out := run("mknod /tmp/probe b 8 0 2>/dev/null && echo YES || echo no"); out != "no" {
		t.Errorf("device-node creation available in sandbox (MKNOD): %q", out)
	}
	if out := run("chroot / /bin/true 2>/dev/null && echo YES || echo no"); out != "no" {
		t.Errorf("chroot available in sandbox (SYS_CHROOT): %q", out)
	}

	// No ping probe for NET_RAW: Docker's default ping_group_range lets busybox
	// ping use an unprivileged ICMP socket, so ping succeeds with NET_RAW dropped.
}

// capBits maps capability names to their CapBnd bit (linux/capability.h).
var capBits = map[string]int{
	"DAC_READ_SEARCH": 2,
	"SETPCAP":         8,
	"MKNOD":           27,
	"AUDIT_WRITE":     29,
	"SETFCAP":         31,
	"NET_RAW":         13,
	"SYS_MODULE":      16,
	"SYS_RAWIO":       17,
	"SYS_CHROOT":      18,
	"SYS_PTRACE":      19,
	"SYS_ADMIN":       21,
}
