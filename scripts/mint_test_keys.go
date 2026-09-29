//go:build ignore

// Command mint_test_keys seeds an isolated test DB with per-org API keys and
// starting balances, so parallel e2e agents each get an isolated org.
//
// Usage: go run scripts/mint_test_keys.go <state.db> <org>:<balance> ...
// Prints one "<org> <key>" line per org.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: mint_test_keys <state.db> <org>:<balance> ...")
		os.Exit(2)
	}
	st, err := store.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer func() {
		if cerr := st.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "mint_test_keys: close: %v\n", cerr)
		}
	}()
	for _, spec := range os.Args[2:] {
		org, balStr, _ := strings.Cut(spec, ":")
		bal, err := strconv.ParseFloat(balStr, 64)
		if err != nil {
			// panic, not os.Exit, so the deferred Close still runs.
			panic(fmt.Errorf("invalid balance %q for org %q: %w", balStr, org, err))
		}
		if _, err := st.EnsureCredits(org, bal); err != nil {
			panic(err)
		}
		_, key, err := st.CreateKey("test-"+org, org)
		if err != nil {
			panic(err)
		}
		fmt.Printf("%s %s\n", org, key)
	}
}
