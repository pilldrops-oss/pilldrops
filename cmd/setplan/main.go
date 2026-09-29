// Command setplan assigns an org's limit preset directly in the store. An org
// with no preset is unlimited, so this restricts a tenant; store.ClearPlan
// puts it back. Usage:
//
//	DB=<postgres-dsn> go run ./cmd/setplan -org default -plan enterprise
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/pilldrops-oss/pilldrops/internal/entitlements"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

func main() {
	ctx := context.Background()
	org := flag.String("org", "default", "org id")
	plan := flag.String("plan", "enterprise", "limit preset id: free|personal|pro|max|enterprise")
	dsn := flag.String("db", os.Getenv("DB"), "database DSN (or $DB)")
	flag.Parse()

	p, ok := entitlements.PlanByID(*plan)
	if !ok {
		fmt.Fprintln(os.Stderr, "unknown plan:", *plan)
		os.Exit(1)
	}
	st, err := store.Open(ctx, *dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open store:", err)
		os.Exit(1)
	}
	if err := st.SetPlan(ctx, *org, *plan); err != nil {
		fmt.Fprintln(os.Stderr, "SetPlan:", err)
		os.Exit(1)
	}
	fmt.Printf("✅ org %q → plan %q (MaxConcurrent=%d, MaxVCPU=%d, MaxMemMB=%d, SessionTTLHrs=%d)\n",
		*org, *plan, p.MaxConcurrent, p.MaxVCPU, p.MaxMemMB, p.SessionTTLHrs)
}
