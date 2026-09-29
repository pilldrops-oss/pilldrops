package httpapi

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

	"github.com/pilldrops-oss/pilldrops/internal/observability"
)

// metrics holds sandboxd's monotonic counters, served at GET /metrics in
// Prometheus text format. Live gauges are read from the daemon at scrape time.
type metrics struct {
	start time.Time

	// Atomics, not mutex-guarded: these are bumped on every request.
	creates      atomic.Int64
	destroys     atomic.Int64
	execs        atomic.Int64
	execErrors   atomic.Int64
	authFailures atomic.Int64

	// mu guards only the maps and the histogram, which cannot be atomics.
	mu             sync.Mutex
	requests       map[string]int64 // "METHOD STATUS" -> count
	funcInvokes    map[string]int64 // function name -> invoke count
	funcColdStarts map[string]int64 // function name -> cold-start count

	// Agent counters are keyed by agent so one misbehaving agent is not averaged away.
	agentTurns     map[string]int64 // "agent result" -> count
	agentInterrupt map[string]int64 // agent -> interrupts
	agentUnreach   map[string]int64 // agent -> supervisor unreachable
	agentSpendStop map[string]int64 // agent -> turns refused by a spend cap
	// agentApprovals is keyed "agent outcome"; agentApprovalTurns counts turns
	// needing at least one approval, agentApprovalFriction those over the threshold.
	agentApprovals        map[string]int64
	agentApprovalTurns    map[string]int64
	agentApprovalFriction map[string]int64
	mirrorPruned          atomic.Int64
	mirrorPruneErrors     atomic.Int64
	spendPaused           atomic.Int64
	spendDeferred         atomic.Int64
	spendPauseErr         atomic.Int64

	// Request-latency histogram (seconds). latCounts[i] is the cumulative count
	// of observations <= latBuckets[i]; latCount is the total (the +Inf bucket).
	latBuckets []float64
	latCounts  []int64
	latSum     float64
	latCount   int64
}

func newMetrics() *metrics {
	return &metrics{
		start:          time.Now(),
		requests:       make(map[string]int64),
		funcInvokes:    make(map[string]int64),
		funcColdStarts: make(map[string]int64),
		agentTurns:     make(map[string]int64),
		agentInterrupt: make(map[string]int64),
		agentUnreach:   make(map[string]int64),
		agentSpendStop: make(map[string]int64),

		agentApprovals:        make(map[string]int64),
		agentApprovalTurns:    make(map[string]int64),
		agentApprovalFriction: make(map[string]int64),
		latBuckets:            []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		latCounts:             make([]int64, 11),
	}
}

// observeLatency records a request duration (seconds) into the histogram.
func (m *metrics) observeLatency(seconds float64) {
	m.mu.Lock()
	for i, ub := range m.latBuckets {
		if seconds <= ub {
			m.latCounts[i]++
		}
	}
	m.latSum += seconds
	m.latCount++
	m.mu.Unlock()
}

// writeLatencyHistogram appends the request-latency histogram in Prometheus format.
func (m *metrics) writeLatencyHistogram(b *strings.Builder) {
	m.mu.Lock()
	buckets := append([]float64(nil), m.latBuckets...)
	counts := append([]int64(nil), m.latCounts...)
	sum, total := m.latSum, m.latCount
	m.mu.Unlock()

	fmt.Fprintf(b, "# HELP sandboxd_http_request_duration_seconds API request latency in seconds.\n# TYPE sandboxd_http_request_duration_seconds histogram\n")
	for i, ub := range buckets {
		fmt.Fprintf(b, "sandboxd_http_request_duration_seconds_bucket{le=%q} %d\n",
			strconv.FormatFloat(ub, 'g', -1, 64), counts[i])
	}
	fmt.Fprintf(b, "sandboxd_http_request_duration_seconds_bucket{le=\"+Inf\"} %d\n", total)
	fmt.Fprintf(b, "sandboxd_http_request_duration_seconds_sum %g\n", sum)
	fmt.Fprintf(b, "sandboxd_http_request_duration_seconds_count %d\n", total)
}

