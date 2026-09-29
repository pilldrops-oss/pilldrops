package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every statement in this package must go through a *Context form so queries
// carry a deadline and can be cancelled.
func TestNoContextFreeDatabaseCalls(t *testing.T) {
	banned := regexp.MustCompile(
		`\b(?:s\.db|tx|d\.db)\.(?:Query|QueryRow|Exec)\(|\.Begin\(\)`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if banned.MatchString(line) {
				t.Errorf("%s:%d uses a context-free database call:\n\t%s\n"+
					"Use ExecContext/QueryContext/QueryRowContext/BeginTx, or the "+
					"package helpers s.exec/s.query/s.queryRow, all of which take a ctx.",
					name, i+1, trimmed)
			}
		}
	}
}

func TestExportedStoreMethodsTakeContext(t *testing.T) {
	// Lifecycle and configuration methods that issue no query.
	exempt := map[string]bool{
		"Close": true, "SetSecretKey": true, "PoolStats": true,
		"SetVaultKey": true, "SetAllowPlaintextVault": true,
	}
	def := regexp.MustCompile(`^func \(s \*Store\) ([A-Z]\w*)\(([^)]*)\)`)

	entries, _ := os.ReadDir(".")
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(filepath.Clean(name))
		for i, line := range strings.Split(string(src), "\n") {
			m := def.FindStringSubmatch(line)
			if m == nil || exempt[m[1]] {
				continue
			}
			if !strings.HasPrefix(m[2], "ctx context.Context") {
				t.Errorf("%s:%d: Store.%s must take ctx as its first parameter (has %q)",
					name, i+1, m[1], m[2])
			}
		}
	}
}
