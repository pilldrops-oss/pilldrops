package nodeboot

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

// PreflightCheck is one probe and its verdict. Passes are kept so the log can
// distinguish a node that passed from one that never ran preflight.
type PreflightCheck struct {
	Name string // stable probe identifier, e.g. "egress-rules"
	Err  error  // nil when the probe passed
}

// PreflightResult is the outcome of one boot-time isolation probe run.
type PreflightResult struct {
	OK       bool
	Checks   []PreflightCheck // every probe considered, in order
	Failures []string         // one per failed probe
}

// IsolationChecker is implemented by runtimes that can verify their isolation
// is loaded. An isolating runtime that lacks it fails preflight rather than
// passing by omission.
type IsolationChecker interface {
	CheckIsolation(ctx context.Context) error
}

// RunPreflight probes that the isolation this node claims is actually active.
// Dependencies are passed in so it is testable with fakes; the enforce or
// warn-only decision belongs to ReportPreflight.
func RunPreflight(ctx context.Context, runtime rt.Runtime, isolation bool, egressVerify func(mode string) error) PreflightResult {
	var checks []PreflightCheck
	record := func(name string, err error) { checks = append(checks, PreflightCheck{Name: name, Err: err}) }

	var isoErr error
	if !isolation {
		isoErr = fmt.Errorf("runtime %q provides no isolation", runtime.Name())
	}
	record("runtime-isolating", isoErr)

	if ic, ok := runtime.(IsolationChecker); ok {
		err := ic.CheckIsolation(ctx)
		if err != nil {
			err = fmt.Errorf("isolation check: %w", err)
		}
		record("runtime-self-check", err)
	} else if isolation {
		// A coverage gap must not read as a proven-safe node.
		record("runtime-self-check", fmt.Errorf("runtime %q does not support isolation verification", runtime.Name()))
	}

	// The process runtime has no network of its own for egress rules to apply
	// to. Recorded as an explicit skip so the log distinguishes it from not run.
	if runtime.Name() == "process" {
		record("egress-rules-skipped-process-runtime", nil)
	} else {
		mode := "--firecracker"
		if runtime.Name() == "docker" {
			mode = "--docker"
		}
		// A netns-mode node bypasses the bridge host rules by design and may
		// legitimately lack them, so it is checked against its own table.
		if nn, ok := runtime.(interface{ NetNSMode() bool }); ok && nn.NetNSMode() {
			mode = "--netns"
		}
		err := egressVerify(mode)
		if err != nil {
			err = fmt.Errorf("egress verify: %w", err)
		}
		record("egress-rules("+strings.TrimPrefix(mode, "--")+")", err)
	}

	var fails []string
	for _, c := range checks {
		if c.Err != nil {
			fails = append(fails, c.Err.Error())
		}
	}
	return PreflightResult{OK: len(fails) == 0, Checks: checks, Failures: fails}
}

// ReportPreflight logs every probe's verdict and reports whether the node must
// fail closed (refuse creates, suppress the heartbeat). The process stays up so
// the orchestrator schedules elsewhere. SANDBOXD_PREFLIGHT_ENFORCE=0 disables enforcement.
func ReportPreflight(pf PreflightResult, runtimeName string) (failClosed bool) {
	for _, c := range pf.Checks {
		if c.Err != nil {
			slog.Error("preflight check FAILED", "component", "preflight",
				"check", c.Name, "runtime", runtimeName, "err", c.Err)
			continue
		}
		slog.Info("preflight check passed", "component", "preflight",
			"check", c.Name, "runtime", runtimeName)
	}
	if pf.OK {
		slog.Info("preflight passed: this node's isolation invariants are verified live",
			"component", "preflight", "runtime", runtimeName, "checks", len(pf.Checks))
		return false
	}
	if !EnvBool("SANDBOXD_PREFLIGHT_ENFORCE", true) {
		slog.Warn("preflight FAILED but SANDBOXD_PREFLIGHT_ENFORCE=0: this node will keep serving "+
			"tenants without the isolation it advertises — unset it to fail closed",
			"component", "preflight", "runtime", runtimeName, "failures", len(pf.Failures))
		return false
	}
	slog.Error("preflight FAILED: this node will NOT accept sandbox creates or advertise capacity",
		"component", "preflight", "runtime", runtimeName, "failures", len(pf.Failures))
	return true
}

// verifyEgressScript is a copy of scripts/verify-egress.sh, since embed cannot
// reach outside the package directory.
//
//go:embed scripts/verify-egress.sh
var verifyEgressScript []byte

// VerifyEgress runs the embedded verify-egress.sh against this host's live
// firewall rules. Invoked via bash <tempfile> so it works with /tmp mounted noexec.
func VerifyEgress(mode string) error {
	switch mode {
	case "--firecracker", "--docker", "--netns":
	default:
		return fmt.Errorf("verify-egress: unknown mode %q", mode)
	}
	f, err := os.CreateTemp("", "verify-egress-*.sh")
	if err != nil {
		return fmt.Errorf("verify-egress: temp file: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }() //nolint:errcheck // best-effort temp-file cleanup
	if _, err := f.Write(verifyEgressScript); err != nil {
		_ = f.Close() //nolint:errcheck // best-effort: already returning the write error
		return fmt.Errorf("verify-egress: write temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("verify-egress: close temp file: %w", err)
	}
	var stderr strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), preflightTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", f.Name(), mode)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("verify-egress %s: %s", mode, msg)
	}
	return nil
}

// preflightTimeout bounds the isolation preflight script.
const preflightTimeout = 60 * time.Second
