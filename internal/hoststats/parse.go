package hoststats

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
)

// cpuTimes holds aggregate jiffies from /proc/stat's first line.
type cpuTimes struct{ total, idle uint64 }

// parseStat sums the "cpu " aggregate line; idle = idle + iowait.
func parseStat(b []byte) cpuTimes {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 6 || f[0] != "cpu" {
			continue
		}
		var t cpuTimes
		for i := 1; i < len(f); i++ {
			v, _ := strconv.ParseUint(f[i], 10, 64) //nolint:errcheck // malformed field contributes 0
			t.total += v
			if i == 4 || i == 5 { // idle, iowait
				t.idle += v
			}
		}
		return t
	}
	return cpuTimes{}
}

// cpuBusy returns the busy percentage between two /proc/stat samples.
func cpuBusy(prev, cur cpuTimes) float64 {
	dt := float64(cur.total - prev.total)
	di := float64(cur.idle - prev.idle)
	if dt <= 0 {
		return 0
	}
	return (dt - di) / dt * 100
}

// parseMeminfo returns MemTotal and MemAvailable in MB.
func parseMeminfo(b []byte) (totalMB, availMB int) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		kb, _ := strconv.Atoi(f[1]) //nolint:errcheck // malformed value leaves the stat at 0
		switch f[0] {
		case "MemTotal:":
			totalMB = kb / 1024
		case "MemAvailable:":
			availMB = kb / 1024
		}
	}
	return
}

// parseLoadavg returns the 1/5/15-minute load averages.
func parseLoadavg(b []byte) [3]float64 {
	f := strings.Fields(string(b))
	var out [3]float64
	for i := 0; i < 3 && i < len(f); i++ {
		out[i], _ = strconv.ParseFloat(f[i], 64) //nolint:errcheck // malformed field leaves that average at 0
	}
	return out
}