func (m *metrics) incCreate()      { m.creates.Add(1) }
func (m *metrics) incDestroy()     { m.destroys.Add(1) }
func (m *metrics) incExec()        { m.execs.Add(1) }
func (m *metrics) incExecError()   { m.execErrors.Add(1) }
func (m *metrics) incAuthFailure() { m.authFailures.Add(1) }

func (m *metrics) incAgentTurn(agent, result string) {
	m.mu.Lock()
	m.agentTurns[agent+" "+result]++
	m.mu.Unlock()
}

func (m *metrics) incAgentInterrupt(agent string) {
	m.mu.Lock()
	m.agentInterrupt[agent]++
	m.mu.Unlock()
}

// incAgentUnreachable counts a supervisor that did not answer, distinct from a failed turn.
func (m *metrics) incAgentUnreachable(agent string) {
	m.mu.Lock()
	m.agentUnreach[agent]++
	m.mu.Unlock()
}

func (m *metrics) incAgentApproval(agent, outcome string) {
	m.mu.Lock()
	m.agentApprovals[agent+" "+outcome]++
	m.mu.Unlock()
}

// noteAgentApprovalsPerTurn records one turn's approval count.
func (m *metrics) noteAgentApprovalsPerTurn(agent string, n int) {
	if n <= 0 {
		return
	}
	m.mu.Lock()
	m.agentApprovalTurns[agent]++
	if n > approvalFrictionThreshold {
		m.agentApprovalFriction[agent]++
	}
	m.mu.Unlock()
}

func (m *metrics) addAgentMirrorPruned(n int64) { m.mirrorPruned.Add(n) }

func (m *metrics) incAgentMirrorPruneError() { m.mirrorPruneErrors.Add(1) }

// snapshotSpendPauses reads the auto-pause counter, for tests.
func (m *metrics) snapshotSpendPauses() int64 { return m.spendPaused.Load() }

func (m *metrics) incAgentSpendPause() { m.spendPaused.Add(1) }

// incAgentSpendPauseDeferred counts a cap reached while a turn was still in flight.
func (m *metrics) incAgentSpendPauseDeferred() { m.spendDeferred.Add(1) }

func (m *metrics) incAgentSpendPauseError() { m.spendPauseErr.Add(1) }

// approvalFrictionThreshold is the approvals-per-turn count above which the
// policy is too narrow.
const approvalFrictionThreshold = 5

func (m *metrics) incAgentSpendStop(agent string) {
	m.mu.Lock()
	m.agentSpendStop[agent]++
	m.mu.Unlock()
}

func (m *metrics) recordRequest(method string, status int) {
	key := method + " " + strconv.Itoa(status)
	m.mu.Lock()
	m.requests[key]++
	m.mu.Unlock()
}

func (m *metrics) incFunctionInvoke(name string) {
	m.mu.Lock()
	m.funcInvokes[boundedLabel(m.funcInvokes, name)]++
	m.mu.Unlock()
}

func (m *metrics) incFunctionColdStart(name string) {
	m.mu.Lock()
	m.funcColdStarts[boundedLabel(m.funcColdStarts, name)]++
	m.mu.Unlock()
}

// maxFunctionLabels caps distinct function-name series. Names are customer
// chosen, so without a cap the map and the scrape cardinality are unbounded.
const maxFunctionLabels = 500

// otherLabel collects everything past the cap so the total stays right.
const otherLabel = "__other__"

// boundedLabel returns name while there is room or it is already tracked,
// otherwise otherLabel. Callers must hold the metrics mutex.
func boundedLabel(m map[string]int64, name string) string {
	if _, known := m[name]; known {
		return name
	}
	if len(m) >= maxFunctionLabels {
		return otherLabel
	}
	return name
}

