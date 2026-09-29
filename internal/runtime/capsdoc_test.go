package runtime_test

// Checks docs/runtimes.md against each adapter's DeclaredCaps(). External test
// package because the adapters import internal/runtime; DeclaredCaps rather
// than a constructed Runtime because firecracker and docker cannot be built in CI.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/docker"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/firecracker"
	"github.com/pilldrops-oss/pilldrops/internal/runtime/process"
)

const capsDocPath = "../../docs/runtimes.md"

// capsColumns is the table's column order after "runtime". A new capability
// column goes here, in docRow.cells and in capOf, or nothing checks it.
var capsColumns = []string{"isolation", "snapshot", "multiport", "checkpoint", "standby", "l4forward", "nested docker", "desktop"}

// optInEnv names the environment variable behind each capability documented as opt-in.
// Empty today: firecracker's snapshot and checkpoint were opt-in behind
// FC_MEM_SNAPSHOTS until that default was flipped on, and TestSnapshotsDefaultOn
// pins the flip. A row documented "opt-in" with no entry here still fails.
var optInEnv = map[string]map[string]string{}

// declared is every runtime this repository ships.
var declared = map[string]func() rt.Caps{
	"firecracker": firecracker.DeclaredCaps,
	"docker":      docker.DeclaredCaps,
	"process":     process.DeclaredCaps,
}

type docRow struct {
	isolation, snapshot, multiport, checkpoint, standby, l4forward, nestedDocker, desktop string
}

// cells maps each documented column to its cell, so every check walks the same
// list and a column added to the table cannot go unchecked.
func (r docRow) cells() map[string]string {
	return map[string]string{
		"isolation": r.isolation, "snapshot": r.snapshot,
		"multiport": r.multiport, "checkpoint": r.checkpoint,
		"standby": r.standby, "l4forward": r.l4forward,
		"nested docker": r.nestedDocker, "desktop": r.desktop,
	}
}

// parseCapsTable fails, rather than returning an empty map, when the table is
// missing, so the tests cannot pass vacuously.
func parseCapsTable(t *testing.T) map[string]docRow {
	t.Helper()

	b, err := os.ReadFile(filepath.Clean(capsDocPath))
	if err != nil {
		t.Fatalf("read %s: %v", capsDocPath, err)
	}
	body := string(b)

	const begin, end = "<!-- BEGIN CAPS TABLE -->", "<!-- END CAPS TABLE -->"
	i, j := strings.Index(body, begin), strings.Index(body, end)
	if i < 0 || j < 0 || j < i {
		t.Fatalf("%s: could not find the %s / %s markers — the table this test "+
			"checks has moved or been removed, so nothing is being verified", capsDocPath, begin, end)
	}

	rows := map[string]docRow{}
	for _, line := range strings.Split(body[i+len(begin):j], "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != len(capsColumns)+1 {
			continue
		}
		for k := range cells {
			cells[k] = strings.ToLower(strings.TrimSpace(cells[k]))
		}
		switch cells[0] {
		case "runtime", "": // header
			continue
		}
		if strings.HasPrefix(cells[1], "-") { // separator row
			continue
		}
		rows[cells[0]] = docRow{
			isolation: cells[1], snapshot: cells[2], multiport: cells[3],
			checkpoint: cells[4], standby: cells[5], l4forward: cells[6],
			nestedDocker: cells[7], desktop: cells[8],
		}
	}

	if len(rows) == 0 {
		t.Fatalf("%s: found the markers but parsed no rows — this test is no longer checking anything", capsDocPath)
	}
	return rows
}

func wantBool(t *testing.T, runtime, column, cell string) bool {
	t.Helper()
	switch cell {
	case "yes":
		return true
	case "no":
		return false
	default:
		t.Fatalf("%s/%s: unrecognised value %q (want yes, no, or opt-in)", runtime, column, cell)
		return false
	}
}

