package httpx

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestTheProxyWarningFiresOnlyWhenItMeansSomething pins both halves.
//
// A commit message once claimed this warning "covers both tiers"; it existed in
// one tier only, on a path gated behind an unrelated feature, so a node behind a
// terminator got silence. A test that only proved the warning can appear would
// not have caught that, so this also pins the two cases where it must NOT.
func TestTheProxyWarningFiresOnlyWhenItMeansSomething(t *testing.T) {
	capture := func(fn func()) string {
		var buf bytes.Buffer
		old := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
		defer slog.SetDefault(old)
		fn()
		return buf.String()
	}

	t.Setenv("TRUSTED_PROXY_CIDRS", "")
	got := capture(func() { WarnIfUntrustedProxy("sandboxd", true) })
	if !strings.Contains(got, "TRUSTED_PROXY_CIDRS") || !strings.Contains(got, "sandboxd") {
		t.Errorf("limits on and no trusted proxy: want a warning naming the tier, got %q", got)
	}

	if got := capture(func() { WarnIfUntrustedProxy("sandboxd", false) }); got != "" {
		t.Errorf("no per-address limits: want silence, got %q", got)
	}
}