// writeFunctionCounters appends the per-function counters in Prometheus format.
func (m *metrics) writeFunctionCounters(b *strings.Builder) {
	m.mu.Lock()
	inv := maps.Clone(m.funcInvokes)
	cold := maps.Clone(m.funcColdStarts)
	m.mu.Unlock()

	fmt.Fprintf(b, "# HELP sandboxd_function_invokes_total Function invocations by name.\n# TYPE sandboxd_function_invokes_total counter\n")
	for _, k := range sortedKeys(inv) {
		fmt.Fprintf(b, "sandboxd_function_invokes_total{name=%q} %d\n", k, inv[k])
	}
	m.writeAgentMetrics(b)
	fmt.Fprintf(b, "# HELP sandboxd_function_cold_starts_total Function cold starts by name.\n# TYPE sandboxd_function_cold_starts_total counter\n")
	for _, k := range sortedKeys(cold) {
		fmt.Fprintf(b, "sandboxd_function_cold_starts_total{name=%q} %d\n", k, cold[k])
	}
}

// statusRecorder captures the response status; 200 unless the handler writes another.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush delegates to the underlying writer; without it the wrapper hides
// http.Flusher from streaming handlers.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// instrument wraps a handler with request id, body cap, span, metrics and
// access log. The /preview proxy is left unwrapped so it can hijack and stream.
func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := observability.ResolveRequestID(r.Header.Get(observability.HeaderRequestID))
		if id != "" {
			w.Header().Set(observability.HeaderRequestID, id)
			r = r.WithContext(observability.WithRequestID(r.Context(), id))
		}
		// /files streams to the sandbox under its own PD_FILES_MAX_BYTES.
		if r.Body != nil && s.maxBodyBytes > 0 && !bodyCapExempt(r.URL.Path) {
			r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
		}
		// Started after the request id is in the context so the span carries it.
		ctx, span := observability.StartServer(r.Context(), r.Method, routePattern(r))
		defer span.End()
		r = r.WithContext(ctx)

		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sr, r)
		observability.SetHTTPStatus(span, sr.status)
		dur := time.Since(start)
		s.metrics.recordRequest(r.Method, sr.status)
		s.metrics.observeLatency(dur.Seconds())
		s.accessLog(r, sr.status, dur, id)
	})
}

// handleMetrics serves Prometheus text exposition. Mounted behind auth because
// fleet activity is operational intelligence.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	boxes := s.d.List("")
	byState := map[string]int{}
	for _, b := range boxes {
		byState[string(b.State)]++
	}

	m := s.metrics
	creates, destroys := m.creates.Load(), m.destroys.Load()
	execs, execErrors, authFailures := m.execs.Load(), m.execErrors.Load(), m.authFailures.Load()
	m.mu.Lock()
	reqs := maps.Clone(m.requests)
	m.mu.Unlock()
	uptime := time.Since(m.start).Seconds()

	var b strings.Builder
	counter(&b, "sandboxd_sandbox_creates_total", "Sandboxes created since start.", float64(creates))
	counter(&b, "sandboxd_sandbox_destroys_total", "Sandboxes destroyed since start.", float64(destroys))
	counter(&b, "sandboxd_exec_total", "Exec calls served since start.", float64(execs))
	counter(&b, "sandboxd_exec_errors_total", "Exec calls that failed since start.", float64(execErrors))
	counter(&b, "sandboxd_auth_failures_total", "Rejected requests due to auth since start.", float64(authFailures))
	gauge(&b, "sandboxd_uptime_seconds", "Seconds since the server started.", uptime)
	gauge(&b, "sandboxd_active_sandboxes", "Sandboxes currently known to the daemon.", float64(len(boxes)))

	// Non-zero means the storage driver does not honour --storage-opt size, so
	// a sandbox can exceed the disk it was sold.
	counter(&b, "sandboxd_storage_cap_unenforced_total",
		"Sandboxes booted without an enforced writable-disk cap on this host.",
		float64(s.d.StorageCapUnenforced()))

	fmt.Fprintf(&b, "# HELP sandboxd_sandboxes Current sandboxes by state.\n# TYPE sandboxd_sandboxes gauge\n")
	for _, st := range sortedKeys(byState) {
		fmt.Fprintf(&b, "sandboxd_sandboxes{state=%q} %d\n", st, byState[st])
	}

	fmt.Fprintf(&b, "# HELP sandboxd_http_requests_total API requests by method and status.\n# TYPE sandboxd_http_requests_total counter\n")
	for _, k := range sortedKeys(reqs) {
		method, status, _ := strings.Cut(k, " ")
		fmt.Fprintf(&b, "sandboxd_http_requests_total{method=%q,status=%q} %d\n", method, status, reqs[k])
	}

	m.writeLatencyHistogram(&b)
	m.writeFunctionCounters(&b)
	s.writeDaemonMetrics(&b)
	s.writeStoreMetrics(&b)
	writeRuntimeMetrics(&b)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

