package firecracker

import (
	"os"
	"strings"
	"testing"
)

// New needs /dev/kvm and a real manager, so the call cannot be driven from a
// unit test; this pins the source instead, the way
// internal/nodeboot/runtimefailclosed_test.go pins its branch.
//
// The pin exists because this exact line was missing for the life of the
// monitors: RunMonitors' own doc said "call this after Manager is initialized"
// and nothing did, so Manager.diskPressure — the flag the create and fork paths
// read to refuse work above 80% full — had no writer and was always false. A
// node then accepted creates until its overlay filesystem was genuinely full,
// with a guard in the source that looked like protection.
func TestTheNodeStartsTheManagerMonitors(t *testing.T) {
	body, err := os.ReadFile("wire.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)

	if !strings.Contains(src, "vm.RunMonitors(ctx, mgr, logger)") {
		t.Fatal("firecracker.New no longer starts the manager's monitors.\n\n" +
			"Without vm.RunMonitors nothing writes Manager.diskPressure, so the disk-pressure " +
			"refusal on the create and fork paths can never fire and the node accepts sandboxes " +
			"until its overlay filesystem is full. If the call moved, move this pin with it.")
	}
	// Under context.Background() the goroutines would outlive the node's shutdown.
	if strings.Contains(src, "vm.RunMonitors(context.Background()") {
		t.Fatal("the monitors are started on context.Background() rather than the node's " +
			"lifetime context, so nothing stops them on shutdown")
	}
}
