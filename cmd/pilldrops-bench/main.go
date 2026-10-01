package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"sync"
	"syscall"
	"time"
)

var (
	host = flag.String("host", "http://localhost:18080", "server base URL")
	key  = flag.String("key", "", "API key (default $PILLDROPS_API_KEY)")
	// GET /v1/host is node-wide, so a tenant key is refused on it.
	operatorKey = flag.String("operator-key", "", "operator key for host stats (GET /v1/host) (default $PILLDROPS_OPERATOR_KEY, then -key)")
	template    = flag.String("template", "base", "template to create")
	mode        = flag.String("mode", "ramp", "ramp | churn | invoke | latency")
	step        = flag.Int("step", 5, "ramp: sandboxes added per stage")
	interval    = flag.Int("interval", 4, "ramp: seconds per stage")
	maxN        = flag.Int("max", 300, "ramp: hard stop count")
	memFloor    = flag.Int("mem-floor-mb", 2048, "ramp: stop if mem_available drops below")
	loadMax     = flag.Float64("load-max", 0, "ramp: stop if 1-min load average exceeds (0 = off)")
	start       = flag.String("start", "", "persistent workload command run in each sandbox (real load, not an idle pod)")
	memMB       = flag.Int("mem", 0, "memory_mb per sandbox (0 = template default); small values pack more per node")
	conc        = flag.Int("c", 10, "churn/invoke: concurrency")
	total       = flag.Int("n", 200, "churn/invoke: total requests; latency: samples to take")

	// latency mode. Runtime, runner class and commit have no safe default, so they are required.
	cacheState  = flag.String("cache", "hot", "latency: cold | warm | hot — the template cache state to establish before sampling")
	runtimeName = flag.String("runtime", "", "latency: the runtime under test (firecracker|docker|gvisor). Required — not discoverable via the API until GET /v1/capabilities exists, so it is recorded as an operator assertion")
	runnerClass = flag.String("runner-class", "", "latency: the machine class, e.g. kvm-bare-1. Required — results are only comparable within a class")
	nodeID      = flag.String("node-id", "", "latency: optional node identifier recorded in the result")
	outDir      = flag.String("out", "", "latency: result directory (default docs/benchmarks/<UTC date>)")
	commitSHA   = flag.String("commit", "", "latency: the commit under test. Detected from git when the harness runs in a checkout; required when it does not (the reference node has no Go toolchain and no clone), because a result that cannot name its commit cannot be compared to one that can")

	allowDropCaches = flag.Bool("allow-drop-caches", false, "latency: consent to dropping the host page cache for -cache warm (affects every process on the box; only meaningful when run on the node)")
	readyTimeout    = flag.Duration("ready-timeout", 60*time.Second, "latency: give up on a sandbox that never becomes usable, and record it as a failure rather than a slow sample")
	execCmd         = flag.String("exec", "", "churn: optional command per sandbox")
	fnName          = flag.String("fn", "", "invoke: function name to load-test")
	fnPath          = flag.String("path", "/api/hello", "invoke: handler path to hit")
	keep            = flag.Bool("keep", false, "do not destroy created sandboxes on exit")
)

type client struct{ c *http.Client }

func (cl client) req(method, path, body string) (int, []byte) {
	return cl.reqAs(*key, method, path, body)
}

func (cl client) reqAs(apiKey, method, path, body string) (int, []byte) {
	r, err := http.NewRequest(method, *host+path, bytes.NewBufferString(body))
	if err != nil {
		return 0, nil
	}
	r.Header.Set("Authorization", "Bearer "+apiKey)
	r.Header.Set("Content-Type", "application/json")
	resp, err := cl.c.Do(r)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()       //nolint:errcheck // best-effort; body already read below
	b, _ := io.ReadAll(resp.Body) //nolint:errcheck
	return resp.StatusCode, b
}

func (cl client) create() (string, int, time.Duration, string) {
	t := time.Now()
	body := map[string]any{"template": *template}
	if *start != "" {
		body["start"] = *start
	}
	if *memMB > 0 {
		body["memory_mb"] = *memMB
	}
	bj, _ := json.Marshal(body) //nolint:errcheck // body holds only marshalable scalars (string, int)
	code, b := cl.req("POST", "/v1/sandboxes", string(bj))
	d := time.Since(t)
	var out struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(b, &out) //nolint:errcheck
	return out.ID, code, d, out.Error
}

