package httpapi

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"github.com/pilldrops-oss/pilldrops/internal/sandbox/daemon"
)

// writeDaemonMetrics exposes the control loop's timings.
func (s *Server) writeDaemonMetrics(b *strings.Builder) {
	if s.d == nil {
		return
	}
	m := s.d.Metrics()
	if m == nil {
		return
	}
	writeHistogram(b, "sandboxd_reconcile_duration_seconds",
		"How long one reconcile tick took. The tick owns metering, TTL reaping, idle "+
			"auto-pause and credit enforcement, so this stalling stalls all of them.",
		m.ReconcileDuration)
	counter(b, "sandboxd_reconcile_overruns_total",
		"Reconcile ticks abandoned at their budget. Non-zero means background work is being dropped.",
		float64(m.ReconcileOverruns.Load()))
	writeHistogram(b, "sandboxd_sandbox_pause_duration_seconds",
		"Runtime snapshot time during pause — the usual cause of a reconcile overrun.",
		m.PauseDuration)
	writeHistogram(b, "sandboxd_sandbox_resume_duration_seconds",
		"Runtime restore time during resume; this is the customer's wake-on-request latency.",
		m.ResumeDuration)
	writeHistogram(b, "sandboxd_scanner_duration_seconds",
		"One OOM/crash scanner pass. Exceeding the tick interval means events arrive late or not at all.",
		m.ScanDuration)
}

// writeStoreMetrics exposes the database connection pool.
func (s *Server) writeStoreMetrics(b *strings.Builder) {
	if s.st == nil {
		return
	}
	st := s.st.PoolStats()
	gauge(b, "sandboxd_store_conns_open", "Open database connections.", float64(st.OpenConnections))
	gauge(b, "sandboxd_store_conns_in_use", "Database connections currently in use.", float64(st.InUse))
	gauge(b, "sandboxd_store_conns_idle", "Idle database connections.", float64(st.Idle))
	counter(b, "sandboxd_store_conns_wait_total",
		"Times a caller had to WAIT for a database connection — the pool-saturation signal.",
		float64(st.WaitCount))
	counter(b, "sandboxd_store_conns_wait_seconds_total",
		"Total time spent waiting for a database connection.", st.WaitDuration.Seconds())
	counter(b, "sandboxd_store_conns_closed_max_idle_total",
		"Connections closed because the idle limit was reached.", float64(st.MaxIdleClosed))
	counter(b, "sandboxd_store_conns_closed_max_lifetime_total",
		"Connections closed because their maximum lifetime elapsed.", float64(st.MaxLifetimeClosed))
}

// writeRuntimeMetrics exposes goroutine and heap numbers for leak detection.
// ReadMemStats stops the world, so only the few fields a scrape needs are reported.
func writeRuntimeMetrics(b *strings.Builder) {
	gauge(b, "go_goroutines", "Goroutines currently running.", float64(runtime.NumGoroutine()))

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	gauge(b, "go_memstats_alloc_bytes", "Heap bytes currently allocated.", float64(ms.Alloc))
	gauge(b, "go_memstats_heap_objects", "Live heap objects.", float64(ms.HeapObjects))
	gauge(b, "go_memstats_sys_bytes", "Bytes obtained from the OS.", float64(ms.Sys))
	counter(b, "go_memstats_mallocs_total", "Cumulative heap object allocations.", float64(ms.Mallocs))
	counter(b, "go_gc_cycles_total", "Completed GC cycles.", float64(ms.NumGC))
	counter(b, "go_gc_pause_seconds_total", "Cumulative stop-the-world GC pause.",
		float64(ms.PauseTotalNs)/1e9)
}

// writeHistogram renders a daemon.Durations in Prometheus histogram format.
func writeHistogram(b *strings.Builder, name, help string, h *daemon.Durations) {
	if h == nil {
		return
	}
	bounds, counts, sum, total := h.Snapshot()
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
	for i, ub := range bounds {
		fmt.Fprintf(b, "%s_bucket{le=%q} %d\n", name, strconv.FormatFloat(ub, 'g', -1, 64), counts[i])
	}
	fmt.Fprintf(b, "%s_bucket{le=\"+Inf\"} %d\n%s_sum %g\n%s_count %d\n", name, total, name, sum, name, total)
}
