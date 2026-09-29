package docker

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Storage-cap probes: a driver can accept --storage-opt size and silently not
// honour it (overlayfs on ext4 does), so the cap is verified by exceeding it.

const (
	capProbeSizeMB  = 64
	capProbeWriteMB = 192
	// capProbeTimeout yields Unknown, not a verdict, on a slow host.
	capProbeTimeout = 90 * time.Second
	// capProbeFileCeiling: XFS inodes cost 512 B of project quota, so a 64 MB
	// cap should stop near 131k files. 200k is unambiguous in both directions.
	capProbeFileCeiling = 200000
	// capProbeFileBatch is paths per touch; one process per file would eat the timeout.
	capProbeFileBatch = 2000
)

// StorageCapState is what a host's storage driver was observed to do.
type StorageCapState int

const (
	// StorageCapUnknown means the probe reached no verdict; never treat it as enforcement.
	StorageCapUnknown StorageCapState = iota
	// StorageCapEnforced means a write past the declared cap was stopped.
	StorageCapEnforced
	// StorageCapNotEnforced means the write succeeded or the driver rejected the option.
	StorageCapNotEnforced
)

// VerifyStorageCap creates a throwaway container with a small declared cap and
// writes past it. image should already be present locally. Direct IO matters:
// a buffered write is OOM-killed by the memory cgroup first, which looks like containment.
func VerifyStorageCap(ctx context.Context, image string) (StorageCapState, string, error) {
	ctx, cancel := context.WithTimeout(ctx, capProbeTimeout)
	defer cancel()

	// dd exits non-zero on ENOSPC, which is the pass case, hence || true.
	script := fmt.Sprintf(
		"dd if=/dev/zero of=/probe.bin bs=1M count=%d oflag=direct 2>&1 || true; "+
			"stat -c %%s /probe.bin 2>/dev/null || echo 0",
		capProbeWriteMB)

	args := []string{
		"run", "--rm",
		"--storage-opt", "size=" + strconv.Itoa(capProbeSizeMB) + "m",
		"--network", "none",
		"--entrypoint", "sh",
		image, "-c", script,
	}

	out, se, code, err := dockerOut(ctx, args...)
	if err != nil || code != 0 {
		// A rejected --storage-opt is a definitive "not enforced"; anything else is Unknown.
		if strings.Contains(strings.ToLower(se), "storage-opt") {
			return StorageCapNotEnforced, "storage driver rejected --storage-opt: " + strings.TrimSpace(se), nil
		}
		return StorageCapUnknown, strings.TrimSpace(se), fmt.Errorf("storage-cap probe did not run: %w", err)
	}

	lines := strings.Fields(strings.TrimSpace(out))
	if len(lines) == 0 {
		return StorageCapUnknown, "probe produced no output", nil
	}
	wroteBytes, perr := strconv.ParseInt(lines[len(lines)-1], 10, 64)
	if perr != nil {
		return StorageCapUnknown, "could not parse probe output: " + strings.TrimSpace(out), nil
	}

	wroteMB := wroteBytes / (1024 * 1024)
	// 20% headroom for filesystem overhead in the writable layer.
	if wroteMB > capProbeSizeMB*12/10 {
		return StorageCapNotEnforced,
			fmt.Sprintf("wrote %d MB into a declared %d MB cap — the driver accepts --storage-opt and does not honour it",
				wroteMB, capProbeSizeMB), nil
	}
	return StorageCapEnforced,
		fmt.Sprintf("write stopped at %d MB against a declared %d MB cap", wroteMB, capProbeSizeMB), nil
}

// BaseProbeImage is the node's own base image, already present, so the probe needs no pull.
func BaseProbeImage() string { return imageFor("") }

// VerifyStorageCap probes this host once and records an unenforced cap on the
// package counter. known is false when the probe reached no verdict.
func (r *Runtime) VerifyStorageCap(ctx context.Context) (enforced, known bool, detail string) {
	state, detail, err := VerifyStorageCap(ctx, BaseProbeImage())
	switch state {
	case StorageCapEnforced:
		// The file-count leg only informs detail. enforced stays a statement
		// about bytes, since productionNode keys on it.
		if bounded, known, fileDetail := VerifyFileCap(ctx, BaseProbeImage()); known && !bounded {
			return true, true, detail + "; FILE COUNT UNBOUNDED: " + fileDetail
		} else if known {
			return true, true, detail + "; " + fileDetail
		}
		return true, true, detail + "; file-count leg reached no verdict"
	case StorageCapNotEnforced:
		noteStorageCapUnenforced(capProbeSizeMB)
		return false, true, detail
	default:
		if err != nil && detail == "" {
			detail = err.Error()
		}
		return false, false, detail
	}
}

// StorageCapUnenforced implements rt.StorageCapReporter.
func (r *Runtime) StorageCapUnenforced() int64 { return StorageCapUnenforced() }

// fileCapVerdict turns a created-file count into a verdict. Pure, so the
// threshold is testable without a daemon. Reaching the ceiling means unbounded.
func fileCapVerdict(created, ceiling int) (bounded bool, detail string) {
	if created >= ceiling {
		return false, fmt.Sprintf(
			"created %d empty files inside a declared %d MB cap without being stopped — "+
				"empty files are not charged against the cap, so a sandbox can exhaust the host's "+
				"inode table while using almost no bytes",
			created, capProbeSizeMB)
	}
	return true, fmt.Sprintf("file creation stopped at %d files inside a declared %d MB cap",
		created, capProbeSizeMB)
}

// VerifyFileCap creates empty files in a throwaway container until the layer
// refuses or capProbeFileCeiling is reached. Empty files can exhaust the host
// inode table while using almost no bytes. known is false with no verdict.
func VerifyFileCap(ctx context.Context, image string) (bounded, known bool, detail string) {
	ctx, cancel := context.WithTimeout(ctx, capProbeTimeout)
	defer cancel()

	// The verdict reads the count, not touch's exit status; being cut short is the pass case.
	script := fmt.Sprintf(
		"mkdir -p /probe.d && cd /probe.d && "+
			"seq 1 %d | xargs -n %d touch 2>/dev/null || true; "+
			"ls -U | wc -l",
		capProbeFileCeiling, capProbeFileBatch)

	out, se, code, err := dockerOut(ctx, "run", "--rm",
		"--storage-opt", "size="+strconv.Itoa(capProbeSizeMB)+"m",
		"--network", "none",
		"--entrypoint", "sh",
		image, "-c", script)
	if err != nil || code != 0 {
		return false, false, strings.TrimSpace(se)
	}

	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) == 0 {
		return false, false, "probe produced no output"
	}
	created, perr := strconv.Atoi(fields[len(fields)-1])
	if perr != nil {
		return false, false, "could not parse probe output: " + strings.TrimSpace(out)
	}
	bounded, detail = fileCapVerdict(created, capProbeFileCeiling)
	return bounded, true, detail
}
