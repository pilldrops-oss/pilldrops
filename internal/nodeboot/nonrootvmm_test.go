package nodeboot

import (
	"os"
	"regexp"
	"testing"
)

// Asserted here rather than in the linux-only vm package so it runs on every checkout.
func TestNonRootVMMIsTheDefault(t *testing.T) {
	t.Setenv("FC_NONROOT_VMM", "")
	if !NonRootVMMRequested() {
		t.Fatal("FC_NONROOT_VMM unset must mean an unprivileged VMM.\n\n" +
			"With this false the jailer gets --uid 0 and the VMM runs as root with the " +
			"full capability set — finding C2. A node that configures nothing must get " +
			"the contained VMM, not the root one.")
	}
}

func TestNonRootVMMEscapeHatch(t *testing.T) {
	for _, off := range []string{"0", "false", "no", "off"} {
		t.Run(off, func(t *testing.T) {
			t.Setenv("FC_NONROOT_VMM", off)
			if NonRootVMMRequested() {
				t.Errorf("FC_NONROOT_VMM=%q did not turn the gate off", off)
			}
		})
	}
	for _, on := range []string{"1", "true", "yes", "on"} {
		t.Run(on, func(t *testing.T) {
			t.Setenv("FC_NONROOT_VMM", on)
			if !NonRootVMMRequested() {
				t.Errorf("FC_NONROOT_VMM=%q did not turn the gate on", on)
			}
		})
	}
}

// Checked by reading the source, since constructing the runtime needs
// /dev/kvm, a firecracker binary and a kernel.
func TestSelectRuntimeWiresTheNonRootDefaultThrough(t *testing.T) {
	body, err := os.ReadFile("runtime.go")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`NonRootVMM:\s*NonRootVMMRequested\(\)`).Match(body) {
		t.Fatal("runtime.go does not set NonRootVMM from NonRootVMMRequested().\n\n" +
			"If the field is set from anything else — a literal, a differently-named " +
			"helper, or not at all — the default asserted by TestNonRootVMMIsTheDefault " +
			"never reaches vm.Config and every VMM on the node boots as root anyway.")
	}
}