// handleSandboxMetrics returns live per-sandbox resource usage as JSON.
func (s *Server) handleSandboxMetrics(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.owns(r, id) {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	u, ok := s.d.Usage(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "sandbox not found")
		return
	}
	out := map[string]any{
		"id":             id,
		"state":          u.State,
		"vcpu":           u.VCPU,
		"mem_mib":        u.MemMiB,
		"vcpu_seconds":   u.VCPUSeconds,
		"ram_gb_seconds": u.RAMGBSeconds,
		"uptime_seconds": u.UptimeSeconds,
	}
	// Live gauges are present only where the runtime can sample them (docker).
	if ls, ok := s.d.LiveStats(r.Context(), id); ok {
		out["cpu_pct"] = ls.CPUPct
		out["mem_used_mib"] = ls.MemUsedMiB
		if ls.DiskUsedMB >= 0 {
			out["disk_used_mb"] = ls.DiskUsedMB
		}
	}
	// The limit makes overage observable where the driver cannot enforce it.
	if info, ok := s.d.Get(id); ok && info.StorageMB > 0 {
		out["disk_limit_mb"] = info.StorageMB
	}
	writeJSON(w, http.StatusOK, out)
}

func counter(b *strings.Builder, name, help string, v float64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s counter\n%s %g\n", name, help, name, name, v)
}

func gauge(b *strings.Builder, name, help string, v float64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, help, name, name, v)
}

func sortedKeys[V any](m map[string]V) []string { return slices.Sorted(maps.Keys(m)) }

