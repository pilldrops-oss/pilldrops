package runtime_test

// Checks the isolation levels table in docs/runtimes.md against runtime.LevelFor
// and each adapter's DeclaredCaps().

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	rt "github.com/pilldrops-oss/pilldrops/internal/runtime"
)

const levelsBegin, levelsEnd = "<!-- BEGIN LEVELS TABLE -->", "<!-- END LEVELS TABLE -->"

// parseLevelsTable fails, rather than returning an empty map, when the table is
// missing, so the tests cannot pass vacuously.
func parseLevelsTable(t *testing.T) map[rt.IsolationLevel][]string {
	t.Helper()

	b, err := os.ReadFile(filepath.Clean(capsDocPath))
	if err != nil {
		t.Fatalf("read %s: %v", capsDocPath, err)
	}
	body := string(b)

	i, j := strings.Index(body, levelsBegin), strings.Index(body, levelsEnd)
	if i < 0 || j < 0 || j < i {
		t.Fatalf("%s: could not find the %s / %s markers — the table this test checks has "+
			"moved or been removed, so nothing is being verified", capsDocPath, levelsBegin, levelsEnd)
	}

	out := map[rt.IsolationLevel][]string{}
	for _, line := range strings.Split(body[i+len(levelsBegin):j], "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 3 {
			continue
		}
		level := strings.Trim(strings.ToLower(strings.TrimSpace(cells[0])), "`")
		if level == "level" || level == "" || strings.HasPrefix(level, "-") {
			continue // header or separator
		}
		var names []string
		for _, n := range strings.Split(cells[2], ",") {
			n = strings.Trim(strings.TrimSpace(n), "`")
			if n != "" {
				names = append(names, n)
			}
		}
		out[rt.IsolationLevel(level)] = names
	}

	if len(out) == 0 {
		t.Fatalf("%s: found the levels markers but parsed no rows — this test is no longer "+
			"checking anything", capsDocPath)
	}
	return out
}

// Uses each runtime's real DeclaredCaps, so a flipped Isolation bit fails here
// even if the doc row is unchanged.
func TestIsolationLevelsMatchTheDoc(t *testing.T) {
	rows := parseLevelsTable(t)

	for level, names := range rows {
		if len(names) == 0 {
			t.Errorf("%s: level %q lists no runtime — a level nothing satisfies is a promise "+
				"with no implementation behind it", capsDocPath, level)
		}
		for _, name := range names {
			capsFn, ok := declared[name]
			if !ok {
				t.Errorf("%s: level %q is documented as satisfied by %q, which this build does "+
					"not ship", capsDocPath, level, name)
				continue
			}
			if got := rt.LevelFor(name, capsFn().Isolation); got != level {
				t.Errorf("%s: %q is documented as %q but runtime.LevelFor reports %q — the doc "+
					"and the code disagree about the boundary this runtime provides",
					capsDocPath, name, level, got)
			}
		}
	}
}

// Checks both directions: no undocumented level, no documented level the code lacks.
func TestEveryLevelInTheCodeIsDocumented(t *testing.T) {
	rows := parseLevelsTable(t)

	known := map[rt.IsolationLevel]bool{}
	for _, l := range rt.IsolationLevels() {
		known[l] = true
		if _, ok := rows[l]; !ok {
			t.Errorf("%s has no row for isolation level %q, which the code returns", capsDocPath, l)
		}
	}
	if len(known) == 0 {
		t.Fatal("runtime.IsolationLevels() is empty — this guard is passing on an empty set")
	}
	for l := range rows {
		if !known[l] {
			t.Errorf("%s documents isolation level %q, which runtime.IsolationLevels() does not "+
				"list — no API can return it", capsDocPath, l)
		}
	}
}

// Every shipped runtime must appear in exactly one level row.
func TestEveryShippedRuntimeHasALevel(t *testing.T) {
	rows := parseLevelsTable(t)

	seen := map[string]int{}
	for _, names := range rows {
		for _, n := range names {
			seen[n]++
		}
	}
	for name := range declared {
		switch seen[name] {
		case 1:
		case 0:
			t.Errorf("%s: runtime %q has no isolation level. Every shipped runtime needs one — "+
				"it is what a caller asks for instead of naming an engine", capsDocPath, name)
		default:
			t.Errorf("%s: runtime %q appears under %d isolation levels; it provides one boundary, "+
				"not several", capsDocPath, name, seen[name])
		}
	}
}

// A runtime that reports no boundary is trusted whatever it is called.
func TestNoRuntimeNamesItsWayOutOfTrusted(t *testing.T) {
	for _, name := range []string{"firecracker", "docker", "process", "unknown"} {
		if got := rt.LevelFor(name, false); got != rt.IsolationTrusted {
			t.Errorf("LevelFor(%q, isolating=false) = %q, want %q — a runtime that claims no "+
				"boundary does not get a level from its name", name, got, rt.IsolationTrusted)
		}
	}
}
