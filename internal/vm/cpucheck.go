//go:build linux

package vm

import (
	"fmt"
	"strings"

	"github.com/pilldrops-oss/pilldrops/internal/hoststats"
	"github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// ErrCPUTooPoor: the capture was taken on a CPU that provides instructions this
// host does not.
//
// A memory snapshot is a frozen process image, and the processes in it made
// their CPUID-driven code-path choices before they were frozen -- the Go
// runtime picks its AVX2 paths at startup, glibc resolves its ifuncs at load.
// Restore that image on a poorer CPU and Firecracker loads it happily, reports
// a running sandbox, and the guest then dies on the first instruction the host
// does not implement. Measured on the two-node fleet: a Haswell capture
// restored on Ivy Bridge, 'load snapshot' succeeded in 3.8 ms, and the guest
// took SIGILL on c4 e2 7d 78 -- VEX.256 VPBROADCASTB, which is AVX2.
//
// The direction is everything: poorer-to-richer restores fine, richer-to-poorer
// is fatal, so the check is a subset test and not an equality one. A refusal
// the caller can read beats a guest that dies with SIGILL; placement filters on
// the same feature set so the refusal is the backstop rather than the norm.
//
// The sentinel lives in internal/runtime so the API layer can render it without
// importing the VM engine, exactly as ErrRootfsMismatch does.
var ErrCPUTooPoor = runtime.ErrCPUTooPoor

// checkRestorable is every compatibility guard a restore has to pass before a
// VMM is started: the root image the overlay is a delta against, and the
// instruction set the frozen processes chose their code paths from. Both
// callers of either check run both, so they are one function -- a restore path
// that ran one and not the other is the bug this shape prevents.
func (m *Manager) checkRestorable(meta sandboxSnapMeta, srcDir, rootfs string) error {
	if err := m.checkRootfs(meta, srcDir, rootfs); err != nil {
		return err
	}
	return checkCPU(meta, srcDir)
}

// checkCPU refuses a restore whose capture needs instructions this host lacks.
//
// A capture recording no features is let through: it predates this record, and
// refusing every one of them would strand snapshots that restore correctly on
// the node that took them -- the same allowance checkRootfs makes.
//
// A host whose own features could not be read is NOT a pass when the capture
// named some. "The answer is empty" and "the check did not run" must not look
// the same, which is the failure mode that let this defect exist at all.
func checkCPU(meta sandboxSnapMeta, srcDir string) error {
	if len(meta.CPUFeatures) == 0 {
		return nil
	}
	have := hoststats.CPUFeatures()
	if len(have) == 0 {
		return fmt.Errorf("%w: capture %s needs CPU features %s, and this host's own feature set "+
			"could not be read, so the restore cannot be shown to be safe. A guest restored onto a "+
			"poorer CPU dies with SIGILL rather than failing",
			ErrCPUTooPoor, srcDir, strings.Join(meta.CPUFeatures, ","))
	}
	missing := hoststats.MissingCPUFeatures(meta.CPUFeatures, have)
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%w: capture %s was taken on a CPU providing %s, and this host does not "+
		"implement %s. The guest's processes already chose code paths for those instructions "+
		"before they were frozen, so restoring here would return a running sandbox that dies "+
		"with SIGILL. Restore it on a node with at least the capturing node's instruction set",
		ErrCPUTooPoor, srcDir, strings.Join(meta.CPUFeatures, ","), strings.Join(missing, ","))
}