// writeAgentMetrics emits the agent-surface counters.
func (m *metrics) writeAgentMetrics(b *strings.Builder) {
	m.mu.Lock()
	turns := maps.Clone(m.agentTurns)
	interrupts := maps.Clone(m.agentInterrupt)
	unreach := maps.Clone(m.agentUnreach)
	stops := maps.Clone(m.agentSpendStop)
	approvals := maps.Clone(m.agentApprovals)
	apprTurns := maps.Clone(m.agentApprovalTurns)
	friction := maps.Clone(m.agentApprovalFriction)
	m.mu.Unlock()
	pruned, pruneErrs := m.mirrorPruned.Load(), m.mirrorPruneErrors.Load()
	spPaused, spDeferred, spErr := m.spendPaused.Load(), m.spendDeferred.Load(), m.spendPauseErr.Load()

	fmt.Fprintf(b, "# HELP sandboxd_agent_turns_total Agent turns by agent and result.\n"+
		"# TYPE sandboxd_agent_turns_total counter\n")
	for _, k := range sortedKeys(turns) {
		agent, result := splitAgentKey(k)
		fmt.Fprintf(b, "sandboxd_agent_turns_total{agent=%q,result=%q} %d\n", agent, result, turns[k])
	}

	fmt.Fprintf(b, "# HELP sandboxd_agent_spend_autopause_total Workstations paused because their spend cap was reached.\n"+
		"# TYPE sandboxd_agent_spend_autopause_total counter\n"+
		"sandboxd_agent_spend_autopause_total %d\n", spPaused)
	fmt.Fprintf(b, "# HELP sandboxd_agent_spend_autopause_deferred_total Caps reached while a turn was still in flight.\n"+
		"# TYPE sandboxd_agent_spend_autopause_deferred_total counter\n"+
		"sandboxd_agent_spend_autopause_deferred_total %d\n", spDeferred)
	fmt.Fprintf(b, "# HELP sandboxd_agent_spend_autopause_errors_total Pause attempts that failed.\n"+
		"# TYPE sandboxd_agent_spend_autopause_errors_total counter\n"+
		"sandboxd_agent_spend_autopause_errors_total %d\n", spErr)

	fmt.Fprintf(b, "# HELP sandboxd_agent_mirror_events_pruned_total Mirrored events deleted by retention.\n"+
		"# TYPE sandboxd_agent_mirror_events_pruned_total counter\n"+
		"sandboxd_agent_mirror_events_pruned_total %d\n", pruned)
	fmt.Fprintf(b, "# HELP sandboxd_agent_mirror_prune_errors_total Retention passes that failed.\n"+
		"# TYPE sandboxd_agent_mirror_prune_errors_total counter\n"+
		"sandboxd_agent_mirror_prune_errors_total %d\n", pruneErrs)

	fmt.Fprintf(b, "# HELP sandboxd_agent_approvals_total Approval decisions by agent and outcome.\n"+
		"# TYPE sandboxd_agent_approvals_total counter\n")
	for _, k := range sortedKeys(approvals) {
		agent, outcome, _ := strings.Cut(k, " ")
		fmt.Fprintf(b, "sandboxd_agent_approvals_total{agent=%q,outcome=%q} %d\n", agent, outcome, approvals[k])
	}

	fmt.Fprintf(b, "# HELP sandboxd_agent_approval_turns_total Turns that needed at least one human approval, by agent.\n"+
		"# TYPE sandboxd_agent_approval_turns_total counter\n")
	for _, k := range sortedKeys(apprTurns) {
		fmt.Fprintf(b, "sandboxd_agent_approval_turns_total{agent=%q} %d\n", k, apprTurns[k])
	}

	fmt.Fprintf(b, "# HELP sandboxd_agent_approval_friction_total Turns needing more than %d approvals — treat as a policy bug, not engagement.\n"+
		"# TYPE sandboxd_agent_approval_friction_total counter\n", approvalFrictionThreshold)
	for _, k := range sortedKeys(friction) {
		fmt.Fprintf(b, "sandboxd_agent_approval_friction_total{agent=%q} %d\n", k, friction[k])
	}

	fmt.Fprintf(b, "# HELP sandboxd_agent_interrupts_total Turns stopped on request, by agent.\n"+
		"# TYPE sandboxd_agent_interrupts_total counter\n")
	for _, k := range sortedKeys(interrupts) {
		fmt.Fprintf(b, "sandboxd_agent_interrupts_total{agent=%q} %d\n", k, interrupts[k])
	}

	fmt.Fprintf(b, "# HELP sandboxd_agent_supervisor_unreachable_total Times the in-sandbox agent "+
		"supervisor did not answer, by agent. Distinct from a failed turn: this one is ours to fix.\n"+
		"# TYPE sandboxd_agent_supervisor_unreachable_total counter\n")
	for _, k := range sortedKeys(unreach) {
		fmt.Fprintf(b, "sandboxd_agent_supervisor_unreachable_total{agent=%q} %d\n", k, unreach[k])
	}

	fmt.Fprintf(b, "# HELP sandboxd_agent_spend_stops_total Turns refused because a spend cap was "+
		"reached, by agent.\n# TYPE sandboxd_agent_spend_stops_total counter\n")
	for _, k := range sortedKeys(stops) {
		fmt.Fprintf(b, "sandboxd_agent_spend_stops_total{agent=%q} %d\n", k, stops[k])
	}
}

// splitAgentKey undoes the "agent result" composite key.
func splitAgentKey(k string) (agent, result string) {
	if i := strings.LastIndex(k, " "); i > 0 {
		return k[:i], k[i+1:]
	}
	return k, ""
}

// bodyCapExempt reports whether a route carries file bytes rather than JSON
// and is bounded by its own limit instead of MAX_REQUEST_BYTES.
func bodyCapExempt(path string) bool {
	return strings.HasPrefix(path, "/v1/sandboxes/") && strings.HasSuffix(path, "/files")
}