func (cl client) destroy(id string) time.Duration {
	t := time.Now()
	cl.req("DELETE", "/v1/sandboxes/"+id, "")
	return time.Since(t)
}

func (cl client) exec(id, cmd string) {
	body, _ := json.Marshal(map[string]any{"cmd": cmd, "timeout": 30}) //nolint:errcheck // literal map of scalars, cannot fail
	cl.req("POST", "/v1/sandboxes/"+id+"/exec", string(body))
}

func (cl client) hostStats() (map[string]any, error) {
	k := *operatorKey
	if k == "" {
		k = *key
	}
	code, b := cl.reqAs(k, "GET", "/v1/host", "")
	if code != http.StatusOK {
		return nil, fmt.Errorf("GET /v1/host answered %d: host stats need an operator key (-operator-key or PILLDROPS_OPERATOR_KEY)", code)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("GET /v1/host: %w", err)
	}
	return m, nil
}

// tracker records every created id so cleanup can sweep them.
type tracker struct {
	mu  sync.Mutex
	ids map[string]bool
}

func (t *tracker) put(id string) { t.mu.Lock(); t.ids[id] = true; t.mu.Unlock() }
func (t *tracker) del(id string) { t.mu.Lock(); delete(t.ids, id); t.mu.Unlock() }
func (t *tracker) all() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Collect(maps.Keys(t.ids))
}

// keysFromEnv fills the keys left unset on the command line from the
// environment. After flag.Parse rather than as flag defaults, so -help never
// prints a secret.
func keysFromEnv() {
	if *key == "" {
		*key = os.Getenv("PILLDROPS_API_KEY")
	}
	if *operatorKey == "" {
		*operatorKey = os.Getenv("PILLDROPS_OPERATOR_KEY")
	}
}

func main() {
	flag.Parse()
	keysFromEnv()
	if *key == "" {
		fmt.Fprintln(os.Stderr, "set -key or PILLDROPS_API_KEY")
		os.Exit(2)
	}
	cl := client{c: &http.Client{Timeout: 150 * time.Second}}
	tr := &tracker{ids: map[string]bool{}}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	cleanup := func() {
		if *keep {
			fmt.Printf("-keep set; leaving %d sandboxes\n", len(tr.all()))
			return
		}
		ids := tr.all()
		fmt.Printf("cleaning up %d sandboxes…\n", len(ids))
		var wg sync.WaitGroup
		for _, id := range ids {
			wg.Add(1)
			go func(id string) { defer wg.Done(); cl.destroy(id); tr.del(id) }(id)
		}
		wg.Wait()
		fmt.Println("cleanup done")
	}
	go func() { <-sig; cleanup(); os.Exit(130) }()

	switch *mode {
	case "ramp":
		runRamp(cl, tr)
	case "churn":
		runChurn(cl, tr)
	case "invoke":
		runInvoke(cl) // hits an existing function; nothing to clean up
	case "latency":
		runLatency(cl, tr)
	default:
		fmt.Fprintln(os.Stderr, "unknown -mode")
		os.Exit(2)
	}
	cleanup()
}

