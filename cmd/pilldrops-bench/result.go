package main

// The committed benchmark result document written to docs/benchmarks/. The
// full sample series is stored in collection order, and every fact records
// whether it was measured or asserted.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// resultSchema is the document format version; bump it when a field changes meaning.
const resultSchema = "pilldrops.bench.latency/v1"

// percentileMethod is recorded in the document so a stored P95 can be re-derived.
const percentileMethod = "nearest-rank: index = ceil(p/100 * n) - 1, clamped to [0, n-1]"

// Sample is one create → ready → destroy cycle. Times are milliseconds.
type Sample struct {
	// Index is the position in collection order.
	Index int `json:"index"`
	// CreateMS is the create request to its success response.
	CreateMS float64 `json:"create_ms"`
	// ExecMS is the first successful exec call alone.
	ExecMS float64 `json:"exec_ms"`
	// ReadyMS is the create request to that exec returning.
	ReadyMS float64 `json:"ready_ms"`
	// ExecAttempts above one means the sandbox reported created before it was usable.
	ExecAttempts int `json:"exec_attempts"`
	// OK is false when the sandbox never became usable; such samples are excluded from the summary.
	OK  bool   `json:"ok"`
	Err string `json:"error,omitempty"`
	// SandboxID is kept so a failure can be correlated with node logs.
	SandboxID string `json:"sandbox_id,omitempty"`
}

// Stats is the derived summary for one measured quantity.
type Stats struct {
	N    int     `json:"n"`
	Min  float64 `json:"min"`
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
}

// RunnerInfo identifies the machine. Results are only comparable within a class.
type RunnerInfo struct {
	Class      string `json:"class"`
	NodeID     string `json:"node_id,omitempty"`
	Host       string `json:"host"`
	CPUModel   string `json:"cpu_model"`
	CPUCores   int    `json:"cpu_cores"`
	Kernel     string `json:"kernel"`
	MemTotalMB int    `json:"mem_total_mb"`
	// FactsSource says whether CPU/kernel came from the node's own /proc or the harness ran off-node.
	FactsSource string    `json:"facts_source"`
	SampledAt   time.Time `json:"sampled_at"`
}

// ConfigInfo is what was asked for.
type ConfigInfo struct {
	Runtime string `json:"runtime"`
	// RuntimeSource is "flag": the runtime is an operator assertion, not discovered via the API.
	RuntimeSource string  `json:"runtime_source"`
	Template      string  `json:"template"`
	CacheState    string  `json:"cache_state"`
	SamplesAsked  int     `json:"samples_asked"`
	ExecCmd       string  `json:"exec_cmd"`
	ReadyTimeoutS float64 `json:"ready_timeout_s"`
}

// Result is the whole document.
type Result struct {
	Schema           string `json:"schema"`
	PercentileMethod string `json:"percentile_method"`
	GitSHA           string `json:"git_sha"`
	// GitDirty is null when -commit supplied the SHA and there is no checkout to inspect.
	GitDirty *bool            `json:"git_dirty"`
	RunAt    time.Time        `json:"run_at"`
	Runner   RunnerInfo       `json:"runner"`
	Config   ConfigInfo       `json:"config"`
	Summary  map[string]Stats `json:"summary"`
	Failures int              `json:"failures"`
	// Samples is the full series in collection order, not sorted.
	Samples []Sample `json:"samples"`
}

// summarize derives Stats from the OK samples only; a failed sample is not a fast sample.
func summarize(samples []Sample, pick func(Sample) float64) Stats {
	xs := make([]float64, 0, len(samples))
	for _, s := range samples {
		if s.OK {
			xs = append(xs, pick(s))
		}
	}
	if len(xs) == 0 {
		return Stats{}
	}
	var sum float64
	for _, v := range xs {
		sum += v
	}
	return Stats{
		N:    len(xs),
		Min:  slices.Min(xs),
		P50:  percentile(xs, 50),
		P95:  percentile(xs, 95),
		P99:  percentile(xs, 99),
		Max:  slices.Max(xs),
		Mean: sum / float64(len(xs)),
	}
}

// gitSHA reports the commit under test and whether the tree was dirty.
// -commit wins because the node under test has no clone of this repository.
func gitSHA() (string, *bool) {
	if *commitSHA != "" {
		return *commitSHA, nil
	}
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return "unknown", nil
	}
	sha := strings.TrimSpace(string(out))
	st, err := exec.Command("git", "status", "--porcelain").Output()
	if err != nil {
		return sha, nil
	}
	dirty := len(strings.TrimSpace(string(st))) > 0
	return sha, &dirty
}

// localFacts reads CPU model and kernel from /proc. Off Linux the harness is
// not on the node under test, so the fields are left empty.
func localFacts() (cpuModel, kernel, source string) {
	if runtime.GOOS != "linux" {
		return "", "", "remote (harness not on the node; CPU/kernel not collected)"
	}
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "model name") {
				if i := strings.Index(line, ":"); i >= 0 {
					cpuModel = strings.TrimSpace(line[i+1:])
				}
				break
			}
		}
	}
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		kernel = strings.TrimSpace(string(b))
	}
	return cpuModel, kernel, "local (/proc on this machine)"
}

// write stores the document as <runtime>-<cache_state>.json in dir.
func (r *Result) write(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create result dir: %w", err)
	}
	name := fmt.Sprintf("%s-%s.json", r.Config.Runtime, r.Config.CacheState)
	path := filepath.Join(dir, name)
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal result: %w", err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return "", fmt.Errorf("write result: %w", err)
	}
	return path, nil
}
