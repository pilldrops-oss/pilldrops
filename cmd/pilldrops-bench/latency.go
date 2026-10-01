package main

// Latency mode measures how long one sandbox takes to become usable at rest.
// Samples run sequentially on purpose; concurrency would measure queueing.

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// execProbe decides a sandbox is usable; it must be trivial so the platform is measured, not the command.
const execProbe = "true"

// throwawayNote explains the discarded sample in the hot cache state.
const throwawayNote = "warm-up (discarded)"

// timedExec runs one command and reports success, duration and failure detail.
func (cl client) timedExec(id, cmd string) (ok bool, d time.Duration, detail string) {
	body, err := json.Marshal(map[string]any{"cmd": cmd, "timeout": 30})
	if err != nil {
		return false, 0, "marshal: " + err.Error()
	}
	t := time.Now()
	code, b := cl.req("POST", "/v1/sandboxes/"+id+"/exec", string(body))
	d = time.Since(t)
	if code < 200 || code >= 300 {
		return false, d, fmt.Sprintf("http %d: %s", code, truncate(string(b), 160))
	}
	var out struct {
		ExitCode *int   `json:"exit_code"`
		Stderr   string `json:"stderr"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return false, d, "decode: " + truncate(string(b), 160)
	}
	// A missing exit_code is not a zero exit code.
	if out.ExitCode == nil {
		return false, d, "response carried no exit_code: " + truncate(string(b), 160)
	}
	if *out.ExitCode != 0 {
		return false, d, fmt.Sprintf("exit %d: %s", *out.ExitCode, truncate(out.Stderr, 120))
	}
	return true, d, ""
}

// dropPageCache evicts the host page cache; needs root on the node under test.
func dropPageCache() error {
	if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3\n"), 0o200); err != nil {
		return fmt.Errorf("drop page cache (needs root, on the node): %w", err)
	}
	return nil
}

// beforeSample re-establishes the declared cache state ahead of each sample.
// Only "warm" needs it, since every create warms the page cache again.
func beforeSample(state string) error {
	if state == "warm" {
		return dropPageCache()
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// prepareCache establishes the declared cache state, or refuses rather than
// silently measuring a different one.
func prepareCache(cl client, tr *tracker, state string) error {
	switch state {
	case "hot":
		// One throwaway create so the template's pages are resident.
		id, code, _, errMsg := cl.create()
		if id == "" {
			return fmt.Errorf("%s: create failed (http %d) %s", throwawayNote, code, errMsg)
		}
		tr.put(id)
		cl.destroy(id)
		tr.del(id)
		return nil

	case "warm":
		// Consent is checked once here; the drop itself repeats before every sample.
		if !*allowDropCaches {
			return fmt.Errorf("cache state %q drops the host page cache before every sample, which affects every process on the box — re-run with -allow-drop-caches to consent", state)
		}
		return dropPageCache()

	case "cold":
		// Refused until nodes fetch templates from an artifact store; the error says why.
		return fmt.Errorf("cache state \"cold\" is refused on every runtime today, deliberately: " +
			"on firecracker there is no artifact store for a node to re-fetch a template from, so removing it is destructive rather than cold; " +
			"on docker an rmi+re-pull would measure the registry's network rather than this platform, and would not be comparable with the firecracker column. " +
			"Both become measurable together once a node fetches templates from an artifact store. Use -cache warm or -cache hot")

	default:
		return fmt.Errorf("unknown cache state %q (want cold|warm|hot)", state)
	}
}

// runLatency takes n sequential samples and writes one result document.
func runLatency(cl client, tr *tracker) {
	if *runnerClass == "" {
		fmt.Fprintln(os.Stderr, "-runner-class is required: results are only comparable within a class, so a document without one cannot be compared to anything")
		os.Exit(2)
	}
	if *runtimeName == "" {
		fmt.Fprintln(os.Stderr, "-runtime is required: the node's runtime is not discoverable through the API today (GET /v1/capabilities is Phase 1), so it must be asserted and is recorded as an assertion")
		os.Exit(2)
	}

	// Read before any sandbox exists, so a refused key exits with nothing to clean up.
	hs, err := cl.hostStats()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := prepareCache(cl, tr, *cacheState); err != nil {
		fmt.Fprintf(os.Stderr, "cache setup: %v\n", err)
		os.Exit(2)
	}

	cpuModel, kernel, factsSource := localFacts()
	cores, _ := hs["cpu_cores"].(float64)
	memTotal, _ := hs["mem_total_mb"].(float64)
	sha, dirty := gitSHA()

	res := &Result{
		Schema:           resultSchema,
		PercentileMethod: percentileMethod,
		GitSHA:           sha,
		GitDirty:         dirty,
		RunAt:            time.Now().UTC(),
		Runner: RunnerInfo{
			Class:       *runnerClass,
			NodeID:      *nodeID,
			Host:        *host,
			CPUModel:    cpuModel,
			CPUCores:    int(cores),
			Kernel:      kernel,
			MemTotalMB:  int(memTotal),
			FactsSource: factsSource,
			SampledAt:   time.Now().UTC(),
		},
		Config: ConfigInfo{
			Runtime:       *runtimeName,
			RuntimeSource: "flag",
			Template:      *template,
			CacheState:    *cacheState,
			SamplesAsked:  *total,
			ExecCmd:       execProbe,
			ReadyTimeoutS: readyTimeout.Seconds(),
		},
		Summary: map[string]Stats{},
	}

	fmt.Printf("latency: runtime=%s template=%s cache=%s n=%d class=%s\n",
		*runtimeName, *template, *cacheState, *total, *runnerClass)
	fmt.Printf("%-6s %-11s %-10s %-11s %-8s %s\n", "sample", "create_ms", "exec_ms", "ready_ms", "attempts", "note")

	for i := 0; i < *total; i++ {
		if err := beforeSample(*cacheState); err != nil {
			// The remaining samples would be taken in a cache state the document does not claim.
			fmt.Fprintf(os.Stderr, "sample %d: could not re-establish cache state %q: %v\n", i, *cacheState, err)
			os.Exit(1)
		}
		s := takeSample(cl, tr, i)
		res.Samples = append(res.Samples, s)
		if !s.OK {
			res.Failures++
			fmt.Printf("%-6d %-11s %-10s %-11s %-8d FAILED: %s\n", i, "-", "-", "-", s.ExecAttempts, s.Err)
			continue
		}
		fmt.Printf("%-6d %-11.0f %-10.0f %-11.0f %-8d\n", i, s.CreateMS, s.ExecMS, s.ReadyMS, s.ExecAttempts)
	}

	res.Summary["create_ms"] = summarize(res.Samples, func(s Sample) float64 { return s.CreateMS })
	res.Summary["exec_ms"] = summarize(res.Samples, func(s Sample) float64 { return s.ExecMS })
	res.Summary["ready_ms"] = summarize(res.Samples, func(s Sample) float64 { return s.ReadyMS })

	fmt.Println()
	for _, k := range []string{"create_ms", "exec_ms", "ready_ms"} {
		st := res.Summary[k]
		fmt.Printf("%-10s n=%-3d min=%-7.0f p50=%-7.0f p95=%-7.0f p99=%-7.0f max=%-7.0f mean=%.0f\n",
			k, st.N, st.Min, st.P50, st.P95, st.P99, st.Max, st.Mean)
	}
	if res.Failures > 0 {
		fmt.Printf("\n%d of %d samples FAILED and are excluded from the summary\n", res.Failures, *total)
	}

	dir := *outDir
	if dir == "" {
		dir = "docs/benchmarks/" + time.Now().UTC().Format("2006-01-02")
	}
	path, err := res.write(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "write result: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\nwrote %s\n", path)
}

// takeSample is one create, ready, destroy cycle. ready_ms is measured from
// the create request, since that is the interval a caller experiences.
func takeSample(cl client, tr *tracker, i int) Sample {
	s := Sample{Index: i}

	t0 := time.Now()
	id, code, createDur, errMsg := cl.create()
	if id == "" {
		s.Err = fmt.Sprintf("create failed (http %d) %s", code, errMsg)
		return s
	}
	tr.put(id)
	defer func() {
		cl.destroy(id)
		tr.del(id)
	}()

	s.SandboxID = id
	s.CreateMS = float64(createDur.Microseconds()) / 1000

	// A create that returns before the guest is usable shows up as attempts > 1.
	deadline := time.Now().Add(*readyTimeout)
	var lastErr string
	for {
		s.ExecAttempts++
		ok, execDur, detail := cl.timedExec(id, execProbe)
		if ok {
			s.ExecMS = float64(execDur.Microseconds()) / 1000
			s.ReadyMS = float64(time.Since(t0).Microseconds()) / 1000
			s.OK = true
			return s
		}
		lastErr = detail
		if time.Now().After(deadline) {
			// An unusable sandbox is a failure, not a slow sample.
			s.Err = fmt.Sprintf("never became usable within %s after %d attempts: %s",
				*readyTimeout, s.ExecAttempts, lastErr)
			return s
		}
		time.Sleep(250 * time.Millisecond)
	}
}
