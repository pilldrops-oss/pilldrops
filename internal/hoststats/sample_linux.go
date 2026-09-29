//go:build linux

package hoststats

import (
	"os"
	"runtime"
	"syscall"
	"time"
)

// Stats is a point-in-time host resource snapshot.
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

// Sample reads host CPU (over a 200ms delta), memory, load, and disk usage of dataDir.
// Unreadable sources leave their fields zero.
func Sample(dataDir string) Stats {
	var s Stats
	s.Cores = runtime.NumCPU()

	stat1, _ := os.ReadFile("/proc/stat")
	time.Sleep(200 * time.Millisecond)
	stat2, _ := os.ReadFile("/proc/stat")
	s.CPUPercent = cpuBusy(parseStat(stat1), parseStat(stat2))

	if mi, err := os.ReadFile("/proc/meminfo"); err == nil {
		s.MemTotalMB, s.MemAvailMB = parseMeminfo(mi)
		s.MemUsedMB = s.MemTotalMB - s.MemAvailMB
	}
	if la, err := os.ReadFile("/proc/loadavg"); err == nil {
		s.LoadAvg = parseLoadavg(la)
	}
	var fs syscall.Statfs_t
	if syscall.Statfs(dataDir, &fs) == nil {
		bs := float64(fs.Bsize)
		gb := 1024.0 * 1024 * 1024
		s.DiskTotalGB = float64(fs.Blocks) * bs / gb
		s.DiskFreeGB = float64(fs.Bavail) * bs / gb
		s.DiskUsedGB = s.DiskTotalGB - float64(fs.Bfree)*bs/gb
	}
	return s
}