func TestRuntimeCapabilitiesMatchTheDoc(t *testing.T) {
	rows := parseCapsTable(t)

	for name, capsFn := range declared {
		row, ok := rows[name]
		if !ok {
			t.Errorf("runtime %q is shipped in the code but has no row in %s — every runtime "+
				"must declare its capabilities where an operator will read them", name, capsDocPath)
			continue
		}

		got := capsFn()
		check := func(column, cell string, actual bool) {
			if cell == "opt-in" {
				return
			}
			if want := wantBool(t, name, column, cell); want != actual {
				t.Errorf("%s: %s is %v in the code but %q in %s — the declaration and the "+
					"runtime disagree", name, column, actual, cell, capsDocPath)
			}
		}
		for _, column := range capsColumns {
			check(column, row.cells()[column], capOf(got, column))
		}
	}

	for name := range rows {
		if _, ok := declared[name]; !ok {
			t.Errorf("%s documents runtime %q, which this build does not ship", capsDocPath, name)
		}
	}
}

// TestOptInCapabilities pins opt-in as false by default and true only with the
// documented environment variable set.
func TestOptInCapabilities(t *testing.T) {
	rows := parseCapsTable(t)

	for name, row := range rows {
		for column, cell := range row.cells() {
			if cell != "opt-in" {
				continue
			}
			env, ok := optInEnv[name][column]
			if !ok {
				t.Errorf("%s: %s/%s is documented opt-in but no environment variable is "+
					"registered for it, so the claim cannot be verified", capsDocPath, name, column)
				continue
			}

			t.Setenv(env, "")
			if capFor(t, name, column) {
				t.Errorf("%s/%s is documented opt-in but is true with %s unset", name, column, env)
			}
			t.Setenv(env, "1")
			if !capFor(t, name, column) {
				t.Errorf("%s/%s is documented opt-in but stays false with %s=1", name, column, env)
			}
		}
	}
}

func capFor(t *testing.T, name, column string) bool {
	t.Helper()
	fn, ok := declared[name]
	if !ok {
		t.Fatalf("no declared caps for %q", name)
	}
	return capOf(fn(), column)
}

// capOf reads one documented column off a Caps.
func capOf(c rt.Caps, column string) bool {
	switch column {
	case "isolation":
		return c.Isolation
	case "snapshot":
		return c.Snapshot
	case "multiport":
		return c.MultiPort
	case "checkpoint":
		return c.Checkpoint
	case "standby":
		return c.Standby
	case "l4forward":
		return c.L4Forward
	case "nested docker":
		return c.NestedDocker
	case "desktop":
		return c.Desktop
	}
	return false
}

// TestSnapshotsDefaultOn pins the flip: firecracker's memory snapshots, and the
// checkpoints that ride the same capture, are ON with FC_MEM_SNAPSHOTS unset and
// OFF only when it is explicitly disabled. TestOptInCapabilities cannot check
// this — with no opt-in row left, its loop body never runs.
func TestSnapshotsDefaultOn(t *testing.T) {
	t.Setenv("FC_MEM_SNAPSHOTS", "")
	if c := firecracker.DeclaredCaps(); !c.Snapshot || !c.Checkpoint {
		t.Errorf("with FC_MEM_SNAPSHOTS unset: snapshot=%v checkpoint=%v, want both true",
			c.Snapshot, c.Checkpoint)
	}
	for _, off := range []string{"0", "false", "no", "off"} {
		t.Setenv("FC_MEM_SNAPSHOTS", off)
		if c := firecracker.DeclaredCaps(); c.Snapshot || c.Checkpoint {
			t.Errorf("FC_MEM_SNAPSHOTS=%q: snapshot=%v checkpoint=%v, want both false",
				off, c.Snapshot, c.Checkpoint)
		}
	}
	t.Setenv("FC_MEM_SNAPSHOTS", "1")
	if c := firecracker.DeclaredCaps(); !c.Snapshot || !c.Checkpoint {
		t.Error("FC_MEM_SNAPSHOTS=1 must still turn them on")
	}
}

// Pins the count so emptying `declared` cannot make the suite pass vacuously.
// process counts even though only dev builds ship it.
func TestEveryShippedRuntimeIsInTheMap(t *testing.T) {
	t.Parallel()
	const want = 3 // firecracker, docker, process (dev builds only)
	if len(declared) != want {
		t.Fatalf("declared has %d runtimes, want %d. If a runtime was added, register it here "+
			"and add a row to %s; if one was removed, update this count deliberately.",
			len(declared), want, capsDocPath)
	}
}
