package main

import (
	"testing"

	"go.uber.org/zap"
)

// A half-configured fleet (URL set, key empty) must refuse rather than fall
// through to host execution, which runs with the portal's own secrets.
func TestSandboxExecModeFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name            string
		url, key, optIn string
		want            execMode
	}{
		{"fleet configured", "https://orch.internal", "k_live", "", execProxy},
		{"fleet configured, opt-in ignored", "https://orch.internal", "k_live", "1", execProxy},

		// The bug this test exists for.
		{"url set but key missing", "https://orch.internal", "", "", execRefuse},
		{"key set but url missing", "", "k_live", "", execRefuse},
		{"nothing configured", "", "", "", execRefuse},
		{"half-configured, opt-in NOT set", "https://orch.internal", "  ", "", execRefuse},

		// Local development still has to work, but only when asked for.
		{"explicit local opt-in", "", "", "1", execLocal},
		{"explicit local opt-in, true", "", "", "true", execLocal},
		{"opt-in must be truthy", "", "", "0", execRefuse},
		{"opt-in must be truthy, empty", "", "", "", execRefuse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sandboxExecMode(tc.url, tc.key, tc.optIn); got != tc.want {
				t.Errorf("sandboxExecMode(%q, %q, %q) = %v, want %v", tc.url, tc.key, tc.optIn, got, tc.want)
			}
		})
	}
}

func TestRefuseModeIsNotSuccess(t *testing.T) {
	if execRefuse == execProxy || execRefuse == execLocal {
		t.Fatal("execRefuse must be distinct from the modes that actually run code")
	}
	// The zero value must be the safe one.
	var unset execMode
	if unset != execRefuse {
		t.Errorf("zero value of execMode = %v, want execRefuse — unset must fail closed", unset)
	}
}

func TestAPortalInFrontOfAFleetStartsNoEnvd(t *testing.T) {
	cmd, token := startLocalEnvd(zap.NewNop(), "/nonexistent/envd", true)
	if cmd != nil || token != "" {
		t.Fatalf("proxy mode started envd: %v %q", cmd, token)
	}
	stopEnvd(zap.NewNop(), nil)
}
