//go:build ignore

// Command mint_test_keys seeds an isolated test DB with per-org API keys,
// so parallel e2e agents each get an isolated org.
//
// Usage: go run scripts/mint_test_keys.go <dsn> <org> ...
// Prints one "<org> <key>" line per org.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: mint_test_keys <dsn> <org> ...")
		os.Exit(2)
	}
	st, err := store.Open(context.Background(), os.Args[1])
	if err != nil {
		panic(err)
	}
	defer func() {
		if cerr := st.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "mint_test_keys: close: %v\n", cerr)
		}
	}()
	for _, spec := range os.Args[2:] {
		org, _, _ := strings.Cut(spec, ":")
		_, key, err := st.CreateKey(context.Background(), "test-"+org, org)
		if err != nil {
			panic(err)
		}
		fmt.Printf("%s %s\n", org, key)
	}
}
