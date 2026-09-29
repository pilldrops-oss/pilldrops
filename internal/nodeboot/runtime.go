package nodeboot

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/docker"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/firecracker"
	"github.com/pilldrops-oss/pilldrops/internal/templates"
	"go.uber.org/zap"
)

// SelectRuntime picks the runtime and reports whether it isolates.
// SANDBOXD_RUNTIME=docker|process forces one; otherwise Firecracker is probed,
// then Docker. May return (nil, false), so run IsolationGuardError before use.
//
// ctx is the node's lifetime: the Firecracker path starts background monitors
// under it, so they stop when the node does.
func SelectRuntime(ctx context.Context, dataDir string, logger *zap.Logger) (rt.Runtime, bool) {
	switch strings.ToLower(Env("SANDBOXD_RUNTIME", "")) {
	case "docker":
		if dr, err := docker.New(); err == nil {
			slog.Info("using the Docker runtime: real per-image isolation, local",
				"component", "runtime", "selected_by", "SANDBOXD_RUNTIME=docker")
			return dr, true
		} else {
			slog.Warn("Docker was requested but is unavailable",
				"component", "runtime", "err", err, "dev_build", DevBuild)
			return unisolated(dataDir, "SANDBOXD_RUNTIME=docker (unavailable)")
		}
	case "process":
		return unisolated(dataDir, "SANDBOXD_RUNTIME=process")
	}
	slog.Info("probing machine type: Firecracker (KVM), then Docker",
		"component", "runtime", "selected_by", "SANDBOXD_RUNTIME=auto", "dev_build", DevBuild)
	fcBin := Env("FIRECRACKER_BIN", "/usr/bin/firecracker")
	kernel := Env("KERNEL_PATH", filepath.Join(dataDir, "vmlinux"))
	_, binErr := os.Stat(fcBin)
	_, kernErr := os.Stat(kernel)
	kvmOK := false
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err == nil {
		_ = f.Close()
		kvmOK = true
	}
	// A KVM host missing the binary or kernel falls through to Docker.
	if kvmOK && binErr == nil && kernErr == nil {
		snapDir := Env("SNAPSHOT_DIR", filepath.Join(dataDir, "templates"))
		templates.SetDir(snapDir)
		// Per-template defaults; a request may override, with VM_MEMORY_MB and
		// VM_VCPUS as the final fallback.
		tmplRes := map[string]rt.Resources{}
		for _, t := range templates.Catalog() {
			tmplRes[t.ID] = rt.Resources{VCPUs: t.CPU, MemoryMB: t.MemoryMB, StorageMB: t.StorageMB}
		}
		envdAuth := Env("ENVD_AUTH", "1") == "1"
		fc, ferr := firecracker.New(ctx, firecracker.Config{
			SnapshotDir:    snapDir,
			OverlayDir:     Env("OVERLAY_DIR", filepath.Join(dataDir, "overlays")),
			FirecrackerBin: Env("FIRECRACKER_BIN", "/usr/bin/firecracker"),
			JailerBin:      Env("JAILER_BIN", "/usr/bin/jailer"),
			KernelPath:     Env("KERNEL_PATH", filepath.Join(dataDir, "vmlinux")),
			Templates:      TemplateList(Env("TEMPLATE", "base")),
			MaxConcurrent:  10,
			MaxActive:      EnvInt("VM_MAX_ACTIVE", 50),
			MemoryMB:       EnvInt("VM_MEMORY_MB", 1024),
			VCPUs:          EnvInt("VM_VCPUS", 1),
			Resources:      tmplRes,
			// Golden-snapshot restore is on by default; FC_SNAPSHOTS=0 opts a node out.
			EnableSnapshots: EnvBool("FC_SNAPSHOTS", true),
			// ENVD_AUTH=0 is a dev escape hatch, not a working "disable auth":
			// envd still refuses every method until a credential is pushed.
			EnvdAuth:   envdAuth,
			NonRootVMM: NonRootVMMRequested(),
			NetNS:      NetNSRequested(),
			Logger:     logger,
		})
		if ferr == nil {
			slog.Info("/dev/kvm, the firecracker binary and a kernel are all present; "+
				"using Firecracker for real microVM isolation", "component", "runtime")
			return fc, true
		}
		// Fail closed: this host is provisioned for Firecracker, so an init
		// failure is a broken dependency, not a reason to silently degrade to
		// Docker's weaker boundary. A host that wants Docker says so explicitly.
		slog.Error("Firecracker is provisioned on this host (/dev/kvm + binary + kernel) but failed to "+
			"initialise — refusing to degrade to a weaker runtime for an isolation-affecting cause. "+
			"Fix the dependency named below, or select a runtime explicitly with SANDBOXD_RUNTIME=docker",
			"component", "runtime", "err", ferr)
		return unisolated(dataDir, "firecracker provisioned but failed to initialise (fail closed)")
	}
	slog.Info("Firecracker not selected; trying Docker", "component", "runtime",
		"kvm", kvmOK, "firecracker_bin", binErr == nil, "kernel", kernErr == nil)
	if dr, err := docker.New(); err == nil {
		slog.Info("using the Docker runtime: real per-image isolation", "component", "runtime")
		return dr, true
	}
	return unisolated(dataDir, "auto-probe found neither Firecracker nor Docker")
}

// unisolated returns the process runtime in a dev build and nil in a release
// build; IsolationGuardError refuses both unless SANDBOXD_DEV=1 on dev.
func unisolated(dataDir, why string) (rt.Runtime, bool) {
	r := unisolatedRuntime(filepath.Join(dataDir, "sandboxes"))
	if r == nil {
		slog.Error("no isolated runtime is available, and this binary ships none that runs without one",
			"component", "runtime", "reason", why, "dev_build", DevBuild)
		return nil, false
	}
	slog.Warn("using the process runtime: NO isolation, dev only",
		"component", "runtime", "reason", why)
	return r, false
}
