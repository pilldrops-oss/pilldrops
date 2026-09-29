package nodeboot

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// SelectRuntime probes /dev/kvm, so the provisioned branch cannot be driven
// from a unit test; this pins the source instead.
func TestProvisionedFirecrackerInitFailureFailsClosed(t *testing.T) {
	body, err := os.ReadFile("runtime.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)

	start := strings.Index(src, "if kvmOK && binErr == nil && kernErr == nil {")
	if start < 0 {
		t.Fatal("could not find the provisioning gate in runtime.go — if the probe was renamed, move this pin with it")
	}
	end := strings.Index(src, "Firecracker not selected")
	if end < start {
		t.Fatal("could not find the not-provisioned path after the provisioning gate — if it was renamed, move this pin with it")
	}
	branch := src[start:end]

	if strings.Contains(branch, "docker.New") {
		t.Fatal("the Firecracker-provisioned branch of SelectRuntime reaches docker.New.\n\n" +
			"That is the silent-degrade this pin exists to keep dead: a provisioned KVM host " +
			"whose Firecracker init fails (missing nft, jailer, ...) must refuse to boot, not " +
			"serve tenants from a shared-kernel runtime the operator never chose. An operator " +
			"who wants Docker sets SANDBOXD_RUNTIME=docker explicitly.")
	}
	// The ferr path must return via unisolated(), which a release build answers with (nil, false).
	if !regexp.MustCompile(`(?s)ferr\b.*return unisolated\(`).MatchString(branch) {
		t.Fatal("the Firecracker-provisioned branch no longer routes its init-failure path " +
			"through unisolated() — the fail-closed refusal is gone or was rewired; " +
			"see the fail-closed comment in SelectRuntime before changing this")
	}
}
