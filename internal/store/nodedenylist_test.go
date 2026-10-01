package store

import (
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// deploy/postgres-node-role.sql revokes a set of tables from the node's
// database role. That file is only correct as long as node-tier code never
// calls a Store method that touches one of them — and nothing else checks it,
// so a future change that made a node read the vault would not break any test;
// it would break a fleet at deploy time with a permission error, or (worse) the
// role would be widened to make the error go away.
//
// This asserts the claim the SQL file rests on.
func TestNodeTierNeverCallsMethodsOnDeniedTables(t *testing.T) {
	denied := deniedTables

	// Node-tier packages: what cmd/sandboxd and cmd/sandboxd-fleet link.
	nodePkgs := []string{
		"../httpapi", "../sandbox/daemon", "../nodeboot", "../dashboard",
		"../cron",
	}
	methodDef := regexp.MustCompile(`(?m)^func \(s \*Store\) ([A-Z]\w*)\(`)

	nodeSrc := readGoSources(t, nodePkgs...)
	checked := 0

	for _, table := range denied {
		owners := filesMentioning(t, ".", table)
		var methods []string
		for _, f := range owners {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			for _, m := range methodDef.FindAllStringSubmatch(string(b), -1) {
				methods = append(methods, m[1])
			}
		}
		if len(methods) == 0 {
			// A table with no accessors at all: dead schema left by the removed
			// commercial layer. Revoking it is still right, there is just
			// nothing to check. Recorded rather than silently skipped, so the
			// count below can tell this apart from a broken scan.
			t.Logf("  %-16s no Store accessors (dead table) — revoke is defence only", table)
			continue
		}
		checked++
		for _, m := range methods {
			if strings.Contains(nodeSrc, "."+m+"(") {
				t.Errorf("node-tier code calls %s(), which touches %q — that table is REVOKED in "+
					"deploy/postgres-node-role.sql, so this will fail with a permission error on a "+
					"fleet. Either the call belongs on the control plane, or the SQL deny-list is wrong.",
					m, table)
			}
		}
	}
	// If NOTHING was checked, the method scan itself is broken and every
	// assertion above passed for the wrong reason.
	if checked == 0 {
		t.Fatal("no denied table had any Store accessor — the scan is broken, not the code")
	}
	t.Logf("verified %d denied table(s) against node-tier call sites", checked)

	// The write-only half (step 3a of the SQL file). These tables are READABLE
	// by a node — it resolves a template's ready build and fetches the artifact
	// — so the check above would be wrong for them: it would flag the reads the
	// grant deliberately keeps. What must never appear in node-tier code is a
	// call to a Store method that writes one of them.
	//
	// The stakes differ from the deny-list above, which is why this is not just
	// a second entry in it. A node can already PUT arbitrary bytes into the
	// fleet's shared store (internal/artifact.Handler, authenticated by the
	// node key). The only thing that stops another node booting those bytes is
	// that nothing marks them ready — so a node with INSERT or UPDATE here
	// turns object-write into fleet-wide code execution.
	for _, table := range writeDeniedTables {
		writers := storeMethodsWriting(t, ".", table)
		if len(writers) == 0 {
			// True today: migrations 41/42 are schema, and the publish path
			// that writes them (tasks.md §4.5) lands later on the control
			// plane. Logged rather than passed silently, because a vacuous
			// pass and a real one must not look the same — and
			// TestNodeRoleSQLMatchesDeniedTables below is what keeps this
			// entry honest in the meantime.
			t.Logf("  %-16s no Store method writes it yet — nothing to check until §4.5 lands", table)
			continue
		}
		t.Logf("  %-16s %d writing method(s): %s", table, len(writers), strings.Join(writers, ", "))
		for _, m := range writers {
			if strings.Contains(nodeSrc, "."+m+"(") {
				t.Errorf("node-tier code calls %s(), which WRITES %q — deploy/postgres-node-role.sql "+
					"grants a node only SELECT on that table, so this fails with a permission error on "+
					"a fleet. Do not widen the grant to fix it: publishing is operator-only and belongs "+
					"on the control plane, and a node that can mark a build ready can make every other "+
					"node boot bytes it chose.", m, table)
			}
		}
	}
}

// stripGoComments returns src with every comment removed, so a table name that
// appears in PROSE is not mistaken for table ACCESS.
//
// The scan asks "does this file touch this table", and the evidence for that is
// a SQL string, never a sentence. Without this, a doc comment explaining that
// this core does no pricing was enough to make every method in usage.go look
// like it read the revoked `pricing` table — the check failing on the word
// rather than the behaviour. The store.go special case above was a hand-patch
// for the same weakness, one file at a time.
//
// Parse-and-reprint rather than a regex: `//` inside a string literal is not a
// comment, and a stripper that thinks otherwise would silently delete real SQL
// and turn this test vacuous — which is the one failure mode a deny-list test
// must not have. Unparseable source falls back to the raw text, so a syntax
// error surfaces from the compiler rather than as a mysterious pass here.
func stripGoComments(t *testing.T, src string) string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, 0) // no ParseComments: comments dropped
	if err != nil {
		return src
	}
	var b strings.Builder
	if err := printer.Fprint(&b, fset, f); err != nil {
		return src
	}
	return b.String()
}

