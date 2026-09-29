package retention

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/config"
	"github.com/pilldrops-oss/pilldrops/internal/store"
)

// Start prunes every store.RetentionTarget every 6h, each with its own
// <NAME>_RETAIN_DAYS knob (0 disables). The first sweep runs at once so a node
// in a restart loop still prunes.
//
// This lives outside internal/nodeboot because both tiers run it: importing it
// from the node-boot package pulled the whole node tier (httpapi, daemon, vm,
// the runtimes) into the orchestrator binary, which the tier-boundary guard
// exists to prevent. It needs only the store and one env lookup.
func Start(ctx context.Context, st *store.Store) {
	type job struct {
		target store.RetentionTarget
		retain int
	}
	var jobs []job
	for _, t := range store.RetentionTargets {
		retain := config.Int(strings.ToUpper(t.Name)+"_RETAIN_DAYS", t.DefaultDays)
		if retain <= 0 {
			continue
		}
		jobs = append(jobs, job{t, retain})
	}
	if len(jobs) == 0 {
		return
	}
	sweep := func() {
		for _, j := range jobs {
			cutoff := time.Now().AddDate(0, 0, -j.retain).Unix()
			n, err := st.PruneOlderThan(ctx, j.target, cutoff)
			if err != nil {
				slog.Error("retention prune failed", "component", "retention",
					"target", j.target.Name, "err", err)
				continue
			}
			if n > 0 {
				slog.Info("pruned rows", "component", "retention",
					"target", j.target.Name, "count", n, "older_than_days", j.retain)
			}
		}
	}
	go func() {
		sweep()
		tk := time.NewTicker(6 * time.Hour)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
			}
			sweep()
		}
	}()
}