// runRamp adds step live sandboxes every interval seconds until a ceiling:
// create failure, mem floor, or -max.
func runRamp(cl client, tr *tracker) {
	// The mem-floor and load ceilings read host stats; without them the ramp has no ceiling but -max.
	if _, err := cl.hostStats(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Printf("%-4s %-6s %-6s %-14s %-9s %-9s %s\n", "live", "cpu%", "load", "mem_used/total", "disk_used", "crt_p95ms", "note")
	live := 0
	for live < *maxN {
		var lats []float64
		fail := ""
		// Stage creates run concurrently; serial creates make a large ramp take too long.
		n := *step
		if rem := *maxN - live; n > rem {
			n = rem
		}
		var mu sync.Mutex
		var wg sync.WaitGroup
		sem := make(chan struct{}, *conc)
		for i := 0; i < n; i++ {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				id, code, d, errMsg := cl.create()
				mu.Lock()
				defer mu.Unlock()
				if code == 201 && id != "" {
					tr.put(id)
					live++
					lats = append(lats, float64(d.Milliseconds()))
				} else if fail == "" {
					fail = fmt.Sprintf("create HTTP %d: %s", code, errMsg)
				}
			}()
		}
		wg.Wait()
		hs, err := cl.hostStats()
		if err != nil {
			fmt.Printf("\nstopped at %d concurrent — %v\n", live, err)
			return
		}
		avail, _ := hs["mem_available_mb"].(float64)
		memUsed, _ := hs["mem_used_mb"].(float64)
		memTot, _ := hs["mem_total_mb"].(float64)
		cpu, _ := hs["cpu_percent"].(float64)
		diskU, _ := hs["disk_used_gb"].(float64)
		load1 := 0.0
		if la, ok := hs["load_avg"].([]any); ok && len(la) > 0 {
			load1, _ = la[0].(float64)
		}
		fmt.Printf("%-4d %-6.1f %-6.1f %-14s %-9s %-9.0f %s\n",
			live, cpu, load1, fmt.Sprintf("%.0f/%.0f", memUsed, memTot),
			fmt.Sprintf("%.1fGB", diskU), percentile(lats, 95), fail)
		if fail != "" {
			fmt.Printf("\nCEILING: %d concurrent — %s\n", live, fail)
			return
		}
		if avail > 0 && int(avail) < *memFloor {
			fmt.Printf("\nCEILING: %d concurrent — mem_available %.0fMB < floor %dMB\n", live, avail, *memFloor)
			return
		}
		if *loadMax > 0 && load1 > *loadMax {
			fmt.Printf("\nCEILING: %d concurrent — load %.1f > max %.1f (CPU saturated)\n", live, load1, *loadMax)
			return
		}
		time.Sleep(time.Duration(*interval) * time.Second)
	}
	fmt.Printf("\nreached -max %d with no ceiling hit\n", *maxN)
}

// runInvoke hits a deployed function's handler at fixed concurrency and reports throughput and latency.
func runInvoke(cl client) {
	if *fnName == "" {
		fmt.Fprintln(os.Stderr, "invoke mode needs -fn <function>")
		os.Exit(2)
	}
	path := "/v1/functions/" + *fnName + "/invoke" + *fnPath
	var mu sync.Mutex
	var lats []float64
	ct := tally{}
	jobs := make(chan int, *total)
	for i := 0; i < *total; i++ {
		jobs <- i
	}
	close(jobs)
	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < *conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				t := time.Now()
				code, _ := cl.req("GET", path, "")
				d := time.Since(t)
				mu.Lock()
				ct.add(code)
				lats = append(lats, float64(d.Milliseconds()))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	fmt.Printf("\ninvoke %s  (c=%d, n=%d)\n", path, *conc, *total)
	fmt.Printf("throughput: %.1f req/sec   in %.1fs\n", float64(*total)/elapsed, elapsed)
	fmt.Printf("success:    %d/%d   status: %v\n", ct.ok, *total, ct.byStatus)
	fmt.Printf("latency ms: p50=%.0f p95=%.0f p99=%.0f max=%.0f\n",
		percentile(lats, 50), percentile(lats, 95), percentile(lats, 99), percentile(lats, 100))
}

// runChurn runs create, exec, destroy cycles at fixed concurrency for n total.
func runChurn(cl client, tr *tracker) {
	var mu sync.Mutex
	var crt, dst []float64
	ct := tally{}
	jobs := make(chan int, *total)
	for i := 0; i < *total; i++ {
		jobs <- i
	}
	close(jobs)
	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < *conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				id, code, d, _ := cl.create()
				mu.Lock()
				ct.add(code)
				crt = append(crt, float64(d.Milliseconds()))
				mu.Unlock()
				if code != 201 || id == "" {
					continue
				}
				tr.put(id)
				if *execCmd != "" {
					cl.exec(id, *execCmd)
				}
				dd := cl.destroy(id)
				tr.del(id)
				mu.Lock()
				dst = append(dst, float64(dd.Milliseconds()))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	fmt.Printf("\nchurn: %d cycles in %.1fs = %.1f creates/sec\n", *total, elapsed, float64(*total)/elapsed)
	fmt.Printf("success: %d/%d  errors: %v\n", ct.ok, *total, ct.byStatus)
	fmt.Printf("create  ms  p50=%.0f p95=%.0f p99=%.0f\n", percentile(crt, 50), percentile(crt, 95), percentile(crt, 99))
	fmt.Printf("destroy ms  p50=%.0f p95=%.0f p99=%.0f\n", percentile(dst, 50), percentile(dst, 95), percentile(dst, 99))
}
