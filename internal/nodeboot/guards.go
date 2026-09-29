package nodeboot

import (
	"context"
	"errors"
	"strings"
)

// ProductionDBError refuses SANDBOXD_PRODUCTION=1 on anything but Postgres.
func ProductionDBError(production bool, dsn string) error {
	if production && !strings.HasPrefix(dsn, "postgres") {
		return errors.New("SANDBOXD_PRODUCTION=1 requires a Postgres SANDBOXD_DATABASE_URL — " +
			"refusing to run production on a single-file SQLite database")
	}
	return nil
}

// IsolationGuardError refuses to serve without an isolated runtime; a dev build
// may opt out with SANDBOXD_DEV=1. devBuild is a parameter so both shapes are testable.
func IsolationGuardError(devBuild, devInsecure, isolated bool) error {
	if isolated {
		return nil
	}
	if !devBuild {
		return errors.New("no isolated runtime is available (Firecracker init failed or " +
			"/dev/kvm missing, and Docker is also unavailable) — refusing to run untrusted " +
			"code without isolation. This binary was built WITHOUT -tags dev, so it contains " +
			"no unisolated runtime and SANDBOXD_DEV=1 will NOT start it: provision /dev/kvm " +
			"plus the firecracker binary and a kernel, or install Docker. Rebuild with " +
			"-tags dev only for local development")
	}
	if !devInsecure {
		return errors.New("no isolated runtime is available (Firecracker init failed or " +
			"/dev/kvm missing, and Docker is also unavailable) — refusing to run untrusted " +
			"code without isolation. Set SANDBOXD_DEV=1 for local dev only (never in production)")
	}
	return nil
}

// NonRootVMMRequested reports whether the Firecracker VMM runs as an
// unprivileged per-sandbox uid. Default true; FC_NONROOT_VMM=0 is break-glass.
func NonRootVMMRequested() bool {
	return EnvBool("FC_NONROOT_VMM", true)
}

// NetNSRequested reports whether every Firecracker sandbox gets its own
// network namespace. Defaults to true; FC_NETNS=0 keeps the shared-bridge
// fallback for hosts where the netns path will not run.
func NetNSRequested() bool {
	return EnvBool("FC_NETNS", true)
}

// StorageCapVerifier is implemented by runtimes that can prove this host
// enforces the per-sandbox disk cap. known=false means no verdict, which
// callers must not treat as enforcement.
type StorageCapVerifier interface {
	VerifyStorageCap(ctx context.Context) (enforced, known bool, detail string)
}

// StorageCapGuardError refuses a production node whose storage driver does not
// enforce the disk cap. overlayfs on ext4 accepts --storage-opt size and
// silently ignores it, so a probe is needed; an unknown verdict is also refused.
func StorageCapGuardError(production, enforced, known bool, detail string) error {
	if !production || enforced {
		return nil
	}
	if !known {
		return errors.New("SANDBOXD_PRODUCTION=1: could not verify that this host enforces " +
			"per-sandbox disk caps (" + detail + ") — refusing to serve rather than assume it does. " +
			"Unset SANDBOXD_PRODUCTION for local/dev use")
	}
	return errors.New("SANDBOXD_PRODUCTION=1: this host does NOT enforce per-sandbox disk caps (" +
		detail + ") — one sandbox can fill the node's disk and storage is metered at a size " +
		"nothing holds it to. Use a storage driver that honours --storage-opt size (xfs with " +
		"pquota), or the Firecracker runtime, whose overlay is sized per sandbox. " +
		"Unset SANDBOXD_PRODUCTION for local/dev use")
}

// CheckStorageCap runs the verification when the runtime supports it, and
// reports unknown otherwise.
func CheckStorageCap(ctx context.Context, runtime any) (enforced, known bool, detail string) {
	v, ok := runtime.(StorageCapVerifier)
	if !ok {
		return false, false, "runtime does not support storage-cap verification"
	}
	return v.VerifyStorageCap(ctx)
}
