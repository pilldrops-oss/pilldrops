//go:build linux

package vm

// The jailer moves the VMM into <cgroupRoot>/firecracker/<id>, but the SDK's
// JailerConfig cannot pass --cgroup limits, so the leaf is created and limited
// here before the jailer runs. Limits written after Start would miss the boot.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cgroupRoot is the cgroup-v2 mount point; a var so tests can redirect it.
var cgroupRoot = "/sys/fs/cgroup"

// vmmCgroupParent is where the jailer creates per-VM leaves: <root>/<basename of --exec-file>.
func vmmCgroupParent() string { return filepath.Join(cgroupRoot, "firecracker") }

// vmmCgroupPath is the leaf cgroup for one VMM, keyed by the jailer --id (the sandbox id).
func vmmCgroupPath(jailerID string) string { return filepath.Join(vmmCgroupParent(), jailerID) }

const (
	// vmmMemOverheadMB covers the VMM heap plus the page cache charged to the
	// cgroup (rootfs, overlay, restored memory file). Generous on purpose.
	vmmMemOverheadMB = 512

	// vmmPidsOverhead covers the VMM's non-vCPU threads; pids.max counts threads.
	vmmPidsOverhead = 32

	// cpuPeriodUS is the kernel default cpu.max period.
	cpuPeriodUS = 100000

	// vmmCPUHeadroomUS is extra quota for the VMM's I/O and API threads, so a
	// guest pegging every vCPU cannot starve device emulation.
	vmmCPUHeadroomUS = 50000
)

// vmmLimits is one VMM's cgroup-v2 ceiling.
type vmmLimits struct {
	MemoryMaxBytes int64
	PidsMax        int64
	// CPUMax is the raw cpu.max value: "<quota_us> <period_us>".
	CPUMax string
}

// vmmCgroupLimits derives the ceiling from the resolved vcpu/mem, not the raw
// request, so a VM that fell back to the default gets a matching ceiling.
func vmmCgroupLimits(memMB, vcpus int64) vmmLimits {
	if memMB <= 0 {
		memMB = 1024
	}
	if vcpus <= 0 {
		vcpus = 1
	}
	return vmmLimits{
		MemoryMaxBytes: (memMB + vmmMemOverheadMB) * 1024 * 1024,
		PidsMax:        vcpus + vmmPidsOverhead,
		CPUMax:         fmt.Sprintf("%d %d", vcpus*cpuPeriodUS+vmmCPUHeadroomUS, cpuPeriodUS),
	}
}

// vmmCgroupControllers must be delegated at every level above the leaf or the
// leaf has no memory.max/pids.max/cpu.max files; the jailer's firecracker/
// directory only delegates cpuset.
var vmmCgroupControllers = []string{"memory", "pids", "cpu"}

// prepareVMMCgroup creates the VM's cgroup leaf and writes its limits before
// the jailer runs. Fails closed, except on a host with no unified hierarchy,
// where the jailer refuses to run anyway.
func prepareVMMCgroup(jailerID string, memMB, vcpus int64) error {
	if jailerID == "" {
		return fmt.Errorf("vmm cgroup: empty jailer id")
	}
	if _, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers")); err != nil {
		return nil
	}

	if err := os.MkdirAll(vmmCgroupParent(), 0o755); err != nil {
		return fmt.Errorf("vmm cgroup: create %s: %w", vmmCgroupParent(), err)
	}
	// Delegate root -> firecracker/, then firecracker/ -> the leaves.
	for _, dir := range []string{cgroupRoot, vmmCgroupParent()} {
		if err := delegateControllers(dir, vmmCgroupControllers); err != nil {
			return err
		}
	}

	leaf := vmmCgroupPath(jailerID)
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		return fmt.Errorf("vmm cgroup: create %s: %w", leaf, err)
	}

	lim := vmmCgroupLimits(memMB, vcpus)
	for name, want := range map[string]string{
		"memory.max": fmt.Sprintf("%d", lim.MemoryMaxBytes),
		"pids.max":   fmt.Sprintf("%d", lim.PidsMax),
		"cpu.max":    lim.CPUMax,
	} {
		path := filepath.Join(leaf, name)
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			return fmt.Errorf("vmm cgroup: write %s: %w", path, err)
		}
		// Read back; a cgroup write can be accepted and then clamped.
		got, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("vmm cgroup: read back %s: %w", path, err)
		}
		if strings.TrimSpace(string(got)) != want {
			return fmt.Errorf("vmm cgroup: %s is %q after writing %q", path, strings.TrimSpace(string(got)), want)
		}
	}
	return nil
}

// delegateControllers enables each controller in dir's cgroup.subtree_control.
// A controller missing from dir's cgroup.controllers is an error, since the
// leaf would silently have no limit file.
func delegateControllers(dir string, controllers []string) error {
	available, err := readCgroupSet(filepath.Join(dir, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("vmm cgroup: read %s/cgroup.controllers: %w", dir, err)
	}
	enabled, err := readCgroupSet(filepath.Join(dir, "cgroup.subtree_control"))
	if err != nil {
		return fmt.Errorf("vmm cgroup: read %s/cgroup.subtree_control: %w", dir, err)
	}
	for _, c := range controllers {
		if enabled[c] {
			continue
		}
		if !available[c] {
			return fmt.Errorf("vmm cgroup: controller %q is not available in %s (cgroup.controllers); "+
				"its parent has not delegated it, so no limit can be set on the VMM", c, dir)
		}
		path := filepath.Join(dir, "cgroup.subtree_control")
		if err := os.WriteFile(path, []byte("+"+c), 0o644); err != nil {
			return fmt.Errorf("vmm cgroup: enable %q in %s: %w", c, path, err)
		}
		now, err := readCgroupSet(path)
		if err != nil || !now[c] {
			return fmt.Errorf("vmm cgroup: %q still not enabled in %s after writing +%s", c, path, c)
		}
	}
	return nil
}

// readCgroupSet reads a space-separated cgroup controller list into a set.
func readCgroupSet(path string) (map[string]bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, f := range strings.Fields(string(b)) {
		set[strings.TrimPrefix(f, "+")] = true
	}
	return set, nil
}
