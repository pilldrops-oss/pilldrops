package nodeboot_test

// Asserted against the link closure via go list, since a transitive dependency
// compiles in just as surely as a direct one. External test package so it
// cannot import what it is checking.

import (
	"os/exec"
	"strings"
	"testing"
)

// unisolatedPkgs implement runtimes whose Caps.Isolation is false.
var unisolatedPkgs = []string{
	"github.com/pilldrops-oss/pilldrops/internal/runtime/process",
}

// releaseBins are built without -tags dev.
var releaseBins = []string{"./cmd/sandboxd", "./cmd/sandboxd-fleet"}

func closure(t *testing.T, tags, pkg string) map[string]bool {
	t.Helper()
	args := []string{"list", "-deps"}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, pkg)
	cmd := exec.Command("go", args...)
	cmd.Dir = "../.." // repository root
	cmd.Env = append(cmd.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	got := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			got[line] = true
		}
	}
	if len(got) == 0 {
		t.Fatalf("go %s returned an empty closure — this test is checking nothing",
			strings.Join(args, " "))
	}
	return got
}

func TestAReleaseBinaryContainsNoUnisolatedRuntime(t *testing.T) {
	t.Parallel()
	for _, bin := range releaseBins {
		deps := closure(t, "", bin)
		for _, p := range unisolatedPkgs {
			if deps[p] {
				t.Errorf("%s links %s, which declares Caps.Isolation=false. A released "+
					"binary must contain no runtime that can run customer code without a "+
					"boundary — reach it through nodeboot.unisolatedRuntime, whose release "+
					"half returns nil and imports nothing.", bin, p)
			}
		}
	}
}

// Vacuity guard for the test above: if the process runtime vanished from both
// builds, the release assertion would pass for the wrong reason.
func TestTheDevBuildDoesContainTheProcessRuntime(t *testing.T) {
	t.Parallel()
	deps := closure(t, "dev", "./cmd/sandboxd")
	for _, p := range unisolatedPkgs {
		if !deps[p] {
			t.Fatalf("the -tags dev build of ./cmd/sandboxd does NOT link %s. Either the dev "+
				"path was removed (then delete it from unisolatedPkgs deliberately, and say so), "+
				"or the tag split broke — in which case the release assertion above is passing "+
				"for the wrong reason and is no longer checking anything.", p)
		}
	}
}
