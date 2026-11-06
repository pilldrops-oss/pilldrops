//go:build linux

package vm

import (
	"os"
	"testing"

	"github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// newTestFCVM has just enough set for the pure config builders. NonRootVMM is
// left zero so every test sets it explicitly.
func newTestFCVM(t *testing.T) *FirecrackerConfig {
	t.Helper()
	return &FirecrackerConfig{
		SandboxID:   "sb-test-0001",
		Template:    "python-3.11",
		TapName:     "tap-test0",
		OverlayPath: "/tmp/test-overlay.img",
		StatePath:   "/tmp/test-state",
		MemPath:     "/tmp/test-mem",
		VCPUs:       1,
		MemoryMB:    512,
		Config: Config{
			SnapshotDir:    "/tmp/snapshots",
			FirecrackerBin: "/usr/bin/firecracker",
			JailerBin:      "/usr/bin/jailer",
			KernelPath:     "/tmp/vmlinux",
		},
	}
}

// The assertion is equality with uidForSandbox(id), not non-zero: a hardcoded
// constant is also non-zero and would give every sandbox the same uid.
func TestBuildColdConfigDerivesTheUnprivilegedUIDFromTheConfig(t *testing.T) {
	fc := newTestFCVM(t)
	fc.Config.NonRootVMM = true

	cfg := buildColdConfig(fc, "console=ttyS0", "/run/x.sock", nil, nil, nil)

	if !cfg.Seccomp.Enabled {
		t.Fatal("cold config must enable seccomp (no --no-seccomp)")
	}
	if cfg.JailerCfg == nil || cfg.JailerCfg.UID == nil || cfg.JailerCfg.GID == nil {
		t.Fatalf("cold config has no jailer uid/gid at all: %+v", cfg.JailerCfg)
	}
	want := uidForSandbox(fc.SandboxID)
	if got := *cfg.JailerCfg.UID; got != want {
		t.Errorf("jailer UID = %d, want %d (uidForSandbox(%q)) — the VMM is not running "+
			"as this sandbox's own unprivileged user", got, want, fc.SandboxID)
	}
	if got := *cfg.JailerCfg.GID; got != want {
		t.Errorf("jailer GID = %d, want %d — the tap is owned by uid:uid (buildTuntap), and "+
			"the kernel's tun_not_capable check fails on a group mismatch even when the "+
			"owner matches, so a wrong gid means the VMM cannot open its tap", got, want)
	}

	// Two sandboxes must not share a uid or the overlay chown stops separating them.
	other := newTestFCVM(t)
	other.Config.NonRootVMM = true
	other.SandboxID = "sb-test-0002"
	otherCfg := buildColdConfig(other, "console=ttyS0", "/run/y.sock", nil, nil, nil)
	if *otherCfg.JailerCfg.UID == *cfg.JailerCfg.UID {
		t.Errorf("two different sandboxes both got uid %d — the uid is not derived per sandbox",
			*cfg.JailerCfg.UID)
	}
}

func TestBuildRestoreConfigDerivesTheUnprivilegedUIDFromTheConfig(t *testing.T) {
	fc := newTestFCVM(t)
	fc.Config.NonRootVMM = true

	cfg := buildRestoreConfig(fc, "/run/x.sock", nil, nil)

	if !cfg.Seccomp.Enabled {
		t.Fatal("restore config must enable seccomp")
	}
	if cfg.JailerCfg == nil || cfg.JailerCfg.UID == nil || cfg.JailerCfg.GID == nil {
		t.Fatalf("restore config has no jailer uid/gid at all: %+v", cfg.JailerCfg)
	}
	want := uidForSandbox(fc.SandboxID)
	if got := *cfg.JailerCfg.UID; got != want {
		t.Errorf("jailer UID = %d, want %d — a resumed VMM must come back as the same "+
			"unprivileged user that owns its overlay and tap", got, want)
	}
	if got := *cfg.JailerCfg.GID; got != want {
		t.Errorf("jailer GID = %d, want %d", got, want)
	}
}

// Negative control for the tests above: a builder that ignored NonRootVMM and
// always derived a uid would still pass them.
func TestBothBuildersHonourTheRootEscapeHatchIdentically(t *testing.T) {
	for _, nonRoot := range []bool{true, false} {
		fc := newTestFCVM(t)
		fc.Config.NonRootVMM = nonRoot

		cold := buildColdConfig(fc, "console=ttyS0", "/run/x.sock", nil, nil, nil)
		restore := buildRestoreConfig(fc, "/run/x.sock", nil, nil)

		if *cold.JailerCfg.UID != *restore.JailerCfg.UID {
			t.Errorf("NonRootVMM=%v: cold boot uid %d != restore uid %d for the same sandbox",
				nonRoot, *cold.JailerCfg.UID, *restore.JailerCfg.UID)
		}
		if root := *cold.JailerCfg.UID == 0; root != !nonRoot {
			t.Errorf("NonRootVMM=%v produced uid %d: the gate is not being read",
				nonRoot, *cold.JailerCfg.UID)
		}
		// Seccomp is independent of the uid and must survive either setting.
		if !cold.Seccomp.Enabled || !restore.Seccomp.Enabled {
			t.Errorf("NonRootVMM=%v turned seccomp off; it is unconditional", nonRoot)
		}
	}
}

// A signal to the daemon must not reach its guests: the SDK forwards the
// default set when ForwardSignals is nil, which killed every VM on a node
// restart before the shutdown could pause it.
func TestNoSignalIsForwardedToFirecracker(t *testing.T) {
	fc := newTestFCVM(t)
	for name, got := range map[string]interface{ Len() int }{
		"cold":    sigs(buildColdConfig(fc, "console=ttyS0", "/run/x.sock", nil, nil, nil).ForwardSignals),
		"restore": sigs(buildRestoreConfig(fc, "/run/x.sock", nil, nil).ForwardSignals),
	} {
		if got == nil || got.Len() != 0 {
			t.Errorf("%s config forwards signals to Firecracker (nil means the SDK's default set)", name)
		}
	}
}

type sigList []os.Signal

func (s sigList) Len() int { return len(s) }

// sigs is nil for a nil list, so the test can tell "unset" from "empty".
func sigs(s []os.Signal) interface{ Len() int } {
	if s == nil {
		return nil
	}
	return sigList(s)
}

// A golden restore names no size. Its guest has the template's memory from the
// snapshot, so the cgroup ceiling must not fall back to the node default: that
// killed a 2 GB guest at 1.5 GB.
func TestResolveUsesTemplateResourcesWhenTheRequestNamesNone(t *testing.T) {
	fc := newTestFCVM(t)
	fc.VCPUs, fc.MemoryMB = 0, 0
	fc.Template = "hermes"
	fc.Config.VCPUs, fc.Config.MemoryMB = 1, 1024
	fc.Config.Resources = map[string]runtime.Resources{"hermes": {VCPUs: 2, MemoryMB: 2048}}

	vcpus, memMB, _ := resolveVCPUAndMem(fc)
	if vcpus != 2 || memMB != 2048 {
		t.Fatalf("resolved %d vCPU, %d MB; want the template's 2 vCPU, 2048 MB", vcpus, memMB)
	}
}