// deniedTables are the tables a node-tier DB role must not reach at all.
var deniedTables = []string{"vault_secrets", "admission_slots", "credit_ledger", "pricing", "billing_cursor"}

// writeDeniedTables are readable by a node but must not be written by one.
var writeDeniedTables = []string{"template_builds", "artifacts"}

// storeMethodsWriting returns the exported Store methods in dir whose body
// contains an INSERT, UPDATE or DELETE against table.
//
// Body-scoped rather than file-scoped, because these tables are read-and-write:
// a file-level match would flag the reader that lives next to the writer, and
// the reads are exactly what the grant keeps.
func storeMethodsWriting(t *testing.T, dir, table string) []string {
	t.Helper()
	writes := []*regexp.Regexp{
		regexp.MustCompile(`(?i)INSERT\s+INTO\s+` + regexp.QuoteMeta(table) + `\b`),
		regexp.MustCompile(`(?i)UPDATE\s+` + regexp.QuoteMeta(table) + `\b`),
		regexp.MustCompile(`(?i)DELETE\s+FROM\s+` + regexp.QuoteMeta(table) + `\b`),
	}
	methodStart := regexp.MustCompile(`(?m)^func \(s \*Store\) ([A-Z]\w*)\(`)

	var out []string
	for _, f := range filesMentioning(t, dir, table) {
		if filepath.Base(f) == "store.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src := string(b)
		locs := methodStart.FindAllStringSubmatchIndex(src, -1)
		for i, loc := range locs {
			// A top-level func ends where the next one begins; the last runs to
			// EOF. Coarse on purpose — a body that over-reaches can only make
			// this test stricter, never blind.
			end := len(src)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			body := src[loc[1]:end]
			for _, re := range writes {
				if re.MatchString(body) {
					out = append(out, src[loc[2]:loc[3]])
					break
				}
			}
		}
	}
	return out
}

func readGoSources(t *testing.T, dirs ...string) string {
	t.Helper()
	var b strings.Builder
	for _, d := range dirs {
		// Fatal, not skippable. Every path here is in-tree, so unreadable means
		// renamed or moved — and skipping meant a move silently dropped a
		// node-tier package from the scan while the test still passed.
		entries, err := os.ReadDir(d)
		if err != nil {
			t.Fatalf("node-tier package %s is unreadable — if it moved, update "+
				"nodePkgs; this check is only as good as that list: %v", d, err)
		}
		for _, e := range entries {
			n := e.Name()
			if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(d, n))
			if err != nil {
				t.Fatalf("read %s: %v", n, err)
			}
			b.Write(raw)
		}
	}
	if b.Len() == 0 {
		t.Fatal("read no node-tier sources — the check would pass vacuously")
	}
	return b.String()
}

func filesMentioning(t *testing.T, dir, needle string) []string {
	t.Helper()
	var out []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		if strings.Contains(stripGoComments(t, string(raw)), needle) {
			out = append(out, filepath.Join(dir, n))
		}
	}
	return out
}
