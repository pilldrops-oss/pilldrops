//go:build !linux

package hoststats

import (
	"os"
	"runtime"
	"strconv"
)

// Stats mirrors the Linux definition so callers compile everywhere.
type Stats struct {
	Cores       int        `json:"cpu_cores"`
	CPUPercent  float64    `json:"cpu_percent"`
	LoadAvg     [3]float64 `json:"load_avg"`
	MemTotalMB  int        `json:"mem_total_mb"`
	MemUsedMB   int        `json:"mem_used_mb"`
	MemAvailMB  int        `json:"mem_available_mb"`
	DiskTotalGB float64    `json:"disk_total_gb"`
	DiskUsedGB  float64    `json:"disk_used_gb"`
	DiskFreeGB  float64    `json:"disk_free_gb"`
}

// Sample returns only the core count off Linux. HOSTSTATS_MEM_TOTAL_MB fakes
// memory so the scheduler will place sandboxes on a local dev node; it is
// ignored on Linux where /proc is read.
func Sample(dataDir string) Stats {
	s := Stats{Cores: runtime.NumCPU()}
	if v, err := strconv.Atoi(os.Getenv("HOSTSTATS_MEM_TOTAL_MB")); err == nil && v > 0 {
		s.MemTotalMB = v
		s.MemAvailMB = v
	}
	return s
}
