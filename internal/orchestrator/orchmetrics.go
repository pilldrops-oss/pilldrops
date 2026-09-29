package orchestrator

// Control-plane metrics: hand-rolled Prometheus text exposition, same shape as
// the node's /metrics, so one scrape config reads both tiers.

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pilldrops-oss/pilldrops/internal/cluster"
)

// Admission outcomes. denied and unavailable are kept apart: one is a cap
// working, the other is an outage.
const (
	admitAllowed     = "allowed"
	admitDenied      = "denied"      // a real cap was hit
	admitUnavailable = "unavailable" // state could not be established (fail-closed)
)

// Scheduler outcomes.
const (
	schedPlaced     = "placed"
	schedNoTemplate = "no_template" // nothing online can boot it (a 400)
	schedNoRuntime  = "no_runtime"  // no node of the required runtime (a 400)
	schedNoCapacity = "no_capacity" // genuine pressure (a 503)
	schedTooBig     = "too_big"     // larger than any node in the fleet (a 400)
)

type orchMetrics struct {
	start time.Time

	// Atomics on the request path so a per-request mutex is not a serialization point.
	authCacheHits   atomic.Int64
	authCacheMisses atomic.Int64

	mu         sync.Mutex
	admissions map[string]int64
	schedules  map[string]int64
	// forwards is keyed "node outcome" where outcome is ok|error.
	forwards map[string]int64
	// One fleet-wide latency histogram; per-node buckets would multiply cardinality by fleet size.
	latBuckets []float64
	latCounts  []int64
	latSum     float64
	latCount   int64
}

func newOrchMetrics() *orchMetrics {
	return &orchMetrics{
		start:      time.Now(),
		admissions: map[string]int64{},
		schedules:  map[string]int64{},
		forwards:   map[string]int64{},
		latBuckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		latCounts:  make([]int64, 11),
	}
}

// The record* methods tolerate a nil receiver so a bare &Server{} in tests does not panic.

func (m *orchMetrics) recordAdmission(outcome string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.admissions[outcome]++
	m.mu.Unlock()
}

func (m *orchMetrics) recordSchedule(outcome string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.schedules[outcome]++
	m.mu.Unlock()
}

// recordForward counts one node hop and observes its latency.
func (m *orchMetrics) recordForward(nodeID string, seconds float64, err error) {
	if m == nil {
		return
	}
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	m.mu.Lock()
	m.forwards[nodeID+" "+outcome]++
	for i, ub := range m.latBuckets {
		if seconds <= ub {
			m.latCounts[i]++
		}
	}
	m.latSum += seconds
	m.latCount++
	m.mu.Unlock()
}

func (m *orchMetrics) incAuthCacheHit() {
	if m != nil {
		m.authCacheHits.Add(1)
	}
}

func (m *orchMetrics) incAuthCacheMiss() {
	if m != nil {
		m.authCacheMisses.Add(1)
	}
}

// writeCounterMap appends a labelled counter family, key-sorted so a scrape is byte-stable.
func writeCounterMap(b *strings.Builder, name, help, label string, m map[string]int64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	for _, k := range slices.Sorted(maps.Keys(m)) {
		fmt.Fprintf(b, "%s{%s=%q} %d\n", name, label, k, m[k])
	}
}

// writeForwards splits the "node outcome" composite key back into two labels.
func writeForwards(b *strings.Builder, m map[string]int64) {
	const name = "orchestrator_node_forwards_total"
	fmt.Fprintf(b, "# HELP %s Requests forwarded to a node, by node and outcome.\n# TYPE %s counter\n", name, name)
	for _, k := range slices.Sorted(maps.Keys(m)) {
		node, outcome, found := strings.Cut(k, " ")
		if !found {
			continue
		}
		fmt.Fprintf(b, "%s{node=%q,outcome=%q} %d\n", name, node, outcome, m[k])
	}
}

// handleMetrics serves the Prometheus scrape. Operator-only.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	m := s.metrics
	var b strings.Builder

	fmt.Fprintf(&b, "# HELP orchestrator_uptime_seconds Seconds since this replica started.\n# TYPE orchestrator_uptime_seconds gauge\norchestrator_uptime_seconds %d\n",
		int64(time.Since(m.start).Seconds()))

	// Fleet gauges are this replica's view: the registry is per-process, and
	// two replicas disagreeing is a real condition worth seeing.
	now := s.now()
	nodes := s.reg.Nodes()
	healthy, cordoned, schedulable := 0, 0, 0
	for _, n := range nodes {
		if n.Healthy(now, cluster.NodeTTL) {
			healthy++
		}
		if n.Cordoned || n.Draining {
			cordoned++
		}
		if n.Schedulable(now, cluster.NodeTTL) {
			schedulable++
		}
	}
	fmt.Fprintf(&b, "# HELP orchestrator_nodes Nodes in this replica's registry, by state.\n# TYPE orchestrator_nodes gauge\n")
	fmt.Fprintf(&b, "orchestrator_nodes{state=\"known\"} %d\n", len(nodes))
	fmt.Fprintf(&b, "orchestrator_nodes{state=\"healthy\"} %d\n", healthy)
	fmt.Fprintf(&b, "orchestrator_nodes{state=\"schedulable\"} %d\n", schedulable)
	fmt.Fprintf(&b, "orchestrator_nodes{state=\"cordoned\"} %d\n", cordoned)

	m.mu.Lock()
	admissions := maps.Clone(m.admissions)
	schedules := maps.Clone(m.schedules)
	forwards := maps.Clone(m.forwards)
	buckets := append([]float64(nil), m.latBuckets...)
	counts := append([]int64(nil), m.latCounts...)
	sum, total := m.latSum, m.latCount
	m.mu.Unlock()

	writeCounterMap(&b, "orchestrator_admissions_total",
		"Admission decisions, by outcome (allowed|denied|unavailable).", "outcome", admissions)
	writeCounterMap(&b, "orchestrator_schedules_total",
		"Scheduling attempts, by outcome (placed|no_template|no_runtime|no_capacity).", "outcome", schedules)
	writeForwards(&b, forwards)

	fmt.Fprintf(&b, "# HELP orchestrator_node_forward_duration_seconds Node forward latency in seconds.\n# TYPE orchestrator_node_forward_duration_seconds histogram\n")
	for i, ub := range buckets {
		fmt.Fprintf(&b, "orchestrator_node_forward_duration_seconds_bucket{le=%q} %d\n",
			strconv.FormatFloat(ub, 'g', -1, 64), counts[i])
	}
	fmt.Fprintf(&b, "orchestrator_node_forward_duration_seconds_bucket{le=\"+Inf\"} %d\n", total)
	fmt.Fprintf(&b, "orchestrator_node_forward_duration_seconds_sum %g\n", sum)
	fmt.Fprintf(&b, "orchestrator_node_forward_duration_seconds_count %d\n", total)

	fmt.Fprintf(&b, "# HELP orchestrator_auth_cache_total API-key validations served from cache vs the store.\n# TYPE orchestrator_auth_cache_total counter\n")
	fmt.Fprintf(&b, "orchestrator_auth_cache_total{result=\"hit\"} %d\n", m.authCacheHits.Load())
	fmt.Fprintf(&b, "orchestrator_auth_cache_total{result=\"miss\"} %d\n", m.authCacheMisses.Load())

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}
